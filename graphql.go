// GraphQL v4: a small parser for the documents gh and github-mcp-server send,
// plus the resolvers for viewer / repository / node / __type and the mutations.

package main

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var cursorRe = regexp.MustCompile(`^cursor:(\d+)$`)
var gidRe = regexp.MustCompile(`^([^/]+)/([^:]+):(\d+)$`)

var gqlIssueFields = []string{"id", "number", "title", "state", "body", "url", "createdAt", "updatedAt", "author", "comments", "labels", "closed", "closedAt", "repository"}
var gqlPrFields = []string{"id", "number", "title", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "body", "createdAt", "updatedAt", "author", "headRef", "baseRef", "mergeCommit", "closed", "repository", "url"}
var gqlRepoFields = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "url", "description", "createdAt", "updatedAt", "isPrivate", "fork"}
var gqlSearchEnum = []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT"}

func gqlContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

type gqlField struct {
	name       string
	alias      string
	args       map[string]any
	selections []*gqlField
}

type gqlOp struct {
	kind       string // query | mutation
	name       string
	selections []*gqlField
}

func jsonEscapeStr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if c < 0x20 {
				const hexd = "0123456789abcdef"
				b.WriteString("\\u00")
				b.WriteByte(hexd[c>>4])
				b.WriteByte(hexd[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func gqlTokenize(src string) []string {
	var tokens []string
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+3 <= len(src) && src[i:i+3] == "..." {
			tokens = append(tokens, "...")
			i += 3
			continue
		}
		if strings.ContainsRune("{}():!=$@[]", rune(c)) {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					sb.WriteByte(src[j+1])
					j += 2
				} else {
					sb.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, jsonEscapeStr(sb.String()))
			i = j + 1
			continue
		}
		if (c >= '0' && c <= '9') || (c == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9') {
			j := i
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
				j++
			}
			tokens = append(tokens, src[i:j])
			i = j
			continue
		}
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			j := i
			for j < len(src) && ((src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			tokens = append(tokens, src[i:j])
			i = j
			continue
		}
		i++
	}
	return tokens
}

type gqlParser struct {
	pos    int
	tokens []string
	vars   map[string]any
}

func (p *gqlParser) peek() (string, bool) {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos], true
	}
	return "", false
}

func (p *gqlParser) next() string {
	if p.pos >= len(p.tokens) {
		return ""
	}
	t := p.tokens[p.pos]
	p.pos++
	return t
}

func (p *gqlParser) expect(t string) error {
	if p.next() != t {
		return &gqlError{"expected " + t}
	}
	return nil
}

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

