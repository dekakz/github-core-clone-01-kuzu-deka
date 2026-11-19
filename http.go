// HTTP plumbing: routing, JSON responses, body reading, pagination.

package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

const jsonType = "application/json; charset=utf-8"
const maxBodyBytes = 64 * 1024 * 1024

type server struct {
	st *store
	// gen counts completed non-GET dispatches: every mutation goes through
	// one, so a memoized GET is valid only while its generation matches.
	gen atomic.Uint64
	// memo caches whole GET responses (status, content type, extra headers,
	// body) keyed by method+path+query+host+git-protocol; see dispatch.
	// The X-Github-Request-Id header is never memoized: it is regenerated
	// per request, so a memo hit is observably identical to a fresh render.
	memoMu sync.Mutex
	memo   map[string]memoEntry
}

type memoEntry struct {
	gen    uint64
	status int
	ctype  string
	extra  map[string]string
	body   []byte
}

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// appendJSONString appends s as a JSON string, byte-compatible with
// encoding/json with SetEscapeHTML(false): only ", \, control chars and
// U+2028/U+2029 are escaped; <, > and & stay raw; invalid UTF-8 becomes \ufffd.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				const hexd = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', hexd[c>>4], hexd[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			dst = append(dst, '\\', 'u', 'f', 'f', 'f', 'd')
			i++
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			dst = append(dst, '\\', 'u', '2', '0', '2', '8')
			if r == '\u2029' {
				dst[len(dst)-1] = '9'
			}
			i += size
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

// appendIssueJSON renders an issue exactly as encoding/json would render the
// issueJSON struct (same field order, same values), without reflection.
func appendIssueJSON(dst []byte, r *repo, i *issue) []byte {
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendInt(dst, i.id, 10)
	dst = append(dst, `,"node_id":`...)
	dst = appendJSONString(dst, i.nodeID)
	dst = append(dst, `,"number":`...)
	dst = strconv.AppendInt(dst, int64(i.number), 10)
	dst = append(dst, `,"title":`...)
	dst = appendJSONString(dst, i.title)
	dst = append(dst, `,"body":`...)
	dst = appendJSONString(dst, i.body)
	dst = append(dst, `,"state":`...)
	dst = appendJSONString(dst, i.state)
	dst = append(dst, `,"user":{"login":`...)
	dst = appendJSONString(dst, viewerLogin)
	dst = append(dst, `,"id":`...)
	dst = strconv.AppendInt(dst, viewerID, 10)
	dst = append(dst, `,"type":"User"},"labels":[`...)
	for idx, n := range i.labels {
		if idx > 0 {
			dst = append(dst, ',')
		}
		color := "ededed"
		if l, ok := r.labels[n]; ok {
			color = l.color
		}
		dst = append(dst, `{"name":`...)
		dst = appendJSONString(dst, n)
		dst = append(dst, `,"color":`...)
		dst = appendJSONString(dst, color)
		dst = append(dst, '}')
	}
	dst = append(dst, `],"assignee":null,"assignees":[],"milestone":null,"locked":false,"comments":`...)
	dst = strconv.AppendInt(dst, int64(len(i.comments)), 10)
	dst = append(dst, `,"created_at":`...)
	dst = appendJSONString(dst, i.createdAt)
	dst = append(dst, `,"updated_at":`...)
	dst = appendJSONString(dst, i.updatedAt)
	dst = append(dst, `,"closed_at":`...)
	if i.state == "closed" {
		dst = appendJSONString(dst, i.updatedAt)
	} else {
		dst = append(dst, "null"...)
	}
	dst = append(dst, `,"author_association":"OWNER","html_url":"","url":"","repository_url":"","labels_url":"","comments_url":""}`...)
	return dst
}

// marshalJSON renders v as JSON without HTML escaping, no trailing newline.
func marshalJSON(v any) []byte {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	out := make([]byte, len(b))
	copy(out, b)
	bufPool.Put(buf)
	return out
}

func setHeader(h fastHeader, k, v string) {
	// Direct map assignment: Header.Set would re-canonicalize the key.
	h[k] = v
}

