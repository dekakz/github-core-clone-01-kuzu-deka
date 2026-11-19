// Git smart HTTP: advertisements, upload-pack (v2 + v0) and receive-pack.

package main

import (
	"bytes"
	"errors"
	"sort"
	"strconv"
	"strings"
)

func (s *store) sortedRefs(r *repo) [][2]string {
	names := make([]string, 0, len(r.refs))
	for n := range r.refs {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([][2]string, 0, len(names))
	for _, n := range names {
		out = append(out, [2]string{n, r.refs[n]})
	}
	return out
}

func refCaps(r *repo, service string) string {
	symref := "symref=HEAD:refs/heads/" + r.defaultBranch
	if service == "upload" {
		return "side-band-64k ofs-delta thin-pack shallow multi_ack " + symref + " agent=github-core-clone/1.0"
	}
	return "report-status side-band-64k delete-refs ofs-delta " + symref + " agent=github-core-clone/1.0"
}

func (s *store) v0Advertisement(r *repo, service string) []byte {
	caps := refCaps(r, service)
	refs := s.sortedRefs(r)
	var lines []pktItem
	if len(refs) == 0 {
		lines = append(lines, pktStr(zeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		headSha := r.refs["refs/heads/"+r.defaultBranch]
		if headSha == "" {
			headSha = refs[0][1]
		}
		lines = append(lines, pktStr(headSha+" HEAD\x00"+caps+"\n"))
		for _, ref := range refs {
			name, oid := ref[0], ref[1]
			lines = append(lines, pktStr(oid+" "+name+"\n"))
			if obj := s.objects[oid]; strings.HasPrefix(name, "refs/tags/") && obj != nil && obj.typ == objTag {
				lines = append(lines, pktStr(s.peelToCommit(oid)+" "+name+"^{}\n"))
			}
		}
	}
	lines = append(lines, pktFlushItem())
	serviceName := "git-upload-pack"
	if service == "receive" {
		serviceName = "git-receive-pack"
	}
	head := append(pktLine([]byte("# service="+serviceName+"\n")), pktFlush...)
	return append(head, pktLines(lines)...)
}

func v2Advertisement() []byte {
	return pktLines([]pktItem{
		pktStr("version 2\n"),
		pktStr("ls-refs=unborn\n"),
		pktStr("fetch=shallow filter\n"),
		pktStr("object-format=sha1\n"),
		pktStr("agent=github-core-clone/1.0\n"),
		pktFlushItem(),
	})
}

func sidebandPack(pack []byte) []byte {
	var parts [][]byte
	const chunk = 60000
	for i := 0; i < len(pack); i += chunk {
		end := i + chunk
		if end > len(pack) {
			end = len(pack)
		}
		seg := make([]byte, 1+(end-i))
		seg[0] = 1
		copy(seg[1:], pack[i:end])
		parts = append(parts, pktLine(seg))
	}
	if len(pack) == 0 {
		parts = append(parts, pktLine([]byte{1}))
	}
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}

// buildFetchResponse renders a complete upload-pack fetch response for one
// pack and shallow plan, in one buffer. v2 adds the acknowledgments section.
func buildFetchResponse(pack []byte, plan fetchPlan, isV2 bool, hasShallow bool) []byte {
	var buf bytes.Buffer
	if isV2 {
		buf.Write(pktLine([]byte("acknowledgments\n")))
		buf.Write(pktLine([]byte("NAK\n")))
		buf.Write(pktFlush)
	}
	if !isV2 {
		buf.Write(pktLine([]byte("NAK\n")))
	}
	if isV2 && hasShallow {
		buf.Write(pktLine([]byte("shallow-info\n")))
	}
	for _, sh := range plan.newShallow {
		buf.Write(pktLine([]byte("shallow " + sh + "\n")))
	}
	for _, sh := range plan.unshallow {
		buf.Write(pktLine([]byte("unshallow " + sh + "\n")))
	}
	if isV2 && hasShallow {
		buf.Write(pktFlush)
	}
	if isV2 {
		buf.Write(pktLine([]byte("packfile\n")))
	}
	buf.Write(sidebandPack(pack))
	return buf.Bytes()
}

func (s *store) handleUploadPack(r *repo, body []byte, isV2 bool) ([]byte, error) {
	packets, err := decodePktLines(body)
	if err != nil {
		return nil, err
	}
	if isV2 {
		var command string
		var args []string
		seenDelim := false
		for _, p := range packets {
			if p.kind == 'f' {
				break
			}
			if p.kind == 'e' {
				seenDelim = true
				continue
			}
			line := string(p.payload)
			if !seenDelim {
				if strings.HasPrefix(line, "command=") {
					command = strings.TrimSpace(line[8:])
				}
			} else {
				args = append(args, strings.TrimSpace(line))
			}
		}
		if command == "ls-refs" {
			peel, symrefs, unborn := false, false, false
			var prefixes []string
			for _, a := range args {
				switch {
				case a == "peel":
					peel = true
				case a == "symrefs":
					symrefs = true
				case a == "unborn":
					unborn = true
				case strings.HasPrefix(a, "ref-prefix "):
					prefixes = append(prefixes, a[len("ref-prefix "):])
				}
			}
			keep := func(name string) bool {
				if len(prefixes) == 0 {
					return true
				}
				for _, p := range prefixes {
					if strings.HasPrefix(name, p) {
						return true
					}
				}
				return false
			}
			var lines []pktItem
			refs := s.sortedRefs(r)
			headSha := r.refs["refs/heads/"+r.defaultBranch]
			if symrefs && keep("HEAD") {
				if headSha != "" {
					lines = append(lines, pktStr(headSha+" HEAD symref-target:refs/heads/"+r.defaultBranch+"\n"))
				} else if unborn {
					lines = append(lines, pktStr("unborn HEAD symref-target:refs/heads/"+r.defaultBranch+"\n"))
				}
			}
			for _, ref := range refs {
				name, oid := ref[0], ref[1]
				if !keep(name) {
					continue
				}
				line := oid + " " + name
				if peel {
					if obj := s.objects[oid]; strings.HasPrefix(name, "refs/tags/") && obj != nil && obj.typ == objTag {
						line += " peeled:" + s.peelToCommit(oid)
					}
				}
				lines = append(lines, pktStr(line+"\n"))
			}
			lines = append(lines, pktFlushItem())
			return pktLines(lines), nil
		}
		if command == "fetch" {
			var wants, haves, shallow []string
			var depth *int
			for _, a := range args {
				switch {
				case strings.HasPrefix(a, "want "):
					wants = append(wants, strings.TrimSpace(a[5:]))
				case strings.HasPrefix(a, "have "):
					haves = append(haves, strings.TrimSpace(a[5:]))
				case strings.HasPrefix(a, "shallow "):
					shallow = append(shallow, strings.TrimSpace(a[8:]))
				case strings.HasPrefix(a, "deepen "):
					if n, err := strconv.Atoi(strings.TrimSpace(a[7:])); err == nil {
						d := n
						depth = &d
					}
				}
			}
			cacheKey := "v2|" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + strings.Join(shallow, ",") + "|"
			if depth != nil {
				cacheKey += strconv.Itoa(*depth)
			}
			r.packCacheMu.Lock()
			cp, cached := r.packCache[cacheKey]
			r.packCacheMu.Unlock()
			if cached {
				return cp.resp, nil
			}
			plan := s.fetchObjects(r, wants, haves, depth, shallow)
			pack := writePack(plan.objs)
			resp := buildFetchResponse(pack, plan, true, depth != nil || len(shallow) > 0)
			r.packCacheMu.Lock()
			r.packCache[cacheKey] = cachedPack{pack: pack, newShallow: plan.newShallow, unshallow: plan.unshallow, resp: resp}
			r.packCacheMu.Unlock()
			return resp, nil
		}
		return nil, errors.New("unknown v2 command")
	}
	// v0 fetch
	var wants, haves, shallow []string
	var depth *int
	for _, p := range packets {
		if p.kind != 'd' {
			continue
		}
		line := strings.TrimSpace(string(p.payload))
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, strings.Split(strings.TrimSpace(line[5:]), " ")[0])
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				d := n
				depth = &d
			}
		}
	}
	cacheKey := "v0|" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + strings.Join(shallow, ",") + "|"
	if depth != nil {
		cacheKey += strconv.Itoa(*depth)
	}
	r.packCacheMu.Lock()
	cp, cached := r.packCache[cacheKey]
	r.packCacheMu.Unlock()
	if cached {
		return cp.resp, nil
	}
	plan := s.fetchObjects(r, wants, haves, depth, shallow)
	pack := writePack(plan.objs)
	resp := buildFetchResponse(pack, plan, false, false)
	r.packCacheMu.Lock()
	r.packCache[cacheKey] = cachedPack{pack: pack, newShallow: plan.newShallow, unshallow: plan.unshallow, resp: resp}
	r.packCacheMu.Unlock()
	return resp, nil
}

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

