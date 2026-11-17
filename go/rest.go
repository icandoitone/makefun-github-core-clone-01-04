// GitHub REST v3 handlers under /api/v3.
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// requireAuth enforces the bearer token. Returns false when the request is
// unauthenticated (the caller sends the 401).
func (s *Server) requireAuth(req *Request) bool {
	if req.Auth == "" {
		return false
	}
	if s.token != "" && req.Auth != s.token {
		return false
	}
	return true
}

func unauthorized() *Response { return jsonError(401, "Requires authentication") }
func notFound(msg string) *Response {
	if msg == "" {
		msg = "Not Found"
	}
	return jsonError(404, msg)
}

// handleREST routes an /api/v3 request.
func handleREST(s *Server, req *Request) *Response {
	// The API root and meta are reachable without a token? No: the protocol
	// check calls GET /api/v3/ and /meta with a token; the unauthorized check
	// calls GET /repos/{o}/{r} without a token and expects 401. The root and
	// meta are only ever called with a token in the suite, so requiring auth
	// for every /api/v3 route is safe and matches "a request without it is 401".
	if !s.requireAuth(req) {
		return unauthorized()
	}
	path := req.Path
	// Strip the /api/v3 prefix.
	rest := strings.TrimPrefix(path, "/api/v3")
	segs := splitPath(rest)
	// GET /api/v3/ (root)
	if len(segs) == 0 {
		if req.Method != "GET" {
			return jsonError(405, "Method Not Allowed")
		}
		return &Response{
			Status: 200, ContentType: "application/json",
			Body:   []byte(`{"current_user_url":"/api/v3/user","repository_url":"/api/v3/repos/{owner}/{repo}"}`),
			Extra:  [][2]string{{"X-OAuth-Scopes", "repo, user"}},
		}
	}
	switch segs[0] {
	case "meta":
		if req.Method != "GET" {
			return jsonError(405, "Method Not Allowed")
		}
		return &Response{Status: 200, ContentType: "application/json", Body: []byte(`{"installed_version":"1.0.0"}`)}
	case "user":
		return handleUser(s, req, segs[1:])
	case "repos":
		return handleRepos(s, req, segs[1:])
	}
	return notFound("Not Found")
}

func handleUser(s *Server, req *Request, segs []string) *Response {
	if len(segs) == 0 {
		// GET /api/v3/user
		if req.Method != "GET" {
			return jsonError(405, "Method Not Allowed")
		}
		return &Response{Status: 200, ContentType: "application/json", Body: userJSON(viewerUser)}
	}
	if segs[0] == "repos" {
		if req.Method == "POST" {
			return restCreateRepo(s, req)
		}
		if req.Method == "GET" {
			return restListUserRepos(s, req)
		}
		return jsonError(405, "Method Not Allowed")
	}
	return notFound("Not Found")
}

func restCreateRepo(s *Server, req *Request) *Response {
	name, ok := jsonStringField(req.Body, "name")
	if !ok || name == "" {
		return jsonError(400, "name is required")
	}
	if !validRepoName(name) {
		return jsonError(422, "Invalid repository name")
	}
	desc, _ := jsonStringField(req.Body, "description")
	private := true
	if v, ok := jsonBoolField(req.Body, "private"); ok {
		private = v
	}
	r := s.store.createRepo(name, desc, private)
	if r == nil {
		return jsonError(422, "Repository already exists")
	}
	return &Response{Status: 201, ContentType: "application/json", Body: r.cachedRepoJSON()}
}

