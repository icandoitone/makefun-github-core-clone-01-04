// Model Context Protocol (streamable HTTP) at POST /mcp, modern 2026-07-28
// revision with a legacy initialize fallback. JSON-RPC 2.0 over plain JSON.
package main

import (
	"bytes"
	"strconv"
	"strings"
)

const mcpServerName = "forge"
const mcpServerVersion = "1.0.0"
const mcpProtocolVersion = "2026-07-28"

// The tools/list answer, a constant so two calls are byte-identical.
const mcpToolsJSON = `{"tools":[` +
	`{"name":"create_repo","description":"Create a repository for the authenticated user.","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"The repository name."},"description":{"type":"string","description":"A short description of the repository."},"private":{"type":"boolean","description":"Whether the repository is private."}},"required":["name"]}},` +
	`{"name":"get_repo","description":"Get a repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The repository owner."},"repo":{"type":"string","description":"The repository name."}},"required":["owner","repo"]}},` +
	`{"name":"create_issue","description":"Create an issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The repository owner."},"repo":{"type":"string","description":"The repository name."},"title":{"type":"string","description":"The issue title."},"body":{"type":"string","description":"The issue body."}},"required":["owner","repo","title"]}},` +
	`{"name":"list_issues","description":"List issues in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The repository owner."},"repo":{"type":"string","description":"The repository name."},"state":{"type":"string","description":"The issue state: open, closed, or all."}},"required":["owner","repo"]}},` +
	`{"name":"create_pull_request","description":"Create a pull request.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The repository owner."},"repo":{"type":"string","description":"The repository name."},"title":{"type":"string","description":"The pull request title."},"body":{"type":"string","description":"The pull request body."},"head":{"type":"string","description":"The head branch."},"base":{"type":"string","description":"The base branch."}},"required":["owner","repo","title","head","base"]}},` +
	`{"name":"merge_pull_request","description":"Merge a pull request with a squash merge.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The repository owner."},"repo":{"type":"string","description":"The repository name."},"pull_number":{"type":"integer","description":"The pull request number."}},"required":["owner","repo","pull_number"]}}` +
	`]}`

func handleMCP(s *Server, req *Request) *Response {
	if req.Method == "DELETE" {
		// A legacy session close: best-effort, no body.
		return &Response{Status: 200, ContentType: "application/json", Body: []byte("{}")}
	}
	if req.Method != "POST" {
		return jsonError(405, "Method Not Allowed")
	}
	if !s.requireAuth(req) {
		return unauthorized()
	}
	body, ok := parseJSON(req.Body)
	if !ok {
		// Parse error: HTTP 400 with JSON-RPC -32700.
		return &Response{Status: 400, ContentType: "application/json", Body: []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)}
	}
	m, ok := body.(map[string]any)
	if !ok {
		return &Response{Status: 400, ContentType: "application/json", Body: []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request"}}`)}
	}
	method, _ := jsonString(jsonGet(m, "method"))
	idVal := jsonGet(m, "id")
	hasID := idVal != nil
	params, _ := jsonGet(m, "params").(map[string]any)

	// A notification (no id) is answered with 202 and no body.
	if !hasID {
		if method == "notifications/initialized" || method == "notifications/cancelled" {
			return &Response{Status: 202, ContentType: "application/json", Body: nil}
		}
		return &Response{Status: 202, ContentType: "application/json", Body: nil}
	}

	var result []byte
	var rpcErr *rpcError
	switch method {
	case "server/discover":
		result = mcpDiscoverResult()
	case "initialize":
		result = mcpInitializeResult()
	case "tools/list":
		result = []byte(mcpToolsJSON)
	case "tools/call":
		result, rpcErr = mcpToolsCall(s, params)
	case "ping":
		result = []byte("{}")
	default:
		rpcErr = &rpcError{code: -32601, message: "Method not found: " + method}
	}
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":`)
	marshalJSONValue(idVal, &buf)
	if rpcErr != nil {
		buf.WriteString(`,"error":{"code":`)
		buf.WriteString(strconv.Itoa(rpcErr.code))
		buf.WriteString(`,"message":`)
		escapeJSONString(&buf, rpcErr.message)
		buf.WriteString(`}}`)
	} else {
		buf.WriteString(`,"result":`)
		buf.Write(result)
		buf.WriteByte('}')
	}
	return &Response{Status: 200, ContentType: "application/json", Body: buf.Bytes()}
}

type rpcError struct {
	code    int
	message string
}

func mcpDiscoverResult() []byte {
	return []byte(`{"protocolVersion":"` + mcpProtocolVersion + `","supportedVersions":["2026-07-28","2025-11-25","2025-06-18","2025-03-26"],"capabilities":{"tools":{}},"serverInfo":{"name":"` + mcpServerName + `","version":"` + mcpServerVersion + `"}}`)
}

func mcpInitializeResult() []byte {
	return []byte(`{"protocolVersion":"` + mcpProtocolVersion + `","capabilities":{"tools":{}},"serverInfo":{"name":"` + mcpServerName + `","version":"` + mcpServerVersion + `"}}`)
}

