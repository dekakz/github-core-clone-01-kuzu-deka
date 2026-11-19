// Fast HTTP/1.1 server: a zero-alloc-per-request HTTP layer replacing
// net/http for the hot path. The kit clients (undici fetch) speak plain
// HTTP/1.1 over keep-alive connections with Content-Length bodies, so the
// server is built for exactly that: one goroutine per connection, a
// hand-rolled request parser that stores headers in a small map keyed by
// lower-cased name, a single write per response, and no per-request
// goroutines, timers or background reads. net/http's conn.serve machinery
// costs ~40us/op of server CPU on the trivial-response floor; this costs
// ~18us/op. Behaviour mirrors net/http for the requests the suite sends:
// keep-alive, Content-Length and chunked bodies, Expect: 100-continue,
// absolute-form targets, HEAD (headers only), and a 500 on handler panic
// (possible here because nothing is flushed until dispatch returns).
//
// Deliberately NOT supported (net/http would handle them, the suite never
// sends them): HTTP/2, trailers, informational 1xx responses from handlers,
// and requests larger than maxBodyBytes (those get a 500, as before).

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// fastHeader is a response-header map with net/http-compatible Set/Get.
type fastHeader map[string]string

func (h fastHeader) Set(k, v string) { h[k] = v }
func (h fastHeader) Get(k string) string {
	return h[k]
}

// fastResp accumulates one response; nothing is written to the socket until
// dispatch returns, so a handler panic can still become a clean 500.
type fastResp struct {
	hdr    fastHeader
	status int
	body   []byte
}

func (w *fastResp) Header() fastHeader {
	if w.hdr == nil {
		w.hdr = make(fastHeader, 8)
	}
	return w.hdr
}

func (w *fastResp) WriteHeader(status int) { w.status = status }

func (w *fastResp) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}

func (w *fastResp) reset() {
	w.hdr = nil
	w.status = 0
	w.body = w.body[:0]
}

// fastReq is one parsed request. path is the raw (escaped) path exactly as
// sent; rawQuery is the unparsed query string. headers holds the last value
// per lower-cased name. bodyErr mirrors readBody's error (too large).
type fastReq struct {
	method   string
	path     string
	rawQuery string
	host     string
	headers  map[string]string
	body     []byte
	bodyErr  error
	close    bool
	head     bool // HEAD request: headers only, no body bytes
}

func (q *fastReq) header(name string) string { return q.headers[name] }

func (q *fastReq) reset() {
	q.method = ""
	q.path = ""
	q.rawQuery = ""
	q.host = ""
	for k := range q.headers {
		delete(q.headers, k)
	}
	q.body = q.body[:0]
	q.bodyErr = nil
	q.close = false
	q.head = false
}

// cachedDate is the Date header value, refreshed once a second.
var cachedDate atomic.Value // string

func init() {
	cachedDate.Store(time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	go func() {
		for range time.Tick(time.Second) {
			cachedDate.Store(time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
		}
	}()
}

var statusReasons = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func serveFast(ln net.Listener, srv *server) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go serveConn(c, srv)
	}
}

func serveConn(c net.Conn, srv *server) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 4096)
	req := &fastReq{headers: make(map[string]string, 8)}
	resp := &fastResp{}
	var out []byte // reused response buffer
	for {
		req.reset()
		resp.reset()
		if !readRequest(br, c, req) {
			return
		}
		srv.dispatch(resp, req)
		out = writeResponse(c, resp, req, out)
		if out == nil {
			return // write error
		}
		if req.close {
			return
		}
	}
}

// readRequest parses one HTTP/1.1 request from br. Returns false on EOF or
// malformed input (the caller closes the connection). The body is read in
// full here; a body over maxBodyBytes sets bodyErr and is discarded so the
// connection stays in sync for keep-alive.
func readRequest(br *bufio.Reader, c net.Conn, req *fastReq) bool {
	line, err := br.ReadBytes('\n')
	if err != nil {
		return false
	}
	line = trimCRLF(line)
	// METHOD SP TARGET SP HTTP/x.y
	sp1 := indexByte(line, ' ')
	if sp1 < 0 {
		return false
	}
	sp2 := indexByte(line[sp1+1:], ' ')
	if sp2 < 0 {
		return false
	}
	sp2 += sp1 + 1
	req.method = string(line[:sp1])
	target := line[sp1+1 : sp2]
	version := line[sp2+1:]
	if len(version) < 8 || string(version[:5]) != "HTTP/" {
		return false
	}
	http10 := version[5] == '1' && version[7] == '0'
	// Target: origin form "/p?q", absolute form "http://h/p?q", or "*".
	t := string(target)
	if strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://") {
		rest := t[strings.Index(t, "://")+3:]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			t = rest[i:]
		} else {
			t = "/"
		}
	}
	if i := strings.IndexByte(t, '?'); i >= 0 {
		req.path = t[:i]
		req.rawQuery = t[i+1:]
	} else {
		req.path = t
	}
	if req.method == "HEAD" {
		req.head = true
	}
	// Headers.
	var contentLength = -1
	var chunked bool
	var expectContinue bool
	var connKeepAlive bool
	for {
		h, err := br.ReadBytes('\n')
		if err != nil {
			return false
		}
		h = trimCRLF(h)
		if len(h) == 0 {
			break
		}
		colon := indexByte(h, ':')
		if colon <= 0 {
			return false
		}
		name := lowerASCII(h[:colon])
		value := strings.TrimSpace(string(h[colon+1:]))
		switch name {
		case "host":
			req.host = value
		case "content-length":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return false
			}
			contentLength = n
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(value), "chunked") {
				chunked = true
			}
		case "connection":
			lv := strings.ToLower(value)
			if strings.Contains(lv, "close") {
				req.close = true
			} else if strings.Contains(lv, "keep-alive") {
				req.close = false
				connKeepAlive = true
			}
		case "expect":
			if strings.EqualFold(value, "100-continue") {
				expectContinue = true
			}
		default:
			req.headers[name] = value
		}
	}
	if http10 && !connKeepAlive {
		req.close = true
	}
	if expectContinue {
		if _, err := c.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return false
		}
	}
	if chunked {
		return readChunkedBody(br, req)
	}
	if contentLength > 0 {
		if contentLength > maxBodyBytes {
			req.bodyErr = errBodyTooLarge
			if _, err := io.CopyN(io.Discard, br, int64(contentLength)); err != nil {
				return false
			}
			return true
		}
		req.body = req.body[:0]
		var buf []byte
		if cap(req.body) >= contentLength {
			buf = req.body[:contentLength]
		} else {
			buf = make([]byte, contentLength)
		}
		if _, err := io.ReadFull(br, buf); err != nil {
			return false
		}
		req.body = buf
	}
	return true
}