func (s *server) sendJSON(w *fastResp, status int, body any, extra map[string]string) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
	h := w.Header()
	setHeader(h, "Content-Type", jsonType)
	setHeader(h, "Content-Length", strconv.Itoa(buf.Len()))
	setHeader(h, "X-Github-Request-Id", nextReqID())
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
	bufPool.Put(buf)
}

// sendJSONBytes writes a pre-encoded JSON body: no encode, no copy.
func (s *server) sendJSONBytes(w *fastResp, status int, b []byte, extra map[string]string) {
	h := w.Header()
	setHeader(h, "Content-Type", jsonType)
	setHeader(h, "Content-Length", strconv.Itoa(len(b)))
	setHeader(h, "X-Github-Request-Id", nextReqID())
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (s *server) sendErr(w *fastResp, status int, message string, extra map[string]string) {
	s.sendJSON(w, status, map[string]any{"message": message}, extra)
}

func (s *server) sendBytes(w *fastResp, status int, contentType string, body []byte, extra map[string]string, withReqID bool) {
	h := w.Header()
	setHeader(h, "Content-Type", contentType)
	setHeader(h, "Content-Length", strconv.Itoa(len(body)))
	if withReqID {
		setHeader(h, "X-Github-Request-Id", nextReqID())
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

var errBodyTooLarge = &bodyTooLargeError{}

type bodyTooLargeError struct{}

func (e *bodyTooLargeError) Error() string { return "body too large" }

func isAuthed(r *fastReq) bool {
	if bearerToken == "" {
		return true
	}
	return r.header("authorization") == bearerToken
}

func decodePath(p string) string {
	if d, err := url.PathUnescape(p); err == nil {
		return d
	}
	return p
}

// splitPath splits a request path into decoded segments, dropping empties.
// Segments without '%' are returned as-is: PathUnescape would only copy them.
func splitPath(path string) []string {
	raw := strings.Split(path, "/")
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		if seg == "" {
			continue
		}
		if strings.Contains(seg, "%") {
			seg = decodePath(seg)
		}
		out = append(out, seg)
	}
	return out
}

// ------------------------------------------------------------------ query params (ordered, like URLSearchParams)

type kv struct {
	k, v string
}

type queryParams struct {
	items []kv
}

func parseQuery(raw string) *queryParams {
	q := &queryParams{}
	if raw == "" {
		return q
	}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v := part, ""
		if i := strings.IndexByte(part, '='); i >= 0 {
			k, v = part[:i], part[i+1:]
		}
		dk, _ := url.QueryUnescape(k)
		dv, _ := url.QueryUnescape(v)
		q.items = append(q.items, kv{dk, dv})
	}
	return q
}

func (q *queryParams) get(name string) string {
	for _, it := range q.items {
		if it.k == name {
			return it.v
		}
	}
	return ""
}

func (q *queryParams) has(name string) bool {
	for _, it := range q.items {
		if it.k == name {
			return true
		}
	}
	return false
}

// set mirrors URLSearchParams.set: replace the first occurrence's value and
// drop the rest, or append when absent.
func (q *queryParams) set(name, value string) {
	found := -1
	out := q.items[:0]
	for i, it := range q.items {
		if it.k == name {
			if found < 0 {
				found = i
				out = append(out, kv{name, value})
			}
			continue
		}
		out = append(out, it)
	}
	if found < 0 {
		out = append(out, kv{name, value})
	}
	q.items = out
}

func formEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '*' || c == '-' || c == '.' || c == '_':
			b.WriteByte(c)
		default:
			const hexd = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&0xf])
		}
	}
	return b.String()
}

func (q *queryParams) encode() string {
	var parts []string
	for _, it := range q.items {
		parts = append(parts, formEscape(it.k)+"="+formEscape(it.v))
	}
	return strings.Join(parts, "&")
}

func (q *queryParams) clone() *queryParams {
	out := &queryParams{items: make([]kv, len(q.items))}
	copy(out.items, q.items)
	return out
}

// ------------------------------------------------------------------ JSON body / JS value helpers