func (p *gqlParser) parseOperation() (*gqlOp, error) {
	kind := "query"
	var name string
	t, _ := p.peek()
	if t == "query" || t == "mutation" {
		kind = p.next()
		n, ok := p.peek()
		if ok && n != "{" && n != "(" {
			name = p.next()
		}
	}
	if t, ok := p.peek(); ok && t == "(" {
		p.next()
		for {
			peek, ok := p.peek()
			if !ok || peek == ")" {
				break
			}
			if err := p.expect("$"); err != nil {
				return nil, err
			}
			p.next() // var name
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			for {
				peek, ok := p.peek()
				if !ok || peek == ")" || peek == "$" || peek == "=" || peek == "," {
					break
				}
				p.next()
			}
			if peek, ok := p.peek(); ok && peek == "=" {
				p.next()
				if _, err := p.parseValue(); err != nil {
					return nil, err
				}
			}
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlOp{kind: kind, name: name, selections: sels}, nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var fields []*gqlField
	for {
		peek, ok := p.peek()
		if !ok || peek == "}" {
			break
		}
		if peek == "..." {
			p.next()
			if n, _ := p.peek(); n == "on" {
				p.next()
				typeName := p.next()
				sub, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				fields = append(fields, &gqlField{name: "...on:" + typeName, selections: sub, args: map[string]any{}})
				continue
			}
			fragName := p.next()
			fields = append(fields, &gqlField{name: "..." + fragName, args: map[string]any{}})
			continue
		}
		name := p.next()
		alias := ""
		if peek, _ := p.peek(); peek == ":" {
			p.next()
			alias = name
			name = p.next()
		}
		args := map[string]any{}
		if peek, _ := p.peek(); peek == "(" {
			p.next()
			for {
				peek, ok := p.peek()
				if !ok || peek == ")" {
					break
				}
				argName := p.next()
				if err := p.expect(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				args[argName] = v
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
		}
		var sels []*gqlField
		if peek, _ := p.peek(); peek == "{" {
			var err error
			sels, err = p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
		}
		fields = append(fields, &gqlField{name: name, alias: alias, args: args, selections: sels})
	}
	if err := p.expect("}"); err != nil {
		return nil, err
	}
	return fields, nil
}

func (p *gqlParser) parseValue() (any, error) {
	t := p.next()
	switch {
	case t == "$":
		v, ok := p.vars[p.next()]
		if !ok || v == nil {
			return nil, nil
		}
		return v, nil
	case t == "{":
		obj := map[string]any{}
		for {
			peek, ok := p.peek()
			if !ok || peek == "}" {
				break
			}
			k := p.next()
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			obj[k] = v
		}
		if err := p.expect("}"); err != nil {
			return nil, err
		}
		return obj, nil
	case t == "[":
		var arr []any
		for {
			peek, ok := p.peek()
			if !ok || peek == "]" {
				break
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		return arr, nil
	case t == "true":
		return true, nil
	case t == "false":
		return false, nil
	case t == "null":
		return nil, nil
	case strings.HasPrefix(t, "\""):
		var s string
		if err := json.Unmarshal([]byte(t), &s); err != nil {
			return nil, err
		}
		return s, nil
	default:
		if n, err := strconv.ParseFloat(t, 64); err == nil {
			return n, nil
		}
		return t, nil
	}
}

// ------------------------------------------------------------------ resolvers

func gqlState(i *issue) string {
	if i.state == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func gqlPrState(p *pull) string {
	if p.merged {
		return "MERGED"
	}
	if p.state == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func (s *server) resolveIssueFields(r *repo, iss *issue, fields []*gqlField, errors *[]string) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		switch f.name {
		case "id":
			out[key] = gid("issue", viewerLogin+"/"+r.name+":"+strconv.Itoa(iss.number))
		case "number":
			out[key] = iss.number
		case "title":
			out[key] = iss.title
		case "state":
			out[key] = gqlState(iss)
		case "body":
			out[key] = iss.body
		case "url":
			out[key] = ""
		case "createdAt":
			out[key] = iss.createdAt
		case "updatedAt":
			out[key] = iss.updatedAt
		default:
			if !gqlContains(gqlIssueFields, f.name) {
				*errors = append(*errors, "Cannot query field \""+f.name+"\" on type \"Issue\".")
			} else {
				out[key] = nil
			}
		}
	}
	return out
}

func (s *server) resolvePrFields(r *repo, pr *pull, fields []*gqlField, errors *[]string) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		switch f.name {
		case "id":
			out[key] = gid("pr", viewerLogin+"/"+r.name+":"+strconv.Itoa(pr.number))
		case "number":
			out[key] = pr.number
		case "title":
			out[key] = pr.title
		case "state":
			out[key] = gqlPrState(pr)
		case "merged":
			out[key] = pr.merged
		case "headRefName":
			out[key] = pr.headBranch
		case "baseRefName":
			out[key] = pr.baseBranch
		case "mergeable":
			out[key] = s.st.prMergeable(r, pr)
		case "files":
			first := 100
			if v, ok := f.args["first"].(float64); ok {
				first = int(v)
			}
			files := s.st.prFiles(r, pr)
			if len(files) > first {
				files = files[:first]
			}
			nodes := []map[string]any{}
			for _, fl := range files {
				nodes = append(nodes, map[string]any{"path": fl.filename})
			}
			out[key] = map[string]any{"nodes": nodes, "totalCount": len(files)}
		default:
			if !gqlContains(gqlPrFields, f.name) {
				*errors = append(*errors, "Cannot query field \""+f.name+"\" on type \"PullRequest\".")
			} else {
				out[key] = nil
			}
		}
	}
	return out
}

func (s *server) resolveRepoFields(r *repo, fields []*gqlField, errors *[]string) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		switch f.name {
		case "id":
			out[key] = gid("repo", viewerLogin+"/"+r.name)
		case "databaseId":
			out[key] = r.id
		case "name":
			out[key] = r.name
		case "nameWithOwner":
			out[key] = viewerLogin + "/" + r.name
		case "owner":
			out[key] = map[string]any{"login": viewerLogin, "id": viewerID}
		case "defaultBranchRef":
			if sha, ok := r.refs["refs/heads/"+r.defaultBranch]; ok {
				_ = sha
				out[key] = map[string]any{"name": r.defaultBranch}
			} else {
				out[key] = nil
			}
		case "viewerPermission":
			out[key] = "ADMIN"
		case "hasIssuesEnabled":
			out[key] = true
		case "issue":
			num := 0
			if v, ok := f.args["number"].(float64); ok {
				num = int(v)
			}
			iss := r.issues[num]
			if iss != nil {
				out[key] = s.resolveIssueFields(r, iss, f.selections, errors)
			} else {
				out[key] = nil
			}
		case "issues":
			list := append([]*issue{}, r.issuesOrder...)
			if states, ok := f.args["states"].([]any); ok {
				var filtered []*issue
				for _, iss := range list {
					for _, st := range states {
						if stStr, ok := st.(string); ok && stStr == gqlState(iss) {
							filtered = append(filtered, iss)
							break
						}
					}
				}
				list = filtered
			}
			if orderBy, ok := f.args["orderBy"].(map[string]any); ok {
				if field, _ := orderBy["field"].(string); field == "CREATED_AT" {
					desc := true
					if dir, _ := orderBy["direction"].(string); dir == "ASC" {
						desc = false
					}
					sort.SliceStable(list, func(a, b int) bool {
						if desc {
							if list[a].seq != list[b].seq {
								return list[a].seq > list[b].seq
							}
							return list[a].number > list[b].number
						}
						if list[a].seq != list[b].seq {
							return list[a].seq < list[b].seq
						}
						return list[a].number < list[b].number
					})
				} else {
					sort.SliceStable(list, func(a, b int) bool { return list[a].number < list[b].number })
				}
			} else {
				sort.SliceStable(list, func(a, b int) bool { return list[a].number < list[b].number })
			}
			if after, ok := f.args["after"].(string); ok {
				if n := decodeCursor(after); n >= 0 {
					idx := -1
					for i, iss := range list {
						if iss.number == n {
							idx = i
							break
						}
					}
					if idx >= 0 {
						list = list[idx+1:]
					}
				}
			}
			first := len(list)
			if v, ok := f.args["first"].(float64); ok {
				first = int(v)
			}
			page := list
			if len(page) > first {
				page = page[:first]
			}
			var nodeSel []*gqlField
			for _, sub := range f.selections {
				if sub.name == "nodes" {
					nodeSel = sub.selections
				}
			}
			nodes := []map[string]any{}
			for _, iss := range page {
				nodes = append(nodes, s.resolveIssueFields(r, iss, nodeSel, errors))
			}
			hasNext := len(list) > first
			var endCursor any
			if len(page) > 0 {
				endCursor = encodeCursor(page[len(page)-1].number)
			}
			out[key] = map[string]any{
				"nodes": nodes,
				"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": endCursor},
				"totalCount": len(list),
			}
		case "pullRequests":
			first := 100
			if v, ok := f.args["first"].(float64); ok {
				first = int(v)
			}
			list := append([]*pull{}, r.pullsOrder...)
			sort.SliceStable(list, func(a, b int) bool { return list[a].seq > list[b].seq })
			if len(list) > first {
				list = list[:first]
			}
			var nodeSel []*gqlField
			for _, sub := range f.selections {
				if sub.name == "nodes" {
					nodeSel = sub.selections
				}
			}
			nodes := []map[string]any{}
			for _, p := range list {
				nodes = append(nodes, s.resolvePrFields(r, p, nodeSel, errors))
			}
			out[key] = map[string]any{"nodes": nodes, "totalCount": len(r.pullsOrder)}
		case "pullRequest":
			num := 0
			if v, ok := f.args["number"].(float64); ok {
				num = int(v)
			}
			pr := r.pulls[num]
			if pr != nil {
				out[key] = s.resolvePrFields(r, pr, f.selections, errors)
			} else {
				out[key] = nil
			}
		default:
			if !gqlContains(gqlRepoFields, f.name) {
				*errors = append(*errors, "Cannot query field \""+f.name+"\" on type \"Repository\".")
			} else {
				out[key] = nil
			}
		}
	}
	return out
}

func encodeCursor(n int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(n)))
}

