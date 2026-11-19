// MCP over streamable HTTP.

package main

import (
	"net/http"
	"strconv"
	"strings"
)

var mcpTools = []map[string]any{
	{"name": "create_repo", "description": "Create a new repository for the authenticated user.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string", "description": "Repository name"}, "description": map[string]any{"type": "string", "description": "Repository description"}, "private": map[string]any{"type": "boolean", "description": "Whether the repository is private"}}, "required": []string{"name"}}},
	{"name": "get_repo", "description": "Get a repository by owner and name.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string", "description": "Repository owner login"}, "repo": map[string]any{"type": "string", "description": "Repository name"}, "full_name": map[string]any{"type": "string", "description": "Full repository name owner/repo"}}, "required": []string{"owner", "repo"}}},
	{"name": "create_issue", "description": "Create an issue in a repository.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string", "description": "Repository owner login"}, "repo": map[string]any{"type": "string", "description": "Repository name"}, "title": map[string]any{"type": "string", "description": "Issue title"}, "body": map[string]any{"type": "string", "description": "Issue body"}}, "required": []string{"owner", "repo", "title"}}},
	{"name": "list_issues", "description": "List issues in a repository.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string", "description": "Repository owner login"}, "repo": map[string]any{"type": "string", "description": "Repository name"}, "state": map[string]any{"type": "string", "description": "Issue state: open, closed or all"}}, "required": []string{"owner", "repo"}}},
	{"name": "create_pull_request", "description": "Create a pull request from a head branch into a base branch.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string", "description": "Repository owner login"}, "repo": map[string]any{"type": "string", "description": "Repository name"}, "title": map[string]any{"type": "string", "description": "Pull request title"}, "body": map[string]any{"type": "string", "description": "Pull request body"}, "head": map[string]any{"type": "string", "description": "Head branch name"}, "base": map[string]any{"type": "string", "description": "Base branch name"}}, "required": []string{"owner", "repo", "title", "head", "base"}}},
	{"name": "merge_pull_request", "description": "Merge a pull request by number.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string", "description": "Repository owner login"}, "repo": map[string]any{"type": "string", "description": "Repository name"}, "pull_number": map[string]any{"type": "number", "description": "Pull request number"}}, "required": []string{"owner", "repo", "pull_number"}}},
}

var mcpToolRequired = map[string][]string{
	"create_repo":         {"name"},
	"get_repo":            {"owner", "repo"},
	"create_issue":        {"owner", "repo", "title"},
	"list_issues":         {"owner", "repo"},
	"create_pull_request": {"owner", "repo", "title", "head", "base"},
	"merge_pull_request":  {"owner", "repo", "pull_number"},
}

func mcpTextResult(value any) map[string]any {
	text := string(marshalJSON(value))
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "structuredContent": value}
}

func mcpErrorResult(message string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": message}}, "isError": true}
}

func resolveOwnerRepo(args map[string]any) (owner, name string) {
	if v, ok := args["owner"].(string); ok {
		owner = v
	}
	if v, ok := args["repo"].(string); ok {
		name = v
	}
	if full, ok := args["full_name"].(string); ok && full != "" && (owner == "" || name == "") {
		if i := strings.Index(full, "/"); i >= 0 {
			if owner == "" {
				owner = full[:i]
			}
			if name == "" {
				name = full[i+1:]
			}
		}
	}
	if name == "" {
		if v, ok := args["name"].(string); ok {
			name = v
		}
	}
	return owner, name
}