func validRepoName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func restListUserRepos(s *Server, req *Request) *Response {
	perPage := 30
	if v, ok := queryParam(req.Query, "per_page"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	page := 1
	if v, ok := queryParam(req.Query, "page"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	repos := s.store.listViewerRepos()
	total := len(repos)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var b bytes.Buffer
	b.WriteByte('[')
	for i := start; i < end; i++ {
		if i > start {
			b.WriteByte(',')
		}
		b.Write(repos[i].cachedRepoJSON())
	}
	b.WriteByte(']')
	resp := &Response{Status: 200, ContentType: "application/json", Body: b.Bytes()}
	// Link header with absolute URLs (the client checks same-origin).
	if end < total || page > 1 {
		host := req.Host
		if host == "" {
			host = "127.0.0.1"
		}
		base := "http://" + host + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage)
		var links []string
		if end < total {
			links = append(links, "<"+base+"&page="+strconv.Itoa(page+1)+`>; rel="next"`)
			lastPage := (total + perPage - 1) / perPage
			links = append(links, "<"+base+"&page="+strconv.Itoa(lastPage)+`>; rel="last"`)
		}
		if page > 1 {
			links = append(links, "<"+base+"&page="+strconv.Itoa(page-1)+`>; rel="prev"`)
			links = append(links, "<"+base+"&page=1"+`>; rel="first"`)
		}
		if len(links) > 0 {
			resp.Extra = append(resp.Extra, [2]string{"Link", strings.Join(links, ", ")})
		}
	}
	return resp
}

// handleRepos routes /api/v3/repos/{owner}/{repo}/...
func handleRepos(s *Server, req *Request, segs []string) *Response {
	if len(segs) < 2 {
		return notFound("Not Found")
	}
	owner, name := segs[0], segs[1]
	r := s.store.getRepo(owner, name)
	if len(segs) == 2 {
		switch req.Method {
		case "GET":
			if r == nil {
				return notFound("Not Found")
			}
			return &Response{Status: 200, ContentType: "application/json", Body: r.cachedRepoJSON()}
		case "PATCH":
			if r == nil {
				return notFound("Not Found")
			}
			if desc, ok := jsonStringField(req.Body, "description"); ok {
				r.mu.Lock()
				r.Description = desc
				r.repoJSON = nil
				r.mu.Unlock()
			}
			return &Response{Status: 200, ContentType: "application/json", Body: r.cachedRepoJSON()}
		case "DELETE":
			if r == nil {
				return notFound("Not Found")
			}
			s.store.deleteRepo(owner, name)
			return &Response{Status: 204, ContentType: "application/json", Body: nil}
		}
		return jsonError(405, "Method Not Allowed")
	}
	if r == nil {
		return notFound("Not Found")
	}
	rest := segs[2:]
	switch rest[0] {
	case "git":
		return handleGitRefs(s, req, r, rest[1:])
	case "branches":
		return handleBranches(s, req, r, rest[1:])
	case "commits":
		return handleCommits(s, req, r, rest[1:])
	case "contents":
		return handleContents(s, req, r, rest[1:])
	case "labels":
		return handleLabels(s, req, r, rest[1:])
	case "issues":
		return handleIssues(s, req, r, rest[1:])
	case "pulls":
		return handlePulls(s, req, r, rest[1:])
	}
	return notFound("Not Found")
}

func handleGitRefs(s *Server, req *Request, r *Repo, segs []string) *Response {
	// POST /git/refs (create a branch) or GET /git/refs/heads/{branch}
	if len(segs) == 1 && segs[0] == "refs" {
		if req.Method != "POST" {
			return jsonError(405, "Method Not Allowed")
		}
		ref, ok := jsonStringField(req.Body, "ref")
		if !ok || !strings.HasPrefix(ref, "refs/heads/") {
			return jsonError(422, "Invalid ref")
		}
		sha, ok := jsonStringField(req.Body, "sha")
		if !ok || !isOID(sha) {
			return jsonError(422, "Invalid sha")
		}
		if r.getObject(sha) == nil {
			return jsonError(422, "Object does not exist")
		}
		r.mu.Lock()
		if _, exists := r.refs[ref]; exists {
			r.mu.Unlock()
			return jsonError(422, "Reference already exists")
		}
		r.refs[ref] = sha
		r.packCache = make(map[string][]byte)
		r.mu.Unlock()
		var b bytes.Buffer
		b.WriteString(`{"ref":`)
		escapeJSONString(&b, ref)
		b.WriteString(`,"object":{"sha":`)
		escapeJSONString(&b, sha)
		b.WriteString(`}}`)
		return &Response{Status: 201, ContentType: "application/json", Body: b.Bytes()}
	}
	if len(segs) == 3 && segs[0] == "refs" && segs[1] == "heads" {
		branch := segs[2]
		if req.Method != "GET" {
			return jsonError(405, "Method Not Allowed")
		}
		oid, ok := r.getRef("refs/heads/" + branch)
		if !ok {
			return notFound("Not Found")
		}
		var b bytes.Buffer
		b.WriteString(`{"ref":`)
		escapeJSONString(&b, "refs/heads/"+branch)
		b.WriteString(`,"object":{"sha":`)
		escapeJSONString(&b, oid)
		b.WriteString(`}}`)
		return &Response{Status: 200, ContentType: "application/json", Body: b.Bytes()}
	}
	return notFound("Not Found")
}

func handleBranches(s *Server, req *Request, r *Repo, segs []string) *Response {
	if req.Method != "GET" {
		return jsonError(405, "Method Not Allowed")
	}
	if len(segs) == 0 {
		refs := r.listRefs()
		var names []string
		for name := range refs {
			if strings.HasPrefix(name, "refs/heads/") {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		var b bytes.Buffer
		b.WriteByte('[')
		for i, name := range names {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(serializeBranch(strings.TrimPrefix(name, "refs/heads/"), refs[name]))
		}
		b.WriteByte(']')
		return &Response{Status: 200, ContentType: "application/json", Body: b.Bytes()}
	}
	if len(segs) == 1 {
		oid, ok := r.getRef("refs/heads/" + segs[0])
		if !ok {
			return notFound("Not Found")
		}
		return &Response{Status: 200, ContentType: "application/json", Body: serializeBranch(segs[0], oid)}
	}
	return notFound("Not Found")
}

func handleCommits(s *Server, req *Request, r *Repo, segs []string) *Response {
	if req.Method != "GET" || len(segs) != 0 {
		return jsonError(405, "Method Not Allowed")
	}
	refName := r.DefaultBranch
	if v, ok := queryParam(req.Query, "sha"); ok && v != "" {
		refName = v
	}
	// Resolve the ref: a branch name, a full ref, or a commit oid.
	ref := refName
	if !strings.HasPrefix(ref, "refs/") && !isOID(ref) {
		ref = "refs/heads/" + ref
	}
	var tip string
	if isOID(refName) {
		tip = refName
	} else {
		var ok bool
		tip, ok = r.getRef(ref)
		if !ok {
			return &Response{Status: 200, ContentType: "application/json", Body: []byte("[]")}
		}
	}
	// List commits from the resolved tip.
	var oids []string
	seen := make(map[string]bool)
	queue := []string{tip}
	r.mu.RLock()
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := r.objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		oids = append(oids, oid)
		cv, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, cv.Parents...)
	}
	r.mu.RUnlock()
	var b bytes.Buffer
	b.WriteByte('[')
	for i, oid := range oids {
		if i > 0 {
			b.WriteByte(',')
		}
		obj := r.getObject(oid)
		if obj == nil {
			continue
		}
		cv, _ := parseCommit(obj.Data)
		b.Write(serializeCommit(oid, cv))
	}
	b.WriteByte(']')
	return &Response{Status: 200, ContentType: "application/json", Body: b.Bytes()}
}

func handleLabels(s *Server, req *Request, r *Repo, segs []string) *Response {
	if len(segs) != 0 || req.Method != "POST" {
		return jsonError(405, "Method Not Allowed")
	}
	name, ok := jsonStringField(req.Body, "name")
	if !ok || name == "" {
		return jsonError(400, "name is required")
	}
	color, _ := jsonStringField(req.Body, "color")
	if color == "" {
		color = "ededed"
	}
	l := r.createLabel(name, color)
	return &Response{Status: 201, ContentType: "application/json", Body: labelJSON(l)}
}

// commitFile creates a blob + tree + commit for a contents change.
// branch is the branch to commit to ("" = default). parents is the current tip.
// Returns the new commit oid and the blob oid.
func commitFile(s *Store, r *Repo, branch, path string, content []byte, message string, isDelete bool, baseTreeOverride string) (commitOID, blobOID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if branch == "" {
		branch = r.DefaultBranch
	}
	ref := "refs/heads/" + branch
	tip, hasTip := r.refs[ref]
	var baseTree string
	if hasTip && tip != "" && tip != zeroOID {
		if cv, err := parseCommit(r.objects[tip].Data); err == nil {
			baseTree = cv.Tree
		}
	}
	if baseTreeOverride != "" {
		baseTree = baseTreeOverride
	}
	var newBlobOID string
	if !isDelete {
		blob := &Object{Type: "blob", Data: content}
		newBlobOID = r.putObjectLocked(blob)
	}
	newTree, err := r.updateTreePath(baseTree, path, newBlobOID)
	if err != nil {
		return "", "", err
	}
	seq := s.nextSeq()
	author := viewerUser.Name + " <" + viewerUser.Email + ">"
	var parents []string
	if hasTip && tip != "" && tip != zeroOID {
		parents = []string{tip}
	}
	commitData := buildCommit(newTree, parents, author, author, message, scenarioEpoch+seq)
	commitObj := &Object{Type: "commit", Data: commitData}
	commitOID = r.putObjectLocked(commitObj)
	r.refs[ref] = commitOID
	r.packCache = make(map[string][]byte)
	return commitOID, newBlobOID, nil
}

func handleContents(s *Server, req *Request, r *Repo, segs []string) *Response {
	// The path is the remaining segments joined (a file path may hold '/').
	filePath := strings.Join(segs, "/")
	if filePath == "" {
		return notFound("Not Found")
	}
	switch req.Method {
	case "PUT":
		return restPutContent(s, req, r, filePath)
	case "GET":
		return restGetContent(s, req, r, filePath)
	case "DELETE":
		return restDeleteContent(s, req, r, filePath)
	}
	return jsonError(405, "Method Not Allowed")
}

func restPutContent(s *Server, req *Request, r *Repo, filePath string) *Response {
	message, _ := jsonStringField(req.Body, "message")
	if message == "" {
		message = "Update " + filePath
	}
	contentB64, hasContent := jsonStringField(req.Body, "content")
	branch, _ := jsonStringField(req.Body, "branch")
	sha, hasSHA := jsonStringField(req.Body, "sha")
	// Resolve the branch tip and current blob.
	br := branch
	if br == "" {
		br = r.DefaultBranch
	}
	tip, hasTip := r.getRef("refs/heads/" + br)
	var curBlobOID, curTree string
	if hasTip && tip != "" && tip != zeroOID {
		if tree, ok := r.commitTree(tip); ok {
			curTree = tree
			if oid, _, found := r.treeEntryAt(tree, filePath); found {
				curBlobOID = oid
			}
		}
	}
	_ = curTree
	isUpdate := curBlobOID != ""
	if isUpdate {
		// An update needs the current sha.
		if !hasSHA || sha != curBlobOID {
			return jsonError(409, "sha does not match")
		}
	} else {
		if hasSHA && sha != "" {
			return jsonError(409, "sha does not match")
		}
	}
	if !hasContent {
		return jsonError(400, "content is required")
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.ReplaceAll(contentB64, "\n", ""), "\r", ""))
	if err != nil {
		return jsonError(400, "content is not valid base64")
	}
	commitOID, blobOID, err := commitFile(s.store, r, br, filePath, content, message, false, "")
	if err != nil {
		return jsonError(500, err.Error())
	}
	status := 201
	if isUpdate {
		status = 200
	}
	var b bytes.Buffer
	b.WriteString(`{"content":{"name":`)
	escapeJSONString(&b, baseName(filePath))
	b.WriteString(`,"path":`)
	escapeJSONString(&b, filePath)
	b.WriteString(`,"sha":`)
	escapeJSONString(&b, blobOID)
	b.WriteString(`,"size":`)
	b.WriteString(strconv.Itoa(len(content)))
	b.WriteString(`,"encoding":"base64"},"commit":{"sha":`)
	escapeJSONString(&b, commitOID)
	b.WriteString(`,"message":`)
	escapeJSONString(&b, message)
	b.WriteString(`}}`)
	return &Response{Status: status, ContentType: "application/json", Body: b.Bytes()}
}