// extractIssueFields parses a POST /issues body of the form
// {"title": "...", "body": "...", "labels": ["a", {"name": "b"}]}
// without reflection. Anything else (unknown keys, non-string values,
// malformed JSON) returns ok=false and the caller falls back to jsonBody,
// so behaviour is identical to the generic path for exotic requests.
func extractIssueFields(body []byte) (title, bodyText string, labels []string, ok bool) {
	p := &jsonScanner{b: body}
	p.ws()
	if !p.byte('{') {
		return "", "", nil, false
	}
	p.ws()
	if p.byte('}') {
		p.ws()
		return "", "", nil, p.eof()
	}
	for {
		p.ws()
		key, good := p.str()
		if !good {
			return "", "", nil, false
		}
		p.ws()
		if !p.byte(':') {
			return "", "", nil, false
		}
		p.ws()
		switch key {
		case "title", "body":
			v, good := p.value()
			if !good {
				return "", "", nil, false
			}
			if key == "title" {
				title = v
			} else {
				bodyText = v
			}
		case "labels":
			ls, good := p.labels()
			if !good {
				return "", "", nil, false
			}
			labels = ls
		default:
			return "", "", nil, false
		}
		p.ws()
		if p.byte(',') {
			continue
		}
		if p.byte('}') {
			p.ws()
			return title, bodyText, labels, p.eof()
		}
		return "", "", nil, false
	}
}

type jsonScanner struct {
	b []byte
	i int
}

func (p *jsonScanner) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jsonScanner) eof() bool { return p.i == len(p.b) }

func (p *jsonScanner) byte(c byte) bool {
	if p.i < len(p.b) && p.b[p.i] == c {
		p.i++
		return true
	}
	return false
}

// str parses a JSON string starting at the current position.
func (p *jsonScanner) str() (string, bool) {
	if p.i >= len(p.b) || p.b[p.i] != '"' {
		return "", false
	}
	p.i++
	start := p.i
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			s := string(p.b[start:p.i])
			p.i++
			return s, true
		}
		if c == '\\' {
			return p.strEscaped(start)
		}
		if c < 0x20 {
			return "", false
		}
		p.i++
	}
	return "", false
}

func (p *jsonScanner) strEscaped(start int) (string, bool) {
	var sb strings.Builder
	sb.Write(p.b[start : p.i-1])
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			p.i++
			return sb.String(), true
		}
		if c != '\\' {
			if c < 0x20 {
				return "", false
			}
			sb.WriteByte(c)
			p.i++
			continue
		}
		p.i++
		if p.i >= len(p.b) {
			return "", false
		}
		switch e := p.b[p.i]; e {
		case '"':
			sb.WriteByte('"')
		case '\\':
			sb.WriteByte('\\')
		case '/':
			sb.WriteByte('/')
		case 'b':
			sb.WriteByte('\b')
		case 'f':
			sb.WriteByte('\f')
		case 'n':
			sb.WriteByte('\n')
		case 'r':
			sb.WriteByte('\r')
		case 't':
			sb.WriteByte('\t')
		case 'u':
			r, good := p.hex4()
			if !good {
				return "", false
			}
			if r >= 0xD800 && r <= 0xDBFF {
				if p.i+1 < len(p.b) && p.b[p.i] == '\\' && p.b[p.i+1] == 'u' {
					save := p.i
					p.i += 2
					r2, good2 := p.hex4()
					if good2 && r2 >= 0xDC00 && r2 <= 0xDFFF {
						r = 0x10000 + (r-0xD800)<<10 + (r2 - 0xDC00)
					} else {
						p.i = save
						sb.WriteRune(0xFFFD)
						continue
					}
				} else {
					sb.WriteRune(0xFFFD)
					continue
				}
			}
			sb.WriteRune(rune(r))
		default:
			return "", false
		}
		p.i++
	}
	return "", false
}

func (p *jsonScanner) hex4() (int, bool) {
	if p.i+4 > len(p.b) {
		return 0, false
	}
	n := 0
	for k := 0; k < 4; k++ {
		c := p.b[p.i+k]
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n += int(c - '0')
		case c >= 'a' && c <= 'f':
			n += int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n += int(c-'A') + 10
		default:
			return 0, false
		}
	}
	p.i += 4
	return n, true
}

