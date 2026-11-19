// Forge entrypoint: bind an ephemeral port (or FORGE_PORT), print it, serve.

package main

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
)

// sendJSONBare mirrors the git endpoints' bare 400s: JSON, no request id,
// no trailing newline.
func (s *server) sendJSONBare(w *fastResp, status int, body any) {
	b := marshalJSON(body)
	h := w.Header()
	h.Set("Content-Type", jsonType)
	h.Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (s *server) handleGit(w *fastResp, r *fastReq, path string, q *queryParams, body []byte) bool {
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
	method := r.method

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
			isV2 := strings.Contains(r.header("git-protocol"), "version=2")
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
		isV2 := strings.Contains(r.header("git-protocol"), "version=2")
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
	// The trial pod allows 2 CPUs: cap the scheduler at 2 so the GC and the
	// netpoller do not spawn dozens of threads fighting for 2 CPUs (Go 1.23
	// reads the host CPU count, not the cgroup limit). A high GC percent
	// keeps collection rare: the live set is tens of MiB under a 1 GiB cap.
	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(1000)
	initClock(os.Getenv("MAKEFUN_TRIAL_NOW"))
	token = os.Getenv("MAKEFUN_SHOP_TOKEN")
	if token != "" {
		bearerToken = "Bearer " + token
	}
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
	if err := serveFast(ln, srv); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
		os.Exit(1)
	}
}