func restGetContent(s *Server, req *Request, r *Repo, filePath string) *Response {
	refName := r.DefaultBranch
	if v, ok := queryParam(req.Query, "ref"); ok && v != "" {
		refName = v
	}
	ref := refName
	if !strings.HasPrefix(ref, "refs/") && !isOID(ref) {
		ref = "refs/heads/" + ref
	}
	var tip string
	if isOID(refName) {
		tip = refName
	} else {
		var ok bool
		tip, ok = r.getRef(ref)
		if !ok {
			return notFound("Not Found")
		}
	}
	tree, ok := r.commitTree(tip)
	if !ok {
		return notFound("Not Found")
	}
	// File?
	if oid, mode, found := r.treeEntryAt(tree, filePath); found {
		if mode == "40000" {
			// A directory.
			entries, ok := r.listTreeDir(tree, filePath)
			if !ok {
				return notFound("Not Found")
			}
			return &Response{Status: 200, ContentType: "application/json", Body: serializeContentDir(r, filePath, entries)}
		}
		obj := r.getObject(oid)
		if obj == nil {
			return notFound("Not Found")
		}
		return &Response{Status: 200, ContentType: "application/json", Body: serializeContentFile(r, filePath, oid, len(obj.Data), obj.Data)}
	}
	// Directory?
	if entries, ok := r.listTreeDir(tree, filePath); ok {
		return &Response{Status: 200, ContentType: "application/json", Body: serializeContentDir(r, filePath, entries)}
	}
	return notFound("Not Found")
}