// value parses a JSON string or null (the only shapes title/body take here).
func (p *jsonScanner) value() (string, bool) {
	if p.i+4 <= len(p.b) && string(p.b[p.i:p.i+4]) == "null" {
		p.i += 4
		return "", true
	}
	return p.str()
}

// labels parses a JSON array of strings or {"name": "..."} objects, or null.
func (p *jsonScanner) labels() ([]string, bool) {
	if p.i+4 <= len(p.b) && string(p.b[p.i:p.i+4]) == "null" {
		p.i += 4
		return nil, true
	}
	if !p.byte('[') {
		return nil, false
	}
	var out []string
	p.ws()
	if p.byte(']') {
		return out, true
	}
	for {
		p.ws()
		if p.i < len(p.b) && p.b[p.i] == '"' {
			s, good := p.str()
			if !good {
				return nil, false
			}
			out = append(out, s)
		} else if p.i < len(p.b) && p.b[p.i] == '{' {
			p.i++
			p.ws()
			name, good := p.str()
			if !good || name != "name" {
				return nil, false
			}
			p.ws()
			if !p.byte(':') {
				return nil, false
			}
			p.ws()
			s, good := p.str()
			if !good {
				return nil, false
			}
			out = append(out, s)
			p.ws()
			if !p.byte('}') {
				return nil, false
			}
		} else {
			return nil, false
		}
		p.ws()
		if p.byte(',') {
			continue
		}
		if p.byte(']') {
			return out, true
		}
		return nil, false
	}
}