func (s *server) mcpCallTool(name string, args map[string]any) map[string]any {
	required, ok := mcpToolRequired[name]
	if !ok {
		return mcpErrorResult("Unknown tool: " + name)
	}
	var missing []string
	for _, k := range required {
		v := args[k]
		if v == nil {
			missing = append(missing, k)
			continue
		}
		if str, isStr := v.(string); isStr && str == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return mcpErrorResult("Missing required argument: " + strings.Join(missing, ", "))
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	switch name {
	case "create_repo":
		repoName := jsStr(args["name"])
		if _, exists := s.st.repos[repoName]; exists {
			return mcpErrorResult("Repository already exists")
		}
		isPrivate, _ := args["private"].(bool)
		desc := ""
		if v, ok := args["description"].(string); ok {
			desc = v
		}
		rp := s.st.createRepoRecord(repoName, isPrivate, desc)
		return mcpTextResult(map[string]any{
			"id": rp.id, "name": rp.name, "full_name": viewerLogin + "/" + rp.name,
			"owner": map[string]any{"login": viewerLogin}, "private": rp.isPrivate,
			"default_branch": rp.defaultBranch, "description": rp.description,
		})
	case "get_repo":
		owner, rname := resolveOwnerRepo(args)
		rp := s.st.getRepo(owner, rname)
		if rp == nil {
			return mcpErrorResult("Repository not found")
		}
		return mcpTextResult(map[string]any{
			"id": rp.id, "name": rp.name, "full_name": owner + "/" + rp.name,
			"owner": map[string]any{"login": owner}, "private": rp.isPrivate,
			"default_branch": rp.defaultBranch, "description": rp.description,
		})
	case "create_issue":
		owner, rname := resolveOwnerRepo(args)
		rp := s.st.getRepo(owner, rname)
		if rp == nil {
			return mcpErrorResult("Repository not found")
		}
		number := rp.nextNumber
		rp.nextNumber++
		sec := tick()
		body := ""
		if v, ok := args["body"].(string); ok {
			body = v
		}
		iss := &issue{
			id: nextEntityID(), number: number, title: jsStr(args["title"]), body: body,
			state: "open", labels: []string{}, seq: number,
			createdAt: isoOf(sec), updatedAt: isoOf(sec), comments: []*comment{},
		}
		rp.issues[number] = iss
		rp.issuesOrder = append(rp.issuesOrder, iss)
		return mcpTextResult(map[string]any{
			"id": iss.id, "number": number, "title": iss.title, "body": iss.body,
			"state": "open", "repository": owner + "/" + rname,
		})
	case "list_issues":
		owner, rname := resolveOwnerRepo(args)
		rp := s.st.getRepo(owner, rname)
		if rp == nil {
			return mcpErrorResult("Repository not found")
		}
		state := "open"
		if v, ok := args["state"].(string); ok {
			state = v
		}
		var list []*issue
		for _, iss := range rp.issuesOrder {
			if state == "all" || iss.state == state {
				list = append(list, iss)
			}
		}
		// Node sorts by seq desc; issuesOrder is seq asc, so walk backwards.
		items := []map[string]any{}
		for i := len(list) - 1; i >= 0; i-- {
			iss := list[i]
			items = append(items, map[string]any{"id": iss.id, "number": iss.number, "title": iss.title, "body": iss.body, "state": iss.state})
		}
		return mcpTextResult(map[string]any{"issues": items})
	case "create_pull_request":
		owner, rname := resolveOwnerRepo(args)
		rp := s.st.getRepo(owner, rname)
		if rp == nil {
			return mcpErrorResult("Repository not found")
		}
		headBranch := jsStr(args["head"])
		baseBranch := jsStr(args["base"])
		headSha, hasHead := rp.refs["refs/heads/"+headBranch]
		baseSha, hasBase := rp.refs["refs/heads/"+baseBranch]
		if !hasHead || !hasBase {
			return mcpErrorResult("Head or base branch not found")
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
			body := ""
			if v, ok := args["body"].(string); ok {
				body = v
			}
			pr = &pull{
				id: nextEntityID(), number: number, title: jsStr(args["title"]), body: body,
				state: "open", merged: false,
				headBranch: headBranch, baseBranch: baseBranch, headSha: headSha, baseSha: baseSha,
				seq: number, createdAt: isoOf(sec),
			}
			rp.pulls[number] = pr
			rp.pullsOrder = append(rp.pullsOrder, pr)
		}
		return mcpTextResult(map[string]any{
			"id": pr.id, "number": pr.number, "title": pr.title, "state": "open",
			"head": map[string]any{"ref": pr.headBranch, "sha": pr.headSha},
			"base": map[string]any{"ref": pr.baseBranch, "sha": pr.baseSha},
		})
	case "merge_pull_request":
		owner, rname := resolveOwnerRepo(args)
		rp := s.st.getRepo(owner, rname)
		if rp == nil {
			return mcpErrorResult("Repository not found")
		}
		num := 0
		switch v := args["pull_number"].(type) {
		case float64:
			num = int(v)
		case string:
			num, _ = strconv.Atoi(v)
		}
		pr := rp.pulls[num]
		if pr == nil {
			return mcpErrorResult("Pull request not found")
		}
		if pr.state != "open" {
			return mcpErrorResult("Pull request is not open")
		}
		if s.st.prConflicts(rp, pr) {
			return mcpErrorResult("Merge conflict")
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
		return mcpTextResult(map[string]any{"number": pr.number, "merged": true, "sha": mergeSha, "state": "closed"})
	default:
		return mcpErrorResult("Unknown tool: " + name)
	}
}

func (s *server) handleMcp(w http.ResponseWriter, r *http.Request, body []byte) {
	sendRpc := func(status int, payload any) {
		s.sendJSON(w, status, payload, nil)
	}
	if r.Method == "DELETE" {
		sendRpc(200, map[string]any{})
		return
	}
	if !isAuthed(r) {
		sendRpc(401, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32001, "message": "Unauthorized"}})
		return
	}
	parsed, ok := parseJSONBody(body)
	if !ok {
		sendRpc(400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"}})
		return
	}
	msg, isMap := parsed.(map[string]any)
	if !isMap {
		sendRpc(400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32600, "message": "Invalid Request"}})
		return
	}
	var id any
	if v, present := msg["id"]; present {
		id = v
	} else {
		id = nil
	}
	method := jsStr(msg["method"])
	params := map[string]any{}
	if p, isMap := msg["params"].(map[string]any); isMap {
		params = p
	}
	if _, hasID := msg["id"]; !hasID {
		sendRpc(202, map[string]any{})
		return
	}
	switch method {
	case "server/discover":
		sendRpc(200, map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{
				"resultType": "discover",
				"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18"},
				"capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "github-core-clone", "version": "1.0.0"},
				"_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "github-core-clone", "version": "1.0.0"}},
			},
		})
	case "initialize":
		requested := jsStr(params["protocolVersion"])
		if requested == "" {
			requested = "2025-11-25"
		}
		negotiated := "2025-11-25"
		for _, v := range []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"} {
			if v == requested {
				negotiated = v
				break
			}
		}
		sendRpc(200, map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{
				"protocolVersion": negotiated,
				"capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "github-core-clone", "version": "1.0.0"},
			},
		})
	case "tools/list":
		sendRpc(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"tools": mcpTools}})
	case "tools/call":
		toolName := jsStr(params["name"])
		args := map[string]any{}
		if a, isMap := params["arguments"].(map[string]any); isMap {
			args = a
		}
		known := false
		for _, t := range mcpTools {
			if t["name"] == toolName {
				known = true
				break
			}
		}
		if !known {
			sendRpc(200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32602, "message": "Unknown tool: " + toolName}})
			return
		}
		result := s.mcpCallTool(toolName, args)
		sendRpc(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	case "ping":
		sendRpc(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
	default:
		sendRpc(200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "Method not found: " + method}})
	}
}