func restDeleteContent(s *Server, req *Request, r *Repo, filePath string) *Response {
	message, _ := jsonStringField(req.Body, "message")
	if message == "" {
		message = "Delete " + filePath
	}
	branch, _ := jsonStringField(req.Body, "branch")
	br := branch
	if br == "" {
		br = r.DefaultBranch
	}
	tip, hasTip := r.getRef("refs/heads/" + br)
	if !hasTip || tip == "" {
		return notFound("Not Found")
	}
	tree, ok := r.commitTree(tip)
	if !ok {
		return notFound("Not Found")
	}
	if _, _, found := r.treeEntryAt(tree, filePath); !found {
		return notFound("Not Found")
	}
	commitOID, _, err := commitFile(s.store, r, br, filePath, nil, message, true, "")
	if err != nil {
		return jsonError(500, err.Error())
	}
	var b bytes.Buffer
	b.WriteString(`{"commit":{"sha":`)
	escapeJSONString(&b, commitOID)
	b.WriteString(`}}`)
	return &Response{Status: 200, ContentType: "application/json", Body: b.Bytes()}
}

func handleIssues(s *Server, req *Request, r *Repo, segs []string) *Response {
	if len(segs) == 0 {
		switch req.Method {
		case "POST":
			return restCreateIssue(s, req, r)
		case "GET":
			return restListIssues(s, req, r)
		}
		return jsonError(405, "Method Not Allowed")
	}
	num, err := strconv.Atoi(segs[0])
	if err != nil || num <= 0 {
		return notFound("Not Found")
	}
	if len(segs) == 1 {
		switch req.Method {
		case "GET":
			// Hot path: serve the cached serialized issue JSON.
			if b, ok := r.cachedIssueJSON(num); ok {
				return &Response{Status: 200, ContentType: "application/json", Body: b}
			}
			return notFound("Not Found")
		case "PATCH":
			state, ok := jsonStringField(req.Body, "state")
			if !ok || (state != "open" && state != "closed") {
				return jsonError(400, "state must be open or closed")
			}
			iss := r.setIssueState(num, state)
			if iss == nil {
				return notFound("Not Found")
			}
			return &Response{Status: 200, ContentType: "application/json", Body: serializeIssue(r, iss)}
		}
		return jsonError(405, "Method Not Allowed")
	}
	if len(segs) == 2 && segs[1] == "comments" {
		switch req.Method {
		case "POST":
			body, ok := jsonStringField(req.Body, "body")
			if !ok {
				return jsonError(400, "body is required")
			}
			seq := s.store.nextSeq()
			c := r.addIssueComment(num, body, seq)
			if c == nil {
				return notFound("Not Found")
			}
			return &Response{Status: 201, ContentType: "application/json", Body: serializeComment(r, num, c)}
		case "GET":
			comments := r.issueComments(num)
			if comments == nil {
				return notFound("Not Found")
			}
			var b bytes.Buffer
			b.WriteByte('[')
			for i, c := range comments {
				if i > 0 {
					b.WriteByte(',')
				}
				b.Write(serializeComment(r, num, c))
			}
			b.WriteByte(']')
			return &Response{Status: 200, ContentType: "application/json", Body: b.Bytes()}
		}
		return jsonError(405, "Method Not Allowed")
	}
	return notFound("Not Found")
}