// mcpToolResult wraps a tool's JSON text in the MCP content shape.
func mcpToolResult(text string, isError bool) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"content":[{"type":"text","text":`)
	escapeJSONString(&buf, text)
	buf.WriteString(`}],"isError":`)
	if isError {
		buf.WriteString("true")
	} else {
		buf.WriteString("false")
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

func mcpToolError(msg string) []byte {
	return mcpToolResult(msg, true)
}

func mcpToolsCall(s *Server, params map[string]any) ([]byte, *rpcError) {
	name, _ := jsonString(jsonGet(params, "name"))
	args, _ := jsonGet(params, "arguments").(map[string]any)
	if args == nil {
		args = make(map[string]any)
	}
	switch name {
	case "create_repo":
		return mcpCreateRepo(s, args), nil
	case "get_repo":
		return mcpGetRepo(s, args), nil
	case "create_issue":
		return mcpCreateIssue(s, args), nil
	case "list_issues":
		return mcpListIssues(s, args), nil
	case "create_pull_request":
		return mcpCreatePull(s, args), nil
	case "merge_pull_request":
		return mcpMergePull(s, args), nil
	case "":
		return nil, &rpcError{code: -32602, message: "Invalid params: tool name is required"}
	default:
		return nil, &rpcError{code: -32601, message: "Method not found: " + name}
	}
}

func mcpArgString(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func mcpArgInt(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(t)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func mcpArgBool(args map[string]any, key string, def bool) bool {
	v, ok := args[key]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

// mcpRepo resolves the owner/repo arguments to a repository.
func mcpRepo(s *Server, args map[string]any) (*Repo, string) {
	owner, _ := mcpArgString(args, "owner")
	name, _ := mcpArgString(args, "repo")
	if name == "" {
		// Some clients send a single full_name.
		if full, ok := mcpArgString(args, "full_name"); ok {
			parts := strings.SplitN(full, "/", 2)
			if len(parts) == 2 {
				owner, name = parts[0], parts[1]
			}
		}
	}
	if name == "" {
		return nil, "repo is required"
	}
	if owner == "" {
		owner = viewerUser.Login
	}
	r := s.store.getRepo(owner, name)
	if r == nil {
		return nil, "repository not found: " + owner + "/" + name
	}
	return r, ""
}

func mcpCreateRepo(s *Server, args map[string]any) []byte {
	name, ok := mcpArgString(args, "name")
	if !ok || name == "" {
		return mcpToolError("name is required")
	}
	desc, _ := mcpArgString(args, "description")
	private := mcpArgBool(args, "private", true)
	r := s.store.createRepo(name, desc, private)
	if r == nil {
		return mcpToolError("repository already exists: " + name)
	}
	return mcpToolResult(string(r.cachedRepoJSON()), false)
}

func mcpGetRepo(s *Server, args map[string]any) []byte {
	r, errMsg := mcpRepo(s, args)
	if r == nil {
		return mcpToolError(errMsg)
	}
	return mcpToolResult(string(r.cachedRepoJSON()), false)
}

func mcpCreateIssue(s *Server, args map[string]any) []byte {
	r, errMsg := mcpRepo(s, args)
	if r == nil {
		return mcpToolError(errMsg)
	}
	title, ok := mcpArgString(args, "title")
	if !ok || title == "" {
		return mcpToolError("title is required")
	}
	body, _ := mcpArgString(args, "body")
	seq := s.store.nextSeq()
	iss := r.createIssue(title, body, nil, seq)
	return mcpToolResult(string(serializeIssue(r, iss)), false)
}

func mcpListIssues(s *Server, args map[string]any) []byte {
	r, errMsg := mcpRepo(s, args)
	if r == nil {
		return mcpToolError(errMsg)
	}
	state, _ := mcpArgString(args, "state")
	if state == "" {
		state = "open"
	}
	issues := r.listIssues(state, "desc")
	return mcpToolResult(string(serializeIssueList(r, issues)), false)
}

func mcpCreatePull(s *Server, args map[string]any) []byte {
	r, errMsg := mcpRepo(s, args)
	if r == nil {
		return mcpToolError(errMsg)
	}
	title, ok := mcpArgString(args, "title")
	if !ok || title == "" {
		return mcpToolError("title is required")
	}
	head, ok := mcpArgString(args, "head")
	if !ok || head == "" {
		return mcpToolError("head is required")
	}
	base, ok := mcpArgString(args, "base")
	if !ok || base == "" {
		return mcpToolError("base is required")
	}
	body, _ := mcpArgString(args, "body")
	headSHA, ok := r.getRef("refs/heads/" + head)
	if !ok {
		return mcpToolError("head branch does not exist: " + head)
	}
	baseSHA, ok := r.getRef("refs/heads/" + base)
	if !ok {
		return mcpToolError("base branch does not exist: " + base)
	}
	mb := r.mergeBase(headSHA, baseSHA)
	var changed []string
	if mb != "" {
		mbTree, _ := r.commitTree(mb)
		headTree, _ := r.commitTree(headSHA)
		for _, d := range r.diffTrees(mbTree, headTree) {
			changed = append(changed, d.Path)
		}
	}
	seq := s.store.nextSeq()
	p := r.createPull(title, body, head, base, headSHA, baseSHA, changed, seq)
	return mcpToolResult(string(serializePull(r, p)), false)
}

func mcpMergePull(s *Server, args map[string]any) []byte {
	r, errMsg := mcpRepo(s, args)
	if r == nil {
		return mcpToolError(errMsg)
	}
	num, ok := mcpArgInt(args, "pull_number")
	if !ok {
		return mcpToolError("pull_number is required")
	}
	p := r.getPull(num)
	if p == nil {
		return mcpToolError("pull request not found")
	}
	if p.Merged {
		return mcpToolError("pull request is already merged")
	}
	sha, status := mergePullRequestInternal(s.store, r, p)
	if status == "conflict" {
		return mcpToolError("merge conflict")
	}
	if status != "ok" {
		return mcpToolError("merge failed: " + status)
	}
	var buf bytes.Buffer
	buf.WriteString(`{"number":`)
	buf.WriteString(strconv.Itoa(num))
	buf.WriteString(`,"sha":`)
	escapeJSONString(&buf, sha)
	buf.WriteString(`,"merged":true,"state":"closed"}`)
	return mcpToolResult(buf.String(), false)
}