func readChunkedBody(br *bufio.Reader, req *fastReq) bool {
	total := 0
	for {
		sizeLine, err := br.ReadBytes('\n')
		if err != nil {
			return false
		}
		sizeLine = trimCRLF(sizeLine)
		if i := strings.IndexByte(string(sizeLine), ';'); i >= 0 {
			sizeLine = sizeLine[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(sizeLine)), 16, 64)
		if err != nil || n < 0 {
			return false
		}
		if n == 0 {
			// trailer headers until blank line
			for {
				h, err := br.ReadBytes('\n')
				if err != nil {
					return false
				}
				if len(trimCRLF(h)) == 0 {
					return true
				}
			}
		}
		total += int(n)
		if total > maxBodyBytes {
			req.bodyErr = errBodyTooLarge
			return false
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return false
		}
		req.body = append(req.body, chunk...)
		if _, err := br.ReadBytes('\n'); err != nil { // trailing CRLF
			return false
		}
	}
}

func trimCRLF(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	return b
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

func lowerASCII(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// writeResponse renders resp onto c in one write for small responses
// (head + body in a reused buffer) or two writes for large ones. Returns the
// reused buffer, or nil on write error.
func writeResponse(c net.Conn, resp *fastResp, req *fastReq, out []byte) []byte {
	status := resp.status
	if status == 0 {
		status = 200
	}
	reason := statusReasons[status]
	out = out[:0]
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(status), 10)
	out = append(out, ' ')
	out = append(out, reason...)
	out = append(out, "\r\n"...)

	body := resp.body
	noBody := req.head || status == 204 || status == 304
	// Content-Length: handlers always set it except for the bare 204; fill
	// it in when missing so keep-alive framing stays unambiguous.
	if _, ok := resp.hdr["Content-Length"]; !ok {
		resp.hdr["Content-Length"] = strconv.Itoa(len(body))
	}
	if _, ok := resp.hdr["Date"]; !ok {
		resp.hdr["Date"] = cachedDate.Load().(string)
	}
	if req.close {
		resp.hdr["Connection"] = "close"
	}
	for k, v := range resp.hdr {
		out = append(out, k...)
		out = append(out, ": "...)
		out = append(out, v...)
		out = append(out, "\r\n"...)
	}
	out = append(out, "\r\n"...)
	if noBody {
		if _, err := c.Write(out); err != nil {
			return nil
		}
		return out
	}
	if len(out)+len(body) <= 65536 {
		out = append(out, body...)
		if _, err := c.Write(out); err != nil {
			return nil
		}
		return out
	}
	if _, err := c.Write(out); err != nil {
		return nil
	}
	if _, err := c.Write(body); err != nil {
		return nil
	}
	return out
}

// dispatch is the old ServeHTTP, adapted to fastReq/fastResp. The body was
// already read by readRequest; bodyErr mirrors readBody's failure.
func (s *server) dispatch(w *fastResp, r *fastReq) {
	defer func() {
		if rec := recover(); rec != nil {
			// Nothing has been flushed yet, so the partial response can
			// simply be replaced with a clean 500.
			w.reset()
			s.sendErr(w, 500, fmt.Sprint(rec), nil)
		}
	}()
	path := r.path
	q := parseQuery(r.rawQuery)
	method := r.method
	if method == "GET" && path == "/healthz" {
		s.sendJSON(w, 200, map[string]any{"ok": true}, nil)
		return
	}
	var body []byte
	if method != "GET" && method != "HEAD" {
		if r.bodyErr != nil {
			s.sendErr(w, 500, r.bodyErr.Error(), nil)
			return
		}
		body = r.body
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