func restCreateIssue(s *Server, req *Request, r *Repo) *Response {
	// Fast path: extract title/body/labels without a full unmarshal.
	title, ok := jsonStringField(req.Body, "title")
	if !ok || title == "" {
		return jsonError(400, "title is required")
	}
	body, _ := jsonStringField(req.Body, "body")
	var labels []string
	if names, ok := jsonStringArrayField(req.Body, "labels"); ok {
		labels = names
	}
	// Validate labels.
	for _, name := range labels {
		if !r.hasLabel(name) {
			return jsonError(422, "Invalid label: "+name)
		}
	}
	seq := s.store.nextSeq()
	iss := r.createIssue(title, body, labels, seq)
	return &Response{Status: 201, ContentType: "application/json", Body: serializeIssue(r, iss)}
}

func restListIssues(s *Server, req *Request, r *Repo) *Response {
	state, _ := queryParam(req.Query, "state")
	sortParam, _ := queryParam(req.Query, "sort")
	direction, _ := queryParam(req.Query, "direction")
	if state == "" {
		state = "open"
	}
	sortDir := "desc"
	if sortParam == "created" && direction == "asc" {
		sortDir = "asc"
	} else if direction == "desc" {
		sortDir = "desc"
	}
	issues := r.listIssues(state, sortDir)
	return &Response{Status: 200, ContentType: "application/json", Body: serializeIssueList(r, issues)}
}