func (s *store) handleReceivePack(r *repo, body []byte) ([]byte, error) {
	off := 0
	var commands []pushCommand
	packStart := len(body)
	for off < len(body) {
		if off+4 > len(body) {
			break
		}
		ln, err := strconvParseHex4(body[off : off+4])
		if err != nil {
			break
		}
		if ln == 0 {
			off += 4
			packStart = off
			break
		}
		payload := string(body[off+4 : off+ln])
		nul := strings.IndexByte(payload, 0)
		cmdLine := payload
		if nul >= 0 {
			cmdLine = payload[:nul]
		}
		cmdLine = strings.TrimSpace(cmdLine)
		parts := strings.Split(cmdLine, " ")
		if len(parts) >= 3 {
			commands = append(commands, pushCommand{oldOid: parts[0], newOid: parts[1], ref: parts[2]})
		}
		off += ln
	}
	packBytes := body[packStart:]
	unpackOk := true
	unpackDetail := "ok"
	if len(packBytes) > 0 {
		objs, err := readPack(packBytes, s)
		if err != nil {
			unpackOk = false
			unpackDetail = err.Error()
		} else {
			for _, o := range objs {
				s.putObjectOID(o.typ, o.data, o.oid)
			}
		}
	}
	var reportLines []string
	if unpackOk {
		reportLines = append(reportLines, "unpack ok\n")
	} else {
		reportLines = append(reportLines, "unpack "+unpackDetail+"\n")
	}
	for _, cmd := range commands {
		ok := false
		reason := ""
		if unpackOk {
			current, hasCurrent := r.refs[cmd.ref]
			if cmd.newOid == zeroOID {
				if !hasCurrent {
					ok, reason = false, "no such ref"
				} else if current != cmd.oldOid && cmd.oldOid != zeroOID {
					ok, reason = false, "non-fast-forward"
				} else {
					delete(r.refs, cmd.ref)
					ok = true
				}
			} else if cmd.oldOid == zeroOID {
				if hasCurrent {
					ok, reason = false, "already exists"
				} else if s.objects[cmd.newOid] == nil {
					ok, reason = false, "object not found"
				} else {
					r.refs[cmd.ref] = cmd.newOid
					ok = true
				}
			} else {
				if !hasCurrent || current != cmd.oldOid {
					ok, reason = false, "non-fast-forward"
				} else if s.objects[cmd.newOid] == nil {
					ok, reason = false, "object not found"
				} else if !s.isAncestor(cmd.oldOid, cmd.newOid) {
					ok, reason = false, "non-fast-forward"
				} else {
					r.refs[cmd.ref] = cmd.newOid
					ok = true
				}
			}
		} else {
			reason = "unpack failed"
		}
		if ok {
			reportLines = append(reportLines, "ok "+cmd.ref+"\n")
		} else {
			reportLines = append(reportLines, "ng "+cmd.ref+" "+reason+"\n")
		}
	}
	r.packCacheMu.Lock()
	r.packCache = make(map[string]cachedPack)
	r.packCacheMu.Unlock()
	var items []pktItem
	for _, l := range reportLines {
		items = append(items, pktStr(l))
	}
	items = append(items, pktFlushItem())
	inner := pktLines(items)
	band1 := pktLine(append([]byte{1}, inner...))
	return append(band1, pktFlush...), nil
}
