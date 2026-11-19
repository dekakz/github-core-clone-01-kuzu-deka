// Forge entrypoint: bind an ephemeral port (or FORGE_PORT), print it, serve.

package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			if !isHeaderSent(w) {
				s.sendErr(w, 500, fmt.Sprint(rec), nil)
			}
		}
	}()
	path := r.URL.EscapedPath()
	if path == "" {
		path = r.URL.Path
	}
	q := parseQuery(r.URL.RawQuery)
	method := r.Method
	if method == "GET" && path == "/healthz" {
		s.sendJSON(w, 200, map[string]any{"ok": true}, nil)
		return
	}
	var body []byte
	if method != "GET" && method != "HEAD" {
		b, err := readBody(r)
		if err != nil {
			s.sendErr(w, 500, err.Error(), nil)
			return
		}
		body = b
	} else {
		body = []byte{}
	}
	if path == "/api/graphql" && method == "POST" {
		s.handleGraphql(w, r, body)
		return
	}
	if path == "/mcp" {
		s.handleMcp(w, r, body)
		return
	}
	if strings.HasPrefix(path, "/api/v3") {
		s.handleRest(w, r, path, q, body)
		return
	}
	if s.handleGit(w, r, path, q, body) {
		return
	}
	s.sendErr(w, 404, "Not Found", nil)
}

func isHeaderSent(w http.ResponseWriter) bool {
	// net/http exposes no direct check; a 500 after headers were sent is
	// already handled by the caller's headersSent guard in Node. Here the
	// recover path only runs when a handler panicked before writing, in
	// practice; writing again is safe because WriteHeader is idempotent
	// for the first call only — instead, attempt the write and let the
	// server log a superfluous-WriteHeader if it was already sent.
	return false
}

// sendJSONBare mirrors the git endpoints' bare 400s: JSON, no request id,
// no trailing newline.
func (s *server) sendJSONBare(w http.ResponseWriter, status int, body any) {
	b := marshalJSON(body)
	h := w.Header()
	h.Set("Content-Type", jsonType)
	h.Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (s *server) handleGit(w http.ResponseWriter, r *http.Request, path string, q *queryParams, body []byte) bool {
	segs := strings.Split(path, "/")
	if len(segs) < 3 || segs[0] != "" || segs[1] == "" || segs[2] == "" {
		return false
	}
	if !strings.HasSuffix(segs[2], ".git") {
		return false
	}
	owner := decodePath(segs[1])
	repoName := decodePath(segs[2][:len(segs[2])-4])
	if repoName == "" {
		return false
	}
	rest := ""
	if len(segs) > 3 {
		rest = "/" + strings.Join(segs[3:], "/")
	}
	method := r.Method

	if method == "GET" && rest == "/info/refs" {
		service := q.get("service")
		s.st.mu.RLock()
		rp := s.st.getRepo(owner, repoName)
		s.st.mu.RUnlock()
		if rp == nil {
			s.sendJSON(w, 404, map[string]any{"message": "Not Found"}, nil)
			return true
		}
		if service == "git-upload-pack" {
			isV2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
			s.st.mu.RLock()
			var payload []byte
			if isV2 {
				payload = v2Advertisement()
			} else {
				payload = s.st.v0Advertisement(rp, "upload")
			}
			s.st.mu.RUnlock()
			s.sendBytes(w, 200, "application/x-git-upload-pack-advertisement", payload, nil, false)
			return true
		}
		if service == "git-receive-pack" {
			s.st.mu.RLock()
			payload := s.st.v0Advertisement(rp, "receive")
			s.st.mu.RUnlock()
			s.sendBytes(w, 200, "application/x-git-receive-pack-advertisement", payload, nil, false)
			return true
		}
		s.sendJSONBare(w, 400, map[string]any{"message": "Bad Request"})
		return true
	}

	if method == "POST" && rest == "/git-upload-pack" {
		s.st.mu.RLock()
		rp := s.st.getRepo(owner, repoName)
		s.st.mu.RUnlock()
		if rp == nil {
			s.sendJSON(w, 404, map[string]any{"message": "Not Found"}, nil)
			return true
		}
		isV2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
		s.st.mu.RLock()
		result, err := s.st.handleUploadPack(rp, body, isV2)
		s.st.mu.RUnlock()
		if err != nil {
			s.sendJSONBare(w, 400, map[string]any{"message": "Bad Request"})
			return true
		}
		s.sendBytes(w, 200, "application/x-git-upload-pack-result", result, nil, false)
		return true
	}

	if method == "POST" && rest == "/git-receive-pack" {
		s.st.mu.RLock()
		rp := s.st.getRepo(owner, repoName)
		s.st.mu.RUnlock()
		if rp == nil {
			s.sendJSON(w, 404, map[string]any{"message": "Not Found"}, nil)
			return true
		}
		s.st.mu.Lock()
		result, err := s.st.handleReceivePack(rp, body)
		s.st.mu.Unlock()
		if err != nil {
			s.sendJSONBare(w, 400, map[string]any{"message": "Bad Request"})
			return true
		}
		s.sendBytes(w, 200, "application/x-git-receive-pack-result", result, nil, false)
		return true
	}
	return false
}

func main() {
	initClock(os.Getenv("MAKEFUN_TRIAL_NOW"))
	token = os.Getenv("MAKEFUN_SHOP_TOKEN")
	st := newStore()
	srv := &server{st: st}
	addr := ":0"
	if p := os.Getenv("FORGE_PORT"); p != "" {
		addr = ":" + p
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[forge] listening on port %d\n", port)
	if err := http.Serve(ln, srv); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
		os.Exit(1)
	}
}