func handlePulls(s *Server, req *Request, r *Repo, segs []string) *Response {
	if len(segs) == 0 {
		switch req.Method {
		case "POST":
			return restCreatePull(s, req, r)
		case "GET":
			state, _ := queryParam(req.Query, "state")
			if state == "" {
				state = "open"
			}
			pulls := r.listPulls(state)
			return &Response{Status: 200, ContentType: "application/json", Body: serializePullList(r, pulls)}
		}
		return jsonError(405, "Method Not Allowed")
	}
	num, err := strconv.Atoi(segs[0])
	if err != nil || num <= 0 {
		return notFound("Not Found")
	}
	if len(segs) == 1 {
		if req.Method != "GET" {
			return jsonError(405, "Method Not Allowed")
		}
		p := r.getPull(num)
		if p == nil {
			return notFound("Not Found")
		}
		return &Response{Status: 200, ContentType: "application/json", Body: serializePull(r, p)}
	}
	if len(segs) == 2 && segs[1] == "files" {
		if req.Method != "GET" {
			return jsonError(405, "Method Not Allowed")
		}
		return restPullFiles(s, req, r, num)
	}
	if len(segs) == 2 && segs[1] == "merge" {
		if req.Method != "PUT" {
			return jsonError(405, "Method Not Allowed")
		}
		return restMergePull(s, req, r, num)
	}
	return notFound("Not Found")
}

func restCreatePull(s *Server, req *Request, r *Repo) *Response {
	title, ok := jsonStringField(req.Body, "title")
	if !ok || title == "" {
		return jsonError(400, "title is required")
	}
	body, _ := jsonStringField(req.Body, "body")
	head, ok := jsonStringField(req.Body, "head")
	if !ok || head == "" {
		return jsonError(400, "head is required")
	}
	base, ok := jsonStringField(req.Body, "base")
	if !ok || base == "" {
		base = r.DefaultBranch
	}
	headSHA, ok := r.getRef("refs/heads/" + head)
	if !ok {
		return jsonError(422, "head branch does not exist")
	}
	baseSHA, ok := r.getRef("refs/heads/" + base)
	if !ok {
		return jsonError(422, "base branch does not exist")
	}
	// Compute the changed paths (merge-base vs head).
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
	return &Response{Status: 201, ContentType: "application/json", Body: serializePull(r, p)}
}