func decodeCursor(c string) int {
	raw, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return -1
	}
	m := cursorRe.FindStringSubmatch(string(raw))
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func gqlTypeFields(name string) []string {
	switch name {
	case "Issue":
		return gqlIssueFields
	case "PullRequest":
		return gqlPrFields
	case "Repository":
		return gqlRepoFields
	}
	return []string{}
}

func (s *server) executeGql(query string, variables map[string]any) (map[string]any, []map[string]any) {
	var errors []string
	parser := &gqlParser{tokens: gqlTokenize(query), vars: variables}
	op, err := parser.parseOperation()
	if err != nil {
		return nil, []map[string]any{{"message": err.Error()}}
	}
	data := map[string]any{}
	s.resolveTop(op, data, &errors)
	var errObjs []map[string]any
	for _, e := range errors {
		errObjs = append(errObjs, map[string]any{"message": e})
	}
	return data, errObjs
}

func (s *server) resolveTop(op *gqlOp, data map[string]any, errors *[]string) {
	for _, f := range op.selections {
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		if op.kind == "query" {
			switch f.name {
			case "viewer":
				v := map[string]any{}
				for _, sub := range f.selections {
					switch sub.name {
					case "login":
						v["login"] = viewerLogin
					case "id":
						v["id"] = viewerID
					case "name":
						v["name"] = viewerName
					case "email":
						v["email"] = viewerEmail
					case "avatarUrl", "url":
						v[sub.name] = ""
					default:
						*errors = append(*errors, "Cannot query field \""+sub.name+"\" on type \"User\".")
					}
				}
				if len(f.selections) == 0 {
					v["login"] = viewerLogin
					v["id"] = viewerID
				}
				data[key] = v
			case "repository":
				owner := jsStr(f.args["owner"])
				name := jsStr(f.args["name"])
				rp := s.st.getRepo(owner, name)
				if rp != nil {
					data[key] = s.resolveRepoFields(rp, f.selections, errors)
				} else {
					data[key] = nil
				}
			case "node":
				decodedKind, decodedKey, ok := decodeGid(jsStr(f.args["id"]))
				if ok && decodedKind == "repo" {
					parts := strings.SplitN(decodedKey, "/", 2)
					var rp *repo
					if len(parts) == 2 {
						rp = s.st.getRepo(parts[0], parts[1])
					}
					if rp != nil {
						obj := map[string]any{"id": jsStr(f.args["id"]), "__typename": "Repository"}
						for _, sub := range f.selections {
							if strings.HasPrefix(sub.name, "...on:") {
								if sub.name == "...on:Repository" {
									for k, v := range s.resolveRepoFields(rp, sub.selections, errors) {
										obj[k] = v
									}
								}
							} else if sub.name == "id" {
								obj["id"] = gid("repo", viewerLogin+"/"+rp.name)
							} else if sub.name == "name" {
								obj["name"] = rp.name
							} else if sub.name == "nameWithOwner" {
								obj["nameWithOwner"] = viewerLogin + "/" + rp.name
							} else {
								*errors = append(*errors, "Cannot query field \""+sub.name+"\" on type \"Node\".")
							}
						}
						data[key] = obj
					} else {
						data[key] = nil
					}
				} else {
					data[key] = nil
				}
			case "__type":
				name := jsStr(f.args["name"])
				out := map[string]any{"name": name}
				for _, sub := range f.selections {
					if sub.name == "fields" {
						fields := []map[string]any{}
						for _, n := range gqlTypeFields(name) {
							fields = append(fields, map[string]any{"name": n})
						}
						out["fields"] = fields
					} else if sub.name == "enumValues" {
						vals := []map[string]any{}
						if name == "SearchType" {
							for _, n := range gqlSearchEnum {
								vals = append(vals, map[string]any{"name": n})
							}
						}
						out["enumValues"] = vals
					}
				}
				data[key] = out
			default:
				*errors = append(*errors, "Cannot query field \""+f.name+"\" on type \"Query\".")
				data[key] = nil
			}
		} else {
			// mutations
			switch f.name {
			case "createIssue":
				input, _ := f.args["input"].(map[string]any)
				dkind, dkey, dok := decodeGid(jsStr(getField(input, "repositoryId")))
				var rp *repo
				if dok && dkind == "repo" {
					parts := strings.SplitN(dkey, "/", 2)
					if len(parts) == 2 {
						rp = s.st.getRepo(parts[0], parts[1])
					}
				}
				if rp == nil {
					*errors = append(*errors, "Repository not found")
					data[key] = map[string]any{"issue": nil}
					break
				}
				number := rp.nextNumber
				rp.nextNumber++
				sec := tick()
				iss := &issue{
					id: nextEntityID(), number: number, title: jsStr(getField(input, "title")),
					body: jsStr(getField(input, "body")), state: "open", labels: []string{},
					seq: number, createdAt: isoOf(sec), updatedAt: isoOf(sec), comments: []*comment{},
					nodeID: gid("issue", viewerLogin+"/"+rp.name+":"+strconv.Itoa(number)),
				}
				rp.issues[number] = iss
				rp.issuesOrder = append(rp.issuesOrder, iss)
				var sel []*gqlField
				for _, sub := range f.selections {
					if sub.name == "issue" {
						sel = sub.selections
					}
				}
				data[key] = map[string]any{"issue": s.resolveIssueFields(rp, iss, sel, errors)}
			case "closeIssue":
				input, _ := f.args["input"].(map[string]any)
				dkind, dkey, dok := decodeGid(jsStr(getField(input, "issueId")))
				var found *issue
				var foundRepo *repo
				if dok && dkind == "issue" {
					m := gidRe.FindStringSubmatch(dkey)
					if m != nil {
						rp := s.st.getRepo(m[1], m[2])
						if rp != nil {
							if iss := rp.issues[atoi(m[3])]; iss != nil {
								found, foundRepo = iss, rp
							}
						}
					}
				}
				if found == nil {
					*errors = append(*errors, "Issue not found")
					data[key] = map[string]any{"issue": nil}
					break
				}
				found.state = "closed"
				found.updatedAt = nowIso()
				found.invalidateJSON()
				var sel []*gqlField
				for _, sub := range f.selections {
					if sub.name == "issue" {
						sel = sub.selections
					}
				}
				data[key] = map[string]any{"issue": s.resolveIssueFields(foundRepo, found, sel, errors)}
			case "createPullRequest":
				input, _ := f.args["input"].(map[string]any)
				dkind, dkey, dok := decodeGid(jsStr(getField(input, "repositoryId")))
				var rp *repo
				if dok && dkind == "repo" {
					parts := strings.SplitN(dkey, "/", 2)
					if len(parts) == 2 {
						rp = s.st.getRepo(parts[0], parts[1])
					}
				}
				if rp == nil {
					*errors = append(*errors, "Repository not found")
					data[key] = map[string]any{"pullRequest": nil}
					break
				}
				headBranch := jsStr(getField(input, "headRefName"))
				baseBranch := jsStrDef(getField(input, "baseRefName"), rp.defaultBranch)
				headSha, hasHead := rp.refs["refs/heads/"+headBranch]
				baseSha, hasBase := rp.refs["refs/heads/"+baseBranch]
				if !hasHead || !hasBase {
					*errors = append(*errors, "Head or base branch not found")
					data[key] = map[string]any{"pullRequest": nil}
					break
				}
				var pr *pull
				for _, p := range rp.pullsOrder {
					if p.state == "open" && p.headBranch == headBranch && p.baseBranch == baseBranch {
						pr = p
						break
					}
				}
				if pr == nil {
					number := rp.nextNumber
					rp.nextNumber++
					sec := tick()
					pr = &pull{
						id: nextEntityID(), number: number, title: jsStr(getField(input, "title")),
						body: jsStr(getField(input, "body")), state: "open", merged: false,
						headBranch: headBranch, baseBranch: baseBranch, headSha: headSha, baseSha: baseSha,
						seq: number, createdAt: isoOf(sec),
					}
					rp.pulls[number] = pr
					rp.pullsOrder = append(rp.pullsOrder, pr)
				}
				var sel []*gqlField
				for _, sub := range f.selections {
					if sub.name == "pullRequest" {
						sel = sub.selections
					}
				}
				data[key] = map[string]any{"pullRequest": s.resolvePrFields(rp, pr, sel, errors)}
			case "mergePullRequest":
				input, _ := f.args["input"].(map[string]any)
				dkind, dkey, dok := decodeGid(jsStr(getField(input, "pullRequestId")))
				var found *pull
				var foundRepo *repo
				if dok && dkind == "pr" {
					m := gidRe.FindStringSubmatch(dkey)
					if m != nil {
						rp := s.st.getRepo(m[1], m[2])
						if rp != nil {
							if pr := rp.pulls[atoi(m[3])]; pr != nil {
								found, foundRepo = pr, rp
							}
						}
					}
				}
				if found == nil {
					*errors = append(*errors, "Pull request not found")
					data[key] = map[string]any{"pullRequest": nil}
					break
				}
				rp, pr := foundRepo, found
				if pr.state == "open" && !s.st.prConflicts(rp, pr) {
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
				}
				var sel []*gqlField
				for _, sub := range f.selections {
					if sub.name == "pullRequest" {
						sel = sub.selections
					}
				}
				data[key] = map[string]any{"pullRequest": s.resolvePrFields(rp, pr, sel, errors)}
			case "addComment":
				input, _ := f.args["input"].(map[string]any)
				dkind, dkey, dok := decodeGid(jsStr(getField(input, "subjectId")))
				var found *issue
				var foundRepo *repo
				if dok && dkind == "issue" {
					m := gidRe.FindStringSubmatch(dkey)
					if m != nil {
						rp := s.st.getRepo(m[1], m[2])
						if rp != nil {
							if iss := rp.issues[atoi(m[3])]; iss != nil {
								found, foundRepo = iss, rp
							}
						}
					}
				}
				if found == nil {
					*errors = append(*errors, "Subject not found")
					data[key] = map[string]any{"commentEdge": nil}
					break
				}
				c := &comment{id: nextEntityID(), body: jsStr(getField(input, "body")), createdAt: nowIso()}
				found.comments = append(found.comments, c)
				found.invalidateJSON()
				data[key] = map[string]any{"commentEdge": map[string]any{"node": map[string]any{
					"id":   gid("comment", viewerLogin+"/"+foundRepo.name+":"+strconv.Itoa(found.number)+":"+strconv.FormatInt(c.id, 10)),
					"body": c.body,
				}}}
			default:
				*errors = append(*errors, "Cannot query field \""+f.name+"\" on type \"Mutation\".")
				data[key] = nil
			}
		}
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (s *server) handleGraphql(w *fastResp, r *fastReq, body []byte) {
	if !isAuthed(r) {
		s.sendErr(w, 401, "Bad credentials", nil)
		return
	}
	parsed, ok := parseJSONBody(body)
	if !ok {
		s.sendJSON(w, 400, map[string]any{"errors": []map[string]any{{"message": "Problems parsing JSON"}}}, nil)
		return
	}
	runOne := func(op any) map[string]any {
		m, isMap := op.(map[string]any)
		if !isMap {
			return map[string]any{"data": nil, "errors": []map[string]any{{"message": "Invalid operation"}}}
		}
		query := jsStr(m["query"])
		variables := map[string]any{}
		if v, isMap := m["variables"].(map[string]any); isMap {
			variables = v
		}
		s.st.mu.Lock()
		data, errs := s.executeGql(query, variables)
		s.st.mu.Unlock()
		out := map[string]any{"data": data}
		if len(errs) > 0 {
			out["errors"] = errs
		}
		return out
	}
	if arr, isArr := parsed.([]any); isArr {
		results := make([]map[string]any, 0, len(arr))
		for _, op := range arr {
			results = append(results, runOne(op))
		}
		s.sendJSON(w, 200, results, nil)
		return
	}
	s.sendJSON(w, 200, runOne(parsed), nil)
}
