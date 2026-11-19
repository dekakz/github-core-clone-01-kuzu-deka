// REST v3.

package main

import (
	"encoding/base64"
	"math"
	"sort"
	"strconv"
	"strings"
)

func (s *server) handleRest(w *fastResp, r *fastReq, path string, q *queryParams, body []byte) bool {
	method := r.method
	segs := splitPath(path)
	if len(segs) < 2 || segs[0] != "api" || segs[1] != "v3" {
		return false
	}
	rest := segs[2:]
	if !isAuthed(r) {
		s.sendErr(w, 401, "Bad credentials", nil)
		return true
	}
	badJSON := func() bool {
		s.sendErr(w, 400, "Problems parsing JSON", nil)
		return true
	}

	if len(rest) == 0 {
		if method != "GET" {
			s.sendErr(w, 404, "Not Found", nil)
			return true
		}
		s.sendJSON(w, 200, map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"}, map[string]string{"x-oauth-scopes": "repo, read:org"})
		return true
	}
	if len(rest) == 1 && rest[0] == "meta" {
		s.sendJSON(w, 200, map[string]any{
			"installed_version": "1.0.0", "verifiable_password_authentication": false,
			"github_services_sha": "", "hook_ip_ranges": []any{}, "import_ip_ranges": []any{},
			"ssh_key_fingerprints": map[string]any{}, "pages": map[string]any{},
		}, nil)
		return true
	}
	if len(rest) == 1 && rest[0] == "user" && method == "GET" {
		s.sendJSON(w, 200, map[string]any{
			"login": viewerLogin, "id": viewerID, "node_id": gid("user", viewerLogin),
			"type": "User", "name": viewerName, "email": viewerEmail, "avatar_url": "",
		}, nil)
		return true
	}
	if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
		if method == "GET" {
			s.st.mu.RLock()
			all := make([]*repo, 0, len(s.st.repos))
			for _, rp := range s.st.repos {
				all = append(all, rp)
			}
			sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
			list := make([]repoJSON, 0, len(all))
			for _, rp := range all {
				list = append(list, s.repoJSON(rp))
			}
			s.st.mu.RUnlock()
			page, link := paginate(list, q, r.host, "/api/v3/user/repos")
			s.sendJSON(w, 200, page, linkHeader(link))
			return true
		}
		if method == "POST" {
			parsed, ok := jsonBody(method, body)
			if !ok {
				return badJSON()
			}
			name := jsStr(getField(parsed, "name"))
			if name == "" {
				s.sendErr(w, 422, "Repository name is required", nil)
				return true
			}
			s.st.mu.Lock()
			defer s.st.mu.Unlock()
			if _, exists := s.st.repos[name]; exists {
				s.sendErr(w, 422, "Repository already exists", nil)
				return true
			}
			isPrivate, _ := parsed["private"].(bool)
			rp := s.st.createRepoRecord(name, isPrivate, jsStr(getField(parsed, "description")))
			s.sendJSON(w, 201, s.repoJSON(rp), nil)
			return true
		}
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	if len(rest) >= 3 && rest[0] == "repos" {
		owner, name := rest[1], rest[2]
		tail := rest[3:]
		s.st.mu.RLock()
		rp := s.st.getRepo(owner, name)
		s.st.mu.RUnlock()
		if rp == nil {
			s.sendErr(w, 404, "Not Found", nil)
			return true
		}
		return s.handleRepo(w, r, rp, owner, name, tail, q, body, method, badJSON)
	}
	s.sendErr(w, 404, "Not Found", nil)
	return true
}

func getField(m map[string]any, k string) any {
	if m == nil {
		return nil
	}
	return m[k]
}

func (s *server) handleRepo(w *fastResp, r *fastReq, rp *repo, owner, name string, tail []string, q *queryParams, body []byte, method string, badJSON func() bool) bool {
	if len(tail) == 0 {
		if method == "GET" {
			s.st.mu.RLock()
			j := s.repoJSON(rp)
			s.st.mu.RUnlock()
			s.sendJSON(w, 200, j, nil)
			return true
		}
		if method == "PATCH" {
			parsed, ok := jsonBody(method, body)
			if !ok {
				return badJSON()
			}
			s.st.mu.Lock()
			if v, isStr := parsed["description"].(string); isStr {
				rp.description = v
			}
			if v, isBool := parsed["private"].(bool); isBool {
				rp.isPrivate = v
			}
			j := s.repoJSON(rp)
			s.st.mu.Unlock()
			s.sendJSON(w, 200, j, nil)
			return true
		}
		if method == "DELETE" {
			s.st.mu.Lock()
			delete(s.st.repos, name)
			s.st.mu.Unlock()
			h := w.Header()
			h.Set("X-Github-Request-Id", nextReqID())
			w.WriteHeader(204)
			return true
		}
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	// branches
	if len(tail) == 1 && tail[0] == "branches" && method == "GET" {
		s.st.mu.RLock()
		refs := s.st.sortedRefs(rp)
		s.st.mu.RUnlock()
		type branchJSON struct {
			Name      string `json:"name"`
			Commit    shaJSON `json:"commit"`
			Protected bool   `json:"protected"`
		}
		list := []branchJSON{}
		for _, ref := range refs {
			if strings.HasPrefix(ref[0], "refs/heads/") {
				list = append(list, branchJSON{Name: ref[0][len("refs/heads/"):], Commit: shaJSON{Sha: ref[1], URL: ""}, Protected: false})
			}
		}
		s.sendJSON(w, 200, list, nil)
		return true
	}
	if len(tail) == 2 && tail[0] == "branches" && method == "GET" {
		s.st.mu.RLock()
		sha, ok := rp.refs["refs/heads/"+tail[1]]
		s.st.mu.RUnlock()
		if !ok {
			s.sendErr(w, 404, "Not Found", nil)
			return true
		}
		s.sendJSON(w, 200, map[string]any{"name": tail[1], "commit": map[string]any{"sha": sha, "url": ""}, "protected": false}, nil)
		return true
	}
	// commits
	if len(tail) == 1 && tail[0] == "commits" && method == "GET" {
		s.st.mu.RLock()
		defer s.st.mu.RUnlock()
		refName := q.get("sha")
		hasSha := q.has("sha") && refName != ""
		var tip string
		var tipOk bool
		if hasSha {
			if v, ok := rp.refs["refs/heads/"+refName]; ok {
				tip, tipOk = v, true
			} else if v, ok := rp.refs[refName]; ok {
				tip, tipOk = v, true
			} else if isHex40(refName) {
				tip, tipOk = refName, true
			}
		} else {
			tip, tipOk = rp.refs["refs/heads/"+rp.defaultBranch]
		}
		list := []map[string]any{}
		if tipOk {
			seen := make(map[string]bool)
			stack := []string{tip}
			for len(stack) > 0 {
				oid := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if seen[oid] {
					continue
				}
				seen[oid] = true
				obj := s.st.objects[oid]
				if obj == nil || obj.typ != objCommit {
					continue
				}
				c := parseCommit(obj.data)
				authorName := c.author
				if i := strings.Index(authorName, " <"); i >= 0 {
					authorName = authorName[:i]
				}
				parents := []map[string]any{}
				for _, p := range c.parents {
					parents = append(parents, map[string]any{"sha": p})
				}
				list = append(list, map[string]any{
					"sha": oid,
					"commit": map[string]any{
						"message": c.message,
						"author":  map[string]any{"name": authorName, "email": viewerEmail, "date": isoOf(epochSec)},
						"committer": map[string]any{"name": viewerName, "email": viewerEmail, "date": isoOf(epochSec)},
					},
					"author":    map[string]any{"login": viewerLogin, "id": viewerID},
					"committer": map[string]any{"login": viewerLogin, "id": viewerID},
					"parents":   parents,
				})
				stack = append(stack, c.parents...)
			}
		}
		s.sendJSON(w, 200, list, nil)
		return true
	}
	// git/refs
	if len(tail) >= 2 && tail[0] == "git" && tail[1] == "refs" {
		if method == "POST" && len(tail) == 2 {
			parsed, ok := jsonBody(method, body)
			if !ok {
				return badJSON()
			}
			ref := jsStr(getField(parsed, "ref"))
			sha := jsStr(getField(parsed, "sha"))
			if !strings.HasPrefix(ref, "refs/") || !isHex40(sha) {
				s.sendErr(w, 422, "Invalid ref", nil)
				return true
			}
			s.st.mu.Lock()
			rp.refs[ref] = sha
			rp.packCacheMu.Lock()
			rp.packCache = make(map[string]cachedPack)
			rp.packCacheMu.Unlock()
			s.st.mu.Unlock()
			s.sendJSON(w, 201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
			return true
		}
		if method == "GET" && len(tail) >= 3 {
			ref := strings.Join(tail[2:], "/")
			full := ref
			if !strings.HasPrefix(ref, "refs/") {
				full = "refs/" + ref
			}
			s.st.mu.RLock()
			sha, ok := rp.refs[full]
			s.st.mu.RUnlock()
			if !ok {
				s.sendErr(w, 404, "Not Found", nil)
				return true
			}
			s.sendJSON(w, 200, map[string]any{"ref": full, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
			return true
		}
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	// contents
	if len(tail) >= 1 && tail[0] == "contents" {
		return s.handleContents(w, r, rp, owner, name, tail[1:], q, body, method, badJSON)
	}
	// labels
	if len(tail) == 1 && tail[0] == "labels" {
		if method == "GET" {
			s.st.mu.RLock()
			list := make([]map[string]any, 0, len(rp.labelsOrder))
			for _, n := range rp.labelsOrder {
				l := rp.labels[n]
				list = append(list, map[string]any{"id": nextEntityID(), "name": l.name, "color": l.color, "default": false, "description": ""})
			}
			s.st.mu.RUnlock()
			s.sendJSON(w, 200, list, nil)
			return true
		}
		if method == "POST" {
			parsed, ok := jsonBody(method, body)
			if !ok {
				return badJSON()
			}
			lname := jsStr(getField(parsed, "name"))
			if lname == "" {
				s.sendErr(w, 422, "Label name is required", nil)
				return true
			}
			color := jsStrDef(getField(parsed, "color"), "ededed")
			s.st.mu.Lock()
			rp.labels[lname] = &label{name: lname, color: color}
			rp.invalidateIssueJSON()
			found := false
			for _, n := range rp.labelsOrder {
				if n == lname {
					found = true
					break
				}
			}
			if !found {
				rp.labelsOrder = append(rp.labelsOrder, lname)
			}
			s.st.mu.Unlock()
			s.sendJSON(w, 201, map[string]any{"id": nextEntityID(), "name": lname, "color": color, "default": false, "description": ""}, nil)
			return true
		}
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	// issues
	if len(tail) >= 1 && tail[0] == "issues" {
		return s.handleIssues(w, r, rp, owner, name, tail, q, body, method, badJSON)
	}
	// pulls
	if len(tail) >= 1 && tail[0] == "pulls" {
		return s.handlePulls(w, r, rp, owner, name, tail, q, body, method, badJSON)
	}
	s.sendErr(w, 404, "Not Found", nil)
	return true
}

type shaJSON struct {
	Sha string `json:"sha"`
	URL string `json:"url"`
}

func (s *server) handleContents(w *fastResp, r *fastReq, rp *repo, owner, name string, tail []string, q *queryParams, body []byte, method string, badJSON func() bool) bool {
	filePath := strings.Join(tail, "/")
	refParam := q.get("ref")
	hasRef := q.has("ref")
	var refName string
	s.st.mu.RLock()
	if hasRef {
		if _, ok := rp.refs["refs/heads/"+refParam]; ok {
			refName = "refs/heads/" + refParam
		} else if _, ok := rp.refs[refParam]; ok {
			refName = refParam
		} else if isHex40(refParam) {
			refName = refParam
		} else {
			refName = "refs/heads/" + rp.defaultBranch
		}
	} else {
		refName = "refs/heads/" + rp.defaultBranch
	}
	s.st.mu.RUnlock()
	if method == "GET" {
		s.st.mu.RLock()
		defer s.st.mu.RUnlock()
		var tip string
		var tipOk bool
		if isHex40(refName) {
			tip, tipOk = refName, true
		} else {
			tip, tipOk = rp.refs[refName]
		}
		if !tipOk {
			s.sendErr(w, 404, "Not Found", nil)
			return true
		}
		obj := s.st.objects[tip]
		if obj == nil {
			s.sendErr(w, 404, "Not Found", nil)
			return true
		}
		treeOid := parseCommit(obj.data).tree
		flat := s.st.flattenTree(treeOid)
		if filePath == "" {
			entries := []map[string]any{}
			seen := make(map[string]bool)
			keys := append([]string{}, flat.keys...)
			sort.Strings(keys)
			for _, p := range keys {
				first := strings.Split(p, "/")[0]
				if seen[first] {
					continue
				}
				seen[first] = true
				// Mirror Node: isDir = !flat.has(first) && any key starts with first+'/'
				_, isFile := flat.get(first)
				isDir := false
				if !isFile {
					for _, k := range flat.keys {
						if strings.HasPrefix(k, first+"/") {
							isDir = true
							break
						}
					}
				}
				typ := "file"
				oid := ""
				size := 0
				if isDir {
					typ = "dir"
					oid = strings.Repeat("0", 40)
				} else {
					if e, ok := flat.get(first); ok {
						oid = e.oid
						if o := s.st.objects[oid]; o != nil {
							size = len(o.data)
						}
					} else {
						oid = strings.Repeat("0", 40)
					}
				}
				entries = append(entries, map[string]any{"type": typ, "name": first, "path": first, "sha": oid, "size": size})
			}
			s.sendJSON(w, 200, entries, nil)
			return true
		}
		if entry, ok := flat.get(filePath); ok {
			data := s.st.objects[entry.oid].data
			parts := strings.Split(filePath, "/")
			s.sendJSON(w, 200, map[string]any{
				"type": "file", "encoding": "base64", "size": len(data),
				"name": parts[len(parts)-1], "path": filePath,
				"content": base64.StdEncoding.EncodeToString(data), "sha": entry.oid,
			}, nil)
			return true
		}
		var children []string
		for _, k := range flat.keys {
			if strings.HasPrefix(k, filePath+"/") {
				children = append(children, k)
			}
		}
		if len(children) > 0 {
			sort.Strings(children)
			entries := []map[string]any{}
			seen := make(map[string]bool)
			for _, p := range children {
				restPath := p[len(filePath)+1:]
				first := strings.Split(restPath, "/")[0]
				if seen[first] {
					continue
				}
				seen[first] = true
				isDir := strings.Contains(restPath, "/")
				typ := "file"
				oid := ""
				size := 0
				if isDir {
					typ = "dir"
					oid = strings.Repeat("0", 40)
				} else {
					if e, ok := flat.get(p); ok {
						oid = e.oid
						if o := s.st.objects[oid]; o != nil {
							size = len(o.data)
						}
					} else {
						oid = strings.Repeat("0", 40)
					}
				}
				entries = append(entries, map[string]any{"type": typ, "name": first, "path": filePath + "/" + first, "sha": oid, "size": size})
			}
			s.sendJSON(w, 200, entries, nil)
			return true
		}
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	if method == "PUT" || method == "DELETE" {
		parsed, ok := jsonBody(method, body)
		if !ok {
			return badJSON()
		}
		branch := rp.defaultBranch
		if v, isStr := parsed["branch"].(string); isStr {
			branch = v
		}
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		tip, hasTip := rp.refs["refs/heads/"+branch]
		var flat *orderedFlat
		if hasTip {
			flat = s.st.flattenTree(parseCommit(s.st.objects[tip].data).tree)
		} else {
			flat = newOrderedFlat()
		}
		existing, hasExisting := flat.get(filePath)
		if method == "PUT" {
			contentB64 := jsStr(getField(parsed, "content"))
			if hasExisting {
				if shaVal, present := parsed["sha"]; !present {
					s.sendErr(w, 409, "Conflict", nil)
					return true
				} else if shaStr, isStr := shaVal.(string); isStr && shaStr != existing.oid {
					s.sendErr(w, 409, "Conflict", nil)
					return true
				}
			}
			content := lenientB64Decode(contentB64)
			message := jsStrDef(getField(parsed, "message"), "Update")
			commitSha := s.st.commitFile(rp, branch, filePath, content, true, message)
			blobOid := s.st.putObject(objBlob, content)
			parts := strings.Split(filePath, "/")
			status := 201
			if hasExisting {
				status = 200
			}
			s.sendJSON(w, status, map[string]any{
				"content": map[string]any{"name": parts[len(parts)-1], "path": filePath, "sha": blobOid, "size": len(content), "encoding": "base64"},
				"commit":  map[string]any{"sha": commitSha, "message": message},
			}, nil)
			return true
		}
		// DELETE contents
		if !hasExisting {
			s.sendErr(w, 404, "Not Found", nil)
			return true
		}
		if sha, isStr := parsed["sha"].(string); isStr && sha != existing.oid {
			s.sendErr(w, 409, "Conflict", nil)
			return true
		}
		message := jsStrDef(getField(parsed, "message"), "Delete")
		commitSha := s.st.commitFile(rp, branch, filePath, nil, false, message)
		s.sendJSON(w, 200, map[string]any{"commit": map[string]any{"sha": commitSha, "message": message}}, nil)
		return true
	}
	s.sendErr(w, 404, "Not Found", nil)
	return true
}

func lenientB64Decode(s string) []byte {
	clean := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' || c == '\n' || c == '\t' || c == ' ' {
			continue
		}
		clean = append(clean, c)
	}
	// Node's base64 decoder ignores non-alphabet chars; strip them.
	filtered := make([]byte, 0, len(clean))
	for _, c := range clean {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			filtered = append(filtered, c)
		}
	}
	for len(filtered)%4 != 0 {
		filtered = append(filtered, '=')
	}
	out, err := base64.StdEncoding.DecodeString(string(filtered))
	if err == nil {
		return out
	}
	out, _ = base64.RawStdEncoding.DecodeString(string(filtered))
	if out == nil {
		return []byte{}
	}
	return out
}

func (s *server) handleIssues(w *fastResp, r *fastReq, rp *repo, owner, name string, tail []string, q *queryParams, body []byte, method string, badJSON func() bool) bool {
	if len(tail) == 1 {
		if method == "GET" {
			s.st.mu.RLock()
			state := q.get("state")
			if state == "" {
				state = "open"
			}
			var list []*issue
			for _, i := range rp.issuesOrder {
				if state == "all" || i.state == state {
					list = append(list, i)
				}
			}
			sortStr := q.get("sort")
			if sortStr == "" {
				sortStr = "created"
			}
			dir := q.get("direction")
			if dir == "" {
				dir = "desc"
			}
			if sortStr == "created" {
				sort.SliceStable(list, func(a, b int) bool {
					if dir == "asc" {
						return list[a].seq < list[b].seq
					}
					return list[a].seq > list[b].seq
				})
			}
			jsonList := make([]issueJSON, 0, len(list))
			for _, i := range list {
				jsonList = append(jsonList, s.issueJSON(rp, i))
			}
			s.st.mu.RUnlock()
			page, link := paginate(jsonList, q, r.host, "/api/v3/repos/"+owner+"/"+name+"/issues")
			s.sendJSON(w, 200, page, linkHeader(link))
			return true
		}
		if method == "POST" {
			var labelNames []string
			var titleText, bodyText string
			fTitle, fBody, fLabels, fast := extractIssueFields(body)
			if fast {
				titleText, bodyText, labelNames = fTitle, fBody, fLabels
				for _, ln := range labelNames {
					s.st.mu.RLock()
					_, exists := rp.labels[ln]
					s.st.mu.RUnlock()
					if !exists {
						s.sendErr(w, 422, "Label does not exist: "+ln, nil)
						return true
					}
				}
			} else {
				parsed, ok := jsonBody(method, body)
				if !ok {
					return badJSON()
				}
				if raw, isArr := parsed["labels"].([]any); isArr {
					for _, l := range raw {
						var ln string
						var isLabel bool
						switch t := l.(type) {
						case string:
							ln, isLabel = t, true
						case map[string]any:
							if n, isStr := t["name"].(string); isStr {
								ln, isLabel = n, true
							}
						}
						if !isLabel {
							continue
						}
						s.st.mu.RLock()
						_, exists := rp.labels[ln]
						s.st.mu.RUnlock()
						if !exists {
							s.sendErr(w, 422, "Label does not exist: "+ln, nil)
							return true
						}
						labelNames = append(labelNames, ln)
					}
				}
				titleText = jsStr(getField(parsed, "title"))
				bodyText = jsStr(getField(parsed, "body"))
			}
			s.st.mu.Lock()
			number := rp.nextNumber
			rp.nextNumber++
			sec := tick()
			iss := &issue{
				id: nextEntityID(), number: number, title: titleText,
				body: bodyText, state: "open", labels: labelNames,
				seq: number, createdAt: isoOf(sec), updatedAt: isoOf(sec), comments: []*comment{},
				nodeID: gid("issue", viewerLogin+"/"+name+":"+strconv.Itoa(number)),
			}
			rp.issues[number] = iss
			rp.issuesOrder = append(rp.issuesOrder, iss)
			jb := appendIssueJSON(nil, rp, iss)
			cp := new([]byte)
			*cp = jb
			iss.cached.Store(cp)
			s.st.mu.Unlock()
			s.sendJSONBytes(w, 201, jb, nil)
			return true
		}
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	num, numOk := jsInt(tail[1])
	if !numOk {
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	if len(tail) == 2 {
		if method == "GET" {
			s.st.mu.RLock()
			iss := rp.issues[num]
			if iss == nil {
				s.st.mu.RUnlock()
				s.sendErr(w, 404, "Not Found", nil)
				return true
			}
			if cp := iss.cached.Load(); cp != nil {
				s.st.mu.RUnlock()
				s.sendJSONBytes(w, 200, *cp, nil)
				return true
			}
			jb := appendIssueJSON(nil, rp, iss)
			cp2 := new([]byte)
			*cp2 = jb
			iss.cached.Store(cp2)
			s.st.mu.RUnlock()
			s.sendJSONBytes(w, 200, jb, nil)
			return true
		}
		if method == "PATCH" {
			parsed, ok := jsonBody(method, body)
			if !ok {
				return badJSON()
			}
			s.st.mu.Lock()
			iss := rp.issues[num]
			if iss == nil {
				s.st.mu.Unlock()
				s.sendErr(w, 404, "Not Found", nil)
				return true
			}
			if st, isStr := parsed["state"].(string); isStr && (st == "open" || st == "closed") {
				iss.state = st
			}
			if v, isStr := parsed["title"].(string); isStr {
				iss.title = v
			}
			if v, isStr := parsed["body"].(string); isStr {
				iss.body = v
			}
			iss.updatedAt = nowIso()
			iss.invalidateJSON()
			j := s.issueJSON(rp, iss)
			s.st.mu.Unlock()
			s.sendJSON(w, 200, j, nil)
			return true
		}
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	if len(tail) == 3 && tail[2] == "comments" {
		if method == "GET" {
			s.st.mu.RLock()
			iss := rp.issues[num]
			if iss == nil {
				s.st.mu.RUnlock()
				s.sendErr(w, 404, "Not Found", nil)
				return true
			}
			list := make([]map[string]any, 0, len(iss.comments))
			for _, c := range iss.comments {
				list = append(list, map[string]any{
					"id": c.id, "node_id": gid("comment", viewerLogin+"/"+name+":"+strconv.Itoa(num)+":"+strconv.FormatInt(c.id, 10)),
					"body": c.body, "user": map[string]any{"login": viewerLogin, "id": viewerID},
					"created_at": c.createdAt, "updated_at": c.createdAt,
				})
			}
			s.st.mu.RUnlock()
			s.sendJSON(w, 200, list, nil)
			return true
		}
		if method == "POST" {
			parsed, ok := jsonBody(method, body)
			if !ok {
				return badJSON()
			}
			s.st.mu.Lock()
			iss := rp.issues[num]
			if iss == nil {
				s.st.mu.Unlock()
				s.sendErr(w, 404, "Not Found", nil)
				return true
			}
			c := &comment{id: nextEntityID(), body: jsStr(getField(parsed, "body")), createdAt: nowIso()}
			iss.comments = append(iss.comments, c)
			iss.invalidateJSON()
			resp := map[string]any{
				"id": c.id, "node_id": gid("comment", viewerLogin+"/"+name+":"+strconv.Itoa(num)+":"+strconv.FormatInt(c.id, 10)),
				"body": c.body, "user": map[string]any{"login": viewerLogin, "id": viewerID},
				"created_at": c.createdAt, "updated_at": c.createdAt,
			}
			s.st.mu.Unlock()
			s.sendJSON(w, 201, resp, nil)
			return true
		}
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	s.sendErr(w, 404, "Not Found", nil)
	return true
}

func (s *server) handlePulls(w *fastResp, r *fastReq, rp *repo, owner, name string, tail []string, q *queryParams, body []byte, method string, badJSON func() bool) bool {
	if len(tail) == 1 {
		if method == "GET" {
			s.st.mu.RLock()
			state := q.get("state")
			if state == "" {
				state = "open"
			}
			var list []*pull
			for _, p := range rp.pullsOrder {
				if state == "all" || p.state == state {
					list = append(list, p)
				}
			}
			sort.SliceStable(list, func(a, b int) bool { return list[a].seq > list[b].seq })
			jsonList := make([]pullJSON, 0, len(list))
			for _, p := range list {
				jsonList = append(jsonList, s.pullJSON(rp, p))
			}
			s.st.mu.RUnlock()
			page, link := paginate(jsonList, q, r.host, "/api/v3/repos/"+owner+"/"+name+"/pulls")
			s.sendJSON(w, 200, page, linkHeader(link))
			return true
		}
		if method == "POST" {
			parsed, ok := jsonBody(method, body)
			if !ok {
				return badJSON()
			}
			headBranch := jsStr(getField(parsed, "head"))
			baseBranch := rp.defaultBranch
			if v, has := parsed["base"]; has && v != nil {
				baseBranch = jsStr(v)
			}
			s.st.mu.Lock()
			defer s.st.mu.Unlock()
			headSha, hasHead := rp.refs["refs/heads/"+headBranch]
			baseSha, hasBase := rp.refs["refs/heads/"+baseBranch]
			if !hasHead || !hasBase {
				s.sendErr(w, 422, "Head or base branch not found", nil)
				return true
			}
			for _, p := range rp.pullsOrder {
				if p.state == "open" && p.headBranch == headBranch && p.baseBranch == baseBranch {
					s.sendErr(w, 422, "A pull request already exists for this branch", nil)
					return true
				}
			}
			number := rp.nextNumber
			rp.nextNumber++
			sec := tick()
			pr := &pull{
				id: nextEntityID(), number: number, title: jsStr(getField(parsed, "title")),
				body: jsStr(getField(parsed, "body")), state: "open", merged: false,
				headBranch: headBranch, baseBranch: baseBranch, headSha: headSha, baseSha: baseSha,
				seq: number, createdAt: isoOf(sec),
			}
			rp.pulls[number] = pr
			rp.pullsOrder = append(rp.pullsOrder, pr)
			s.sendJSON(w, 201, s.pullJSON(rp, pr), nil)
			return true
		}
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	num, numOk := jsInt(tail[1])
	if !numOk {
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	if len(tail) == 2 {
		if method == "GET" {
			s.st.mu.RLock()
			pr := rp.pulls[num]
			if pr == nil {
				s.st.mu.RUnlock()
				s.sendErr(w, 404, "Not Found", nil)
				return true
			}
			j := s.pullJSON(rp, pr)
			s.st.mu.RUnlock()
			s.sendJSON(w, 200, j, nil)
			return true
		}
		s.sendErr(w, 404, "Not Found", nil)
		return true
	}
	if len(tail) == 3 && tail[2] == "files" && method == "GET" {
		s.st.mu.RLock()
		pr := rp.pulls[num]
		if pr == nil {
			s.st.mu.RUnlock()
			s.sendErr(w, 404, "Not Found", nil)
			return true
		}
		files := s.st.prFiles(rp, pr)
		list := make([]map[string]any, 0, len(files))
		for _, f := range files {
			list = append(list, map[string]any{
				"filename": f.filename, "status": f.status, "additions": f.additions,
				"deletions": f.deletions, "changes": f.changes,
			})
		}
		s.st.mu.RUnlock()
		s.sendJSON(w, 200, list, nil)
		return true
	}
	if len(tail) == 3 && tail[2] == "merge" && method == "PUT" {
		s.st.mu.Lock()
		defer s.st.mu.Unlock()
		pr := rp.pulls[num]
		if pr == nil {
			s.sendErr(w, 404, "Not Found", nil)
			return true
		}
		if pr.state != "open" {
			s.sendErr(w, 405, "Pull request is not open", nil)
			return true
		}
		if s.st.prConflicts(rp, pr) {
			s.sendErr(w, 405, "Merge conflict", nil)
			return true
		}
		headFlat := s.st.flattenTree(parseCommit(s.st.objects[pr.headSha].data).tree)
		baseTip, hasBase := rp.refs["refs/heads/"+pr.baseBranch]
		var baseTipPtr *string
		if hasBase {
			baseTipPtr = &baseTip
		}
		mergeSha := s.st.makeCommit(rp, baseTipPtr, headFlat, "Merge pull request #"+strconv.Itoa(pr.number)+" from "+pr.headBranch+"\n\n"+pr.title)
		rp.refs["refs/heads/"+pr.baseBranch] = mergeSha
		pr.state = "closed"
		pr.merged = true
		mergedAt := nowIso()
		pr.mergedAt = &mergedAt
		pr.mergeCommitSha = &mergeSha
		pr.baseSha = mergeSha
		s.sendJSON(w, 200, map[string]any{"sha": mergeSha, "merged": true, "message": "Pull Request successfully merged"}, nil)
		return true
	}
	s.sendErr(w, 404, "Not Found", nil)
	return true
}

var _ = math.NaN