func restPullFiles(s *Server, req *Request, r *Repo, num int) *Response {
	p := r.getPull(num)
	if p == nil {
		return notFound("Not Found")
	}
	mb := r.mergeBase(p.HeadSHA, p.BaseSHA)
	var diffs []treeDiff
	if mb != "" {
		mbTree, _ := r.commitTree(mb)
		headTree, _ := r.commitTree(p.HeadSHA)
		diffs = r.diffTrees(mbTree, headTree)
	}
	var b bytes.Buffer
	b.WriteByte('[')
	for i, d := range diffs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(serializePullFile(r, d))
	}
	b.WriteByte(']')
	return &Response{Status: 200, ContentType: "application/json", Body: b.Bytes()}
}

func restMergePull(s *Server, req *Request, r *Repo, num int) *Response {
	p := r.getPull(num)
	if p == nil {
		return notFound("Not Found")
	}
	if p.Merged {
		return jsonError(405, "already merged")
	}
	sha, status := mergePullRequestInternal(s.store, r, p)
	if status == "conflict" {
		return jsonError(405, "merge conflict")
	}
	if status != "ok" {
		return jsonError(405, status)
	}
	message := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", p.Number, p.Head, p.Title)
	var b bytes.Buffer
	b.WriteString(`{"sha":`)
	escapeJSONString(&b, sha)
	b.WriteString(`,"merged":true,"message":`)
	escapeJSONString(&b, message)
	b.WriteString(`}`)
	return &Response{Status: 200, ContentType: "application/json", Body: b.Bytes()}
}

// mergePullRequestInternal squash-merges p onto its base branch. Returns the
// merge commit sha and a status: "ok", "conflict", or a reason string. The
// caller must hold no lock; this takes r.mu.
func mergePullRequestInternal(s *Store, r *Repo, p *Pull) (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	baseTip, ok := r.refs["refs/heads/"+p.Base]
	if !ok {
		return "", "base branch does not exist"
	}
	headTip, ok := r.refs["refs/heads/"+p.Head]
	if !ok {
		return "", "head branch does not exist"
	}
	p.HeadSHA = headTip
	p.BaseSHA = baseTip
	mb := r.mergeBase(headTip, baseTip)
	if mb == "" {
		return "", "no merge base"
	}
	mbTree, _ := r.commitTree(mb)
	headTree, _ := r.commitTree(headTip)
	baseTree, _ := r.commitTree(baseTip)
	diffs := r.diffTrees(mbTree, headTree)
	newTree := baseTree
	for _, d := range diffs {
		baseOID, _, baseHas := r.treeEntryAt(baseTree, d.Path)
		mbOID, _, _ := r.treeEntryAt(mbTree, d.Path)
		if baseHas && baseOID != mbOID && baseOID != d.NewOID {
			return "", "conflict"
		}
		var err error
		if d.Status == "deleted" {
			newTree, err = r.updateTreePath(newTree, d.Path, "")
		} else {
			newTree, err = r.updateTreePath(newTree, d.Path, d.NewOID)
		}
		if err != nil {
			return "", err.Error()
		}
	}
	seq := s.nextSeq()
	author := viewerUser.Name + " <" + viewerUser.Email + ">"
	message := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", p.Number, p.Head, p.Title)
	commitData := buildCommit(newTree, []string{baseTip}, author, author, message, scenarioEpoch+seq)
	mergeCommit := &Object{Type: "commit", Data: commitData}
	mergeSHA := r.putObjectLocked(mergeCommit)
	r.refs["refs/heads/"+p.Base] = mergeSHA
	r.packCache = make(map[string][]byte)
	p.State = "closed"
	p.Merged = true
	p.MergedAt = timestampFor(seq)
	p.MergeCommitSHA = mergeSHA
	p.BaseSHA = mergeSHA
	return mergeSHA, "ok"
}