// jsonBody mirrors the Node jsonBody helper: GET (or an empty DELETE body)
// yields {}, a malformed body fails, a non-object JSON value yields {}.
func jsonBody(method string, body []byte) (map[string]any, bool) {
	if method == "GET" || (method == "DELETE" && len(body) == 0) {
		return map[string]any{}, true
	}
	if len(body) == 0 {
		return map[string]any{}, true
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	return map[string]any{}, true
}

func parseJSONBody(body []byte) (any, bool) {
	if len(body) == 0 {
		return map[string]any{}, true
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, false
	}
	return v, true
}

// jsStr mirrors String(v ?? '') for the common JSON value kinds.
func jsStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return jsNumStr(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// jsStrDef mirrors String(v ?? def).
func jsStrDef(v any, def string) string {
	if v == nil {
		return def
	}
	return jsStr(v)
}

func jsNumStr(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 9e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// jsNumber mirrors Number(x): NaN when unparseable.
func jsNumber(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return math.NaN()
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	// Number('0x10') is 16; Go's ParseFloat rejects bare hex.
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		if n, err := strconv.ParseInt(s[2:], 16, 64); err == nil {
			return float64(n)
		}
	}
	if s == "Infinity" || s == "+Infinity" {
		return math.Inf(1)
	}
	if s == "-Infinity" {
		return math.Inf(-1)
	}
	return math.NaN()
}

func jsInt(s string) (int, bool) {
	f := jsNumber(s)
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// ------------------------------------------------------------------ pagination

func paginate[T any](items []T, q *queryParams, host, basePath string) ([]T, string) {
	perPageF := jsNumber(q.get("per_page"))
	if math.IsNaN(perPageF) || perPageF == 0 {
		perPageF = 30
	}
	perPage := math.Max(1, math.Min(100, perPageF))
	pageNumF := jsNumber(q.get("page"))
	if math.IsNaN(pageNumF) || pageNumF == 0 {
		pageNumF = 1
	}
	pageNum := math.Max(1, pageNumF)
	totalPages := math.Max(1, math.Ceil(float64(len(items))/perPage))
	start := int((pageNum - 1) * perPage)
	end := int(start + int(perPage))
	if start > len(items) {
		start = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	page := items[start:end]
	var link string
	if pageNum < totalPages {
		qn := q.clone()
		qn.set("page", strconv.Itoa(int(pageNum)+1))
		qn.set("per_page", strconv.Itoa(int(perPage)))
		next := "http://" + host + basePath + "?" + qn.encode()
		ql := q.clone()
		ql.set("page", strconv.Itoa(int(totalPages)))
		ql.set("per_page", strconv.Itoa(int(perPage)))
		last := "http://" + host + basePath + "?" + ql.encode()
		link = "<" + next + ">; rel=\"next\", <" + last + ">; rel=\"last\""
	}
	return page, link
}

func linkHeader(link string) map[string]string {
	if link == "" {
		return nil
	}
	return map[string]string{"link": link}
}

// ------------------------------------------------------------------ REST JSON shapes

type ownerJSON struct {
	Login     string `json:"login"`
	ID        int    `json:"id"`
	Type      string `json:"type"`
	AvatarURL string `json:"avatar_url"`
}

type repoJSON struct {
	ID              int64     `json:"id"`
	NodeID          string    `json:"node_id"`
	Name            string    `json:"name"`
	FullName        string    `json:"full_name"`
	Owner           ownerJSON `json:"owner"`
	Private         bool      `json:"private"`
	HTMLURL         string    `json:"html_url"`
	Description     string    `json:"description"`
	Fork            bool      `json:"fork"`
	URL             string    `json:"url"`
	CreatedAt       string    `json:"created_at"`
	UpdatedAt       string    `json:"updated_at"`
	PushedAt        string    `json:"pushed_at"`
	DefaultBranch   string    `json:"default_branch"`
	HasIssues       bool      `json:"has_issues"`
	HasProjects     bool      `json:"has_projects"`
	HasWiki         bool      `json:"has_wiki"`
	HasPages        bool      `json:"has_pages"`
	HasDownloads    bool      `json:"has_downloads"`
	Archived        bool      `json:"archived"`
	Disabled        bool      `json:"disabled"`
	Visibility      string    `json:"visibility"`
	License         any       `json:"license"`
	Topics          []string  `json:"topics"`
	OpenIssuesCount int       `json:"open_issues_count"`
}

func (s *server) repoJSON(r *repo) repoJSON {
	vis := "public"
	if r.isPrivate {
		vis = "private"
	}
	open := 0
	for _, i := range r.issuesOrder {
		if i.state == "open" {
			open++
		}
	}
	return repoJSON{
		ID: r.id, NodeID: gid("repo", viewerLogin+"/"+r.name), Name: r.name,
		FullName: viewerLogin + "/" + r.name,
		Owner:    ownerJSON{Login: viewerLogin, ID: viewerID, Type: "User", AvatarURL: ""},
		Private:  r.isPrivate, HTMLURL: "", Description: r.description, Fork: false,
		URL: "", CreatedAt: r.createdAt, UpdatedAt: r.createdAt, PushedAt: r.createdAt,
		DefaultBranch: r.defaultBranch, HasIssues: true, HasProjects: true, HasWiki: true,
		HasPages: false, HasDownloads: true, Archived: false, Disabled: false,
		Visibility: vis, License: nil, Topics: []string{}, OpenIssuesCount: open,
	}
}

type labelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type userRefJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type issueJSON struct {
	ID                int64       `json:"id"`
	NodeID            string      `json:"node_id"`
	Number            int         `json:"number"`
	Title             string      `json:"title"`
	Body              string      `json:"body"`
	State             string      `json:"state"`
	User              userRefJSON `json:"user"`
	Labels            []labelJSON `json:"labels"`
	Assignee          any         `json:"assignee"`
	Assignees         []any       `json:"assignees"`
	Milestone         any         `json:"milestone"`
	Locked            bool        `json:"locked"`
	Comments          int         `json:"comments"`
	CreatedAt         string      `json:"created_at"`
	UpdatedAt         string      `json:"updated_at"`
	ClosedAt          any         `json:"closed_at"`
	AuthorAssociation string      `json:"author_association"`
	HTMLURL           string      `json:"html_url"`
	URL               string      `json:"url"`
	RepositoryURL     string      `json:"repository_url"`
	LabelsURL         string      `json:"labels_url"`
	CommentsURL       string      `json:"comments_url"`
}

func (s *server) issueJSON(r *repo, i *issue) issueJSON {
	labels := make([]labelJSON, 0, len(i.labels))
	for _, n := range i.labels {
		color := "ededed"
		if l, ok := r.labels[n]; ok {
			color = l.color
		}
		labels = append(labels, labelJSON{Name: n, Color: color})
	}
	var closedAt any
	if i.state == "closed" {
		closedAt = i.updatedAt
	}
	nodeID := i.nodeID
	if nodeID == "" {
		nodeID = gid("issue", viewerLogin+"/"+r.name+":"+strconv.Itoa(i.number))
	}
	return issueJSON{
		ID: i.id, NodeID: nodeID,
		Number: i.number, Title: i.title, Body: i.body, State: i.state,
		User: userRefJSON{Login: viewerLogin, ID: viewerID, Type: "User"},
		Labels: labels, Assignee: nil, Assignees: []any{}, Milestone: nil, Locked: false,
		Comments: len(i.comments), CreatedAt: i.createdAt, UpdatedAt: i.updatedAt,
		ClosedAt: closedAt, AuthorAssociation: "OWNER", HTMLURL: "", URL: "",
		RepositoryURL: "", LabelsURL: "", CommentsURL: "",
	}
}

type pullJSON struct {
	ID             int64       `json:"id"`
	NodeID         string      `json:"node_id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	State          string      `json:"state"`
	Merged         bool        `json:"merged"`
	MergedAt       any         `json:"merged_at"`
	MergeCommitSha any         `json:"merge_commit_sha"`
	Mergeable      bool        `json:"mergeable"`
	MergeableState string      `json:"mergeable_state"`
	Head           pullRefJSON `json:"head"`
	Base           pullRefJSON `json:"base"`
	User           userRefJSON `json:"user"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	ClosedAt       any         `json:"closed_at"`
	Draft          bool        `json:"draft"`
	Locked         bool        `json:"locked"`
	HTMLURL        string      `json:"html_url"`
	URL            string      `json:"url"`
	Commits        int         `json:"commits"`
	Additions      int         `json:"additions"`
	Deletions      int         `json:"deletions"`
	ChangedFiles   int         `json:"changed_files"`
}

type pullRefJSON struct {
	Label string      `json:"label"`
	Ref   string      `json:"ref"`
	Sha   string      `json:"sha"`
	User  loginJSON   `json:"user"`
}

type loginJSON struct {
	Login string `json:"login"`
}

func (s *server) pullJSON(r *repo, p *pull) pullJSON {
	mergeable := s.st.prMergeable(r, p)
	var closedAt any
	if p.state == "closed" {
		closedAt = p.createdAt
	}
	var mergedAt, mergeSha any
	if p.mergedAt != nil {
		mergedAt = *p.mergedAt
	}
	if p.mergeCommitSha != nil {
		mergeSha = *p.mergeCommitSha
	}
	return pullJSON{
		ID: p.id, NodeID: gid("pr", viewerLogin+"/"+r.name+":"+strconv.Itoa(p.number)),
		Number: p.number, Title: p.title, Body: p.body, State: p.state, Merged: p.merged,
		MergedAt: mergedAt, MergeCommitSha: mergeSha,
		Mergeable: mergeable == "MERGEABLE", MergeableState: strings.ToLower(mergeable),
		Head: pullRefJSON{Label: viewerLogin + ":" + p.headBranch, Ref: p.headBranch, Sha: p.headSha, User: loginJSON{Login: viewerLogin}},
		Base: pullRefJSON{Label: viewerLogin + ":" + p.baseBranch, Ref: p.baseBranch, Sha: p.baseSha, User: loginJSON{Login: viewerLogin}},
		User: userRefJSON{Login: viewerLogin, ID: viewerID, Type: "User"},
		CreatedAt: p.createdAt, UpdatedAt: p.createdAt, ClosedAt: closedAt,
		Draft: false, Locked: false, HTMLURL: "", URL: "",
		Commits: 1, Additions: 0, Deletions: 0, ChangedFiles: 0,
	}
}
