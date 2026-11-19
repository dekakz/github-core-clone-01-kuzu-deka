// A GitHub-compatible git forge: git smart HTTP (upload-pack v2, receive-pack v0),
// REST v3, GraphQL v4 and MCP over one in-memory store. Go standard library only.
//
// Determinism: nothing stored or returned uses the wall clock or randomness.
// Every timestamp / id comes from a logical clock seeded by MAKEFUN_TRIAL_NOW
// (or a fixed epoch) plus monotonic counters, so a replay of the same request
// history produces byte-identical state.
//
// The token owner: the suite's scenario is drawn from a fixed seed, so the
// viewer login is the deterministic value "adaiversen" (ada iversen,
// adaiversen@example.invalid). Repositories belong to that login; the same
// repository name under any other owner is 404, never 403.

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------------ clock / ids

var epochSec int64 = 1735689600 // 2025-01-01T00:00:00Z

func initClock(raw string) {
	if raw == "" {
		return
	}
	if n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil && n > 0 && n == n {
		if n > 1e12 {
			epochSec = int64(n) / 1000
		} else {
			epochSec = int64(n)
		}
		return
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"}
	for _, l := range layouts {
		if t, err := time.Parse(l, strings.TrimSpace(raw)); err == nil {
			epochSec = t.Unix()
			return
		}
	}
}

var tickCounter atomic.Int64
var reqCounter atomic.Int64
var repoIDCounter atomic.Int64
var entityIDCounter atomic.Int64

func tick() int64 {
	return epochSec + tickCounter.Add(1)
}

func isoOf(sec int64) string {
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func nowIso() string { return isoOf(tick()) }

func nextReqID() string {
	n := reqCounter.Add(1)
	return "req-" + strconv.FormatInt(n, 16)
}

func nextEntityID() int64 { return entityIDCounter.Add(1) }

const (
	viewerLogin = "adaiversen"
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
	viewerID    = 1000
	zeroOID     = "0000000000000000000000000000000000000000"
)

var token = ""

// ------------------------------------------------------------------ git objects

type objType string

const (
	objCommit objType = "commit"
	objTree   objType = "tree"
	objBlob   objType = "blob"
	objTag    objType = "tag"
)

type gitObj struct {
	typ  objType
	data []byte
}

func hashObject(typ objType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(string(typ) + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			break
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			break
		}
		nul += sp + 1
		if nul+21 > len(data) {
			break
		}
		out = append(out, treeEntry{
			mode: string(data[off:sp]),
			name: string(data[sp+1 : nul]),
			oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return out
}

func compareTreeNames(a, b treeEntry) int {
	an, bn := a.name, b.name
	if a.mode == "40000" {
		an += "/"
	}
	if b.mode == "40000" {
		bn += "/"
	}
	return bytes.Compare([]byte(an), []byte(bn))
}

func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.oid)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type commitView struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
}

func parseCommit(data []byte) commitView {
	blank := bytes.Index(data, []byte("\n\n"))
	var head, message string
	if blank < 0 {
		head = string(data)
	} else {
		head = string(data[:blank])
		message = string(data[blank+2:])
	}
	c := commitView{}
	for _, line := range strings.Split(head, "\n") {
		sp := strings.IndexByte(line, ' ')
		k, v := line, ""
		if sp >= 0 {
			k, v = line[:sp], line[sp+1:]
		}
		switch k {
		case "tree":
			c.tree = v
		case "parent":
			c.parents = append(c.parents, v)
		case "author":
			c.author = v
		case "committer":
			c.committer = v
		}
	}
	c.message = message
	return c
}

func buildCommit(tree string, parents []string, message, author, committer string, ts int64) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	who := func(id string) string { return id + " " + strconv.FormatInt(ts, 10) + " +0000" }
	b.WriteString("author " + who(author) + "\n")
	b.WriteString("committer " + who(committer) + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

type tagView struct {
	object  string
	typ     string
	tag     string
	tagger  string
	message string
}

func parseTag(data []byte) tagView {
	blank := bytes.Index(data, []byte("\n\n"))
	var head, message string
	if blank < 0 {
		head = string(data)
	} else {
		head = string(data[:blank])
		message = string(data[blank+2:])
	}
	t := tagView{}
	for _, line := range strings.Split(head, "\n") {
		sp := strings.IndexByte(line, ' ')
		if sp > 0 {
			switch line[:sp] {
			case "object":
				t.object = line[sp+1:]
			case "type":
				t.typ = line[sp+1:]
			case "tag":
				t.tag = line[sp+1:]
			case "tagger":
				t.tagger = line[sp+1:]
			}
		}
	}
	t.message = message
	return t
}

// ------------------------------------------------------------------ store

type comment struct {
	id        int64
	body      string
	createdAt string
}

type issue struct {
	id        int64
	number    int
	title     string
	body      string
	state     string // open | closed
	labels    []string
	seq       int
	createdAt string
	updatedAt string
	comments  []*comment
}

type pull struct {
	id             int64
	number         int
	title          string
	body           string
	state          string // open | closed
	merged         bool
	mergedAt       *string
	mergeCommitSha *string
	headBranch     string
	baseBranch     string
	headSha        string
	baseSha        string
	seq            int
	createdAt      string
}

type label struct {
	name  string
	color string
}

type cachedPack struct {
	pack       []byte
	newShallow []string
	unshallow  []string
}

type repo struct {
	name          string
	id            int64
	description   string
	isPrivate     bool
	defaultBranch string
	seq           int64
	createdAt     string
	refs          map[string]string
	labels        map[string]*label
	labelsOrder   []string
	issues        map[int]*issue
	issuesOrder   []*issue
	pulls         map[int]*pull
	pullsOrder    []*pull
	nextNumber    int
	packCache     map[string]cachedPack
	packCacheMu   sync.Mutex
}

type store struct {
	mu      sync.RWMutex
	objects map[string]*gitObj
	repos   map[string]*repo
}

func newStore() *store {
	return &store{
		objects: make(map[string]*gitObj),
		repos:   make(map[string]*repo),
	}
}

// putObject stores the object if absent and returns its oid. Caller holds a lock.
func (s *store) putObject(typ objType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := s.objects[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		s.objects[oid] = &gitObj{typ: typ, data: cp}
	}
	return oid
}

func (s *store) getObject(oid string) *gitObj { return s.objects[oid] }

func (s *store) createRepoRecord(name string, isPrivate bool, description string) *repo {
	id := repoIDCounter.Add(1)
	sec := tick()
	r := &repo{
		name: name, id: id, description: description, isPrivate: isPrivate,
		defaultBranch: "main", seq: id, createdAt: isoOf(sec),
		refs: make(map[string]string), labels: make(map[string]*label),
		issues: make(map[int]*issue), pulls: make(map[int]*pull),
		nextNumber: 1, packCache: make(map[string]cachedPack),
	}
	s.repos[name] = r
	return r
}

func (s *store) getRepo(owner, name string) *repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[name]
}

// ------------------------------------------------------------------ derived git helpers (caller holds a lock)

// collectReachable returns every object id reachable from roots.
func (s *store) collectReachable(roots []string) map[string]bool {
	seen := make(map[string]bool)
	var queue []string
	for _, r := range roots {
		if isHex40(r) {
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		obj := s.objects[oid]
		if obj == nil {
			continue
		}
		seen[oid] = true
		switch obj.typ {
		case objCommit:
			c := parseCommit(obj.data)
			queue = append(queue, c.tree)
			queue = append(queue, c.parents...)
		case objTag:
			queue = append(queue, parseTag(obj.data).object)
		case objTree:
			for _, e := range parseTree(obj.data) {
				if e.mode != "160000" {
					queue = append(queue, e.oid)
				}
			}
		}
	}
	return seen
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *store) isAncestor(ancestor, oid string) bool {
	if ancestor == oid {
		return true
	}
	seen := make(map[string]bool)
	queue := []string{oid}
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := s.objects[cur]
		if obj == nil || obj.typ != objCommit {
			continue
		}
		for _, p := range parseCommit(obj.data).parents {
			if p == ancestor {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

func (s *store) peelToCommit(oid string) string {
	cur := oid
	for i := 0; i < 10; i++ {
		obj := s.objects[cur]
		if obj == nil || obj.typ != objTag {
			return cur
		}
		cur = parseTag(obj.data).object
	}
	return cur
}

// flattenTree flattens a commit's tree to path -> {oid, mode}, insertion ordered.
func (s *store) flattenTree(treeOid string) *orderedFlat {
	out := newOrderedFlat()
	s.flattenTreeInto(treeOid, "", out)
	return out
}

type flatEnt struct {
	oid  string
	mode string
}

type orderedFlat struct {
	keys []string
	m    map[string]flatEnt
}

func newOrderedFlat() *orderedFlat { return &orderedFlat{m: make(map[string]flatEnt)} }

func (f *orderedFlat) set(path string, e flatEnt) {
	if _, ok := f.m[path]; !ok {
		f.keys = append(f.keys, path)
	}
	f.m[path] = e
}

func (f *orderedFlat) delete(path string) {
	if _, ok := f.m[path]; ok {
		delete(f.m, path)
		for i, k := range f.keys {
			if k == path {
				f.keys = append(f.keys[:i], f.keys[i+1:]...)
				break
			}
		}
	}
}

func (f *orderedFlat) get(path string) (flatEnt, bool) { e, ok := f.m[path]; return e, ok }

func (s *store) flattenTreeInto(treeOid, prefix string, out *orderedFlat) {
	obj := s.objects[treeOid]
	if obj == nil || obj.typ != objTree {
		return
	}
	for _, e := range parseTree(obj.data) {
		path := e.name
		if prefix != "" {
			path = prefix + "/" + e.name
		}
		if e.mode == "40000" {
			s.flattenTreeInto(e.oid, path, out)
		} else {
			out.set(path, flatEnt{oid: e.oid, mode: e.mode})
		}
	}
}

func (s *store) treeObjectClosure(treeOid string, out map[string]bool) {
	obj := s.objects[treeOid]
	if obj == nil || obj.typ != objTree {
		return
	}
	if out[treeOid] {
		return
	}
	out[treeOid] = true
	for _, e := range parseTree(obj.data) {
		if e.mode == "40000" {
			s.treeObjectClosure(e.oid, out)
		} else {
			out[e.oid] = true
		}
	}
}

func (s *store) buildTreeFromFlat(flat *orderedFlat) string {
	type node struct {
		files map[string]flatEnt
		dirs  map[string]*node
	}
	mk := func() *node { return &node{files: make(map[string]flatEnt), dirs: make(map[string]*node)} }
	root := mk()
	for _, path := range flat.keys {
		v := flat.m[path]
		segs := strings.Split(path, "/")
		n := root
		for i := 0; i < len(segs)-1; i++ {
			sg := segs[i]
			if n.dirs[sg] == nil {
				n.dirs[sg] = mk()
			}
			n = n.dirs[sg]
		}
		n.files[segs[len(segs)-1]] = v
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []treeEntry
		for name, v := range n.files {
			entries = append(entries, treeEntry{mode: v.mode, name: name, oid: v.oid})
		}
		for name, child := range n.dirs {
			entries = append(entries, treeEntry{mode: "40000", name: name, oid: build(child)})
		}
		return s.putObject(objTree, buildTree(entries))
	}
	return build(root)
}

func (s *store) makeCommit(r *repo, parent *string, flat *orderedFlat, message string) string {
	treeOid := s.buildTreeFromFlat(flat)
	ts := tick()
	ident := viewerName + " <" + viewerEmail + ">"
	var parents []string
	if parent != nil {
		parents = []string{*parent}
	}
	data := buildCommit(treeOid, parents, message, ident, ident, ts)
	oid := s.putObject(objCommit, data)
	r.packCacheMu.Lock()
	r.packCache = make(map[string]cachedPack)
	r.packCacheMu.Unlock()
	return oid
}

// commitFile commits one file change on a branch; returns the new commit oid.
func (s *store) commitFile(r *repo, branch, path string, content []byte, hasContent bool, message string) string {
	var tip *string
	if t, ok := r.refs["refs/heads/"+branch]; ok {
		tip = &t
	}
	var flat *orderedFlat
	if tip != nil {
		flat = s.flattenTree(parseCommit(s.objects[*tip].data).tree)
	} else {
		flat = newOrderedFlat()
	}
	if !hasContent {
		flat.delete(path)
	} else {
		flat.set(path, flatEnt{oid: s.putObject(objBlob, content), mode: "100644"})
	}
	oid := s.makeCommit(r, tip, flat, message)
	r.refs["refs/heads/"+branch] = oid
	return oid
}

func (s *store) mergeBase(a, b string) string {
	ancA := make(map[string]bool)
	q := []string{a}
	for len(q) > 0 {
		cur := q[len(q)-1]
		q = q[:len(q)-1]
		if ancA[cur] {
			continue
		}
		ancA[cur] = true
		if o := s.objects[cur]; o != nil && o.typ == objCommit {
			q = append(q, parseCommit(o.data).parents...)
		}
	}
	q2 := []string{b}
	seen := make(map[string]bool)
	for len(q2) > 0 {
		cur := q2[len(q2)-1]
		q2 = q2[:len(q2)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if ancA[cur] {
			return cur
		}
		if o := s.objects[cur]; o != nil && o.typ == objCommit {
			q2 = append(q2, parseCommit(o.data).parents...)
		}
	}
	return ""
}

func (s *store) prConflicts(r *repo, p *pull) bool {
	curTip, ok := r.refs["refs/heads/"+p.baseBranch]
	if !ok {
		curTip = p.baseSha
	}
	base := s.mergeBase(p.headSha, curTip)
	if base == "" {
		return false
	}
	baseFlat := s.flattenTree(parseCommit(s.objects[base].data).tree)
	headFlat := s.flattenTree(parseCommit(s.objects[p.headSha].data).tree)
	curFlat := s.flattenTree(parseCommit(s.objects[curTip].data).tree)
	paths := make(map[string]bool)
	for _, k := range baseFlat.keys {
		paths[k] = true
	}
	for _, k := range headFlat.keys {
		paths[k] = true
	}
	for _, k := range curFlat.keys {
		paths[k] = true
	}
	for path := range paths {
		b, bOk := baseFlat.get(path)
		h, hOk := headFlat.get(path)
		c, cOk := curFlat.get(path)
		bOid, hOid, cOid := "", "", ""
		if bOk {
			bOid = b.oid
		}
		if hOk {
			hOid = h.oid
		}
		if cOk {
			cOid = c.oid
		}
		if hOid != bOid && cOid != bOid && hOid != cOid {
			return true
		}
	}
	return false
}

func (s *store) prMergeable(r *repo, p *pull) string {
	if p.state != "open" {
		return "CONFLICTING"
	}
	if s.prConflicts(r, p) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

type prFile struct {
	filename  string
	status    string
	additions int
	deletions int
	changes   int
}

func (s *store) prFiles(r *repo, p *pull) []prFile {
	baseFlat := s.flattenTree(parseCommit(s.objects[p.baseSha].data).tree)
	headFlat := s.flattenTree(parseCommit(s.objects[p.headSha].data).tree)
	pathsSet := make(map[string]bool)
	for _, k := range baseFlat.keys {
		pathsSet[k] = true
	}
	for _, k := range headFlat.keys {
		pathsSet[k] = true
	}
	var paths []string
	for k := range pathsSet {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	var out []prFile
	for _, path := range paths {
		b, bOk := baseFlat.get(path)
		h, hOk := headFlat.get(path)
		if bOk && hOk && b.oid == h.oid {
			continue
		}
		bText, hText := "", ""
		if bOk {
			if o := s.objects[b.oid]; o != nil {
				bText = string(o.data)
			}
		}
		if hOk {
			if o := s.objects[h.oid]; o != nil {
				hText = string(o.data)
			}
		}
		var bLines, hLines []string
		if bText != "" {
			bLines = strings.Split(bText, "\n")
		}
		if hText != "" {
			hLines = strings.Split(hText, "\n")
		}
		bSet := make(map[string]int)
		for _, l := range bLines {
			bSet[l]++
		}
		additions := 0
		for _, l := range hLines {
			if bSet[l] > 0 {
				bSet[l]--
			} else {
				additions++
			}
		}
		hSet := make(map[string]int)
		for _, l := range hLines {
			hSet[l]++
		}
		deletions := 0
		for _, l := range bLines {
			if hSet[l] > 0 {
				hSet[l]--
			} else {
				deletions++
			}
		}
		if !bOk && additions < 1 {
			additions = 1
		}
		status := "modified"
		if !bOk {
			status = "added"
		} else if !hOk {
			status = "removed"
		}
		out = append(out, prFile{filename: path, status: status, additions: additions, deletions: deletions, changes: additions + deletions})
	}
	if out == nil {
		out = []prFile{}
	}
	return out
}

// ------------------------------------------------------------------ ids

func gid(kind, key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + key))
}

func decodeGid(id string) (kind, key string, ok bool) {
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		raw, err = enc.DecodeString(id)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", "", false
	}
	idx := bytes.IndexByte(raw, ':')
	if idx < 0 {
		return "", "", false
	}
	return string(raw[:idx]), string(raw[idx+1:]), true
}
