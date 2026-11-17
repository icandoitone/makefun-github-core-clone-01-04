// Git smart HTTP: info/refs advertisement, upload-pack (protocol v2 and v0),
// and receive-pack (push). Implements the wire contract the suite drives.
package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

const gitAgent = "forge/1.0"

const v2AdvertisementCaps = "ls-refs=unborn\nfetch=shallow filter\nagent=" + gitAgent + "\nobject-format=sha1\n"

func uploadPackCapsV0() string {
	return "side-band-64k thin-pack ofs-delta multi_ack multi_ack_detailed shallow symref=HEAD:refs/heads/main agent=" + gitAgent
}

func receivePackCapsV0() string {
	return "report-status-v2 report-status side-band-64k ofs-delta delete-refs symref=HEAD:refs/heads/main agent=" + gitAgent
}

// gitRepoPath parses "/{owner}/{repo}.git/..." into owner, repo, rest.
func gitRepoPath(path string) (owner, repo, rest string, ok bool) {
	// path starts with "/owner/repo.git/..."
	if len(path) < 2 || path[0] != '/' {
		return "", "", "", false
	}
	parts := strings.SplitN(path[1:], "/", 3)
	if len(parts) < 2 {
		return "", "", "", false
	}
	owner = parts[0]
	repoPart := parts[1]
	if !strings.HasSuffix(repoPart, ".git") {
		return "", "", "", false
	}
	repo = repoPart[:len(repoPart)-4]
	if len(parts) == 3 {
		rest = parts[2]
	}
	if owner == "" || repo == "" {
		return "", "", "", false
	}
	return owner, repo, rest, true
}

// sortedRefs returns refs sorted by name (HEAD first is not needed; sorted).
func sortedRefs(refs map[string]string) []string {
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// peelTag returns the commit a tag object points at (following tag->commit).
func peelTag(r *Repo, tagOID string) string {
	obj := r.getObject(tagOID)
	if obj == nil || obj.Type != "tag" {
		return ""
	}
	tv, err := parseTag(obj.Data)
	if err != nil {
		return ""
	}
	// Follow nested tags.
	for {
		target := r.getObject(tv.Object)
		if target == nil || target.Type != "tag" {
			return tv.Object
		}
		ntv, err := parseTag(target.Data)
		if err != nil {
			return tv.Object
		}
		tv = ntv
	}
}

// v0Advertisement builds a v0 ref advertisement body.
func v0Advertisement(r *Repo, service string, caps string) []byte {
	var buf bytes.Buffer
	// Service preamble.
	buf.Write(pktLine([]byte("# service=" + service + "\n")))
	buf.Write(pktFlush())
	refs := r.listRefs()
	names := sortedRefs(refs)
	headTip, hasHead := refs["refs/heads/"+r.DefaultBranch]
	if len(names) == 0 {
		// Empty repository.
		first := zeroOID + " capabilities^{}\x00" + caps + "\n"
		buf.Write(pktLine([]byte(first)))
	} else {
		// HEAD line first, then refs.
		if hasHead {
			first := headTip + " HEAD\x00" + caps + "\n"
			buf.Write(pktLine([]byte(first)))
		} else {
			// No default branch tip; use the first ref for capabilities.
			first := refs[names[0]] + " " + names[0] + "\x00" + caps + "\n"
			buf.Write(pktLine([]byte(first)))
		}
		for _, name := range names {
			if hasHead && name == "refs/heads/"+r.DefaultBranch {
				// Already emitted as HEAD? No: HEAD is separate; emit the branch too.
			}
			oid := refs[name]
			buf.Write(pktLine([]byte(oid + " " + name + "\n")))
			// Peeled tag.
			if strings.HasPrefix(name, "refs/tags/") {
				if peeled := peelTag(r, oid); peeled != "" && peeled != oid {
					buf.Write(pktLine([]byte(peeled + " " + name + "^{}\n")))
				}
			}
		}
	}
	buf.Write(pktFlush())
	return buf.Bytes()
}

// v2Advertisement builds a v2 capability advertisement body.
func v2Advertisement() []byte {
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("# service=git-upload-pack\n")))
	buf.Write(pktFlush())
	buf.Write(pktLine([]byte("version 2\n")))
	for _, line := range strings.Split(strings.TrimSuffix(v2AdvertisementCaps, "\n"), "\n") {
		buf.Write(pktLine([]byte(line + "\n")))
	}
	buf.Write(pktFlush())
	return buf.Bytes()
}

// handleGit dispatches a git smart HTTP request by its trailing path.
// rest holds the segments after /{owner}/{repo}.git.
func handleGit(s *Server, req *Request, owner, repoName string, rest []string) *Response {
	if len(rest) == 2 && rest[0] == "info" && rest[1] == "refs" {
		service, _ := queryParam(req.Query, "service")
		return handleGitInfoRefs(s, req, owner, repoName, service)
	}
	if len(rest) == 1 && rest[0] == "git-upload-pack" {
		return handleGitUploadPack(s, req, owner, repoName)
	}
	if len(rest) == 1 && rest[0] == "git-receive-pack" {
		return handleGitReceivePack(s, req, owner, repoName)
	}
	return notFound("Not Found")
}

// handleGitInfoRefs serves GET info/refs.
func handleGitInfoRefs(s *Server, req *Request, owner, repoName, service string) *Response {
	r := s.store.getRepo(owner, repoName)
	if r == nil {
		return jsonError(404, "Not Found")
	}
	if service == "git-upload-pack" && req.GitProtocol == "version=2" {
		return &Response{Status: 200, ContentType: "application/x-git-upload-pack-advertisement", Body: v2Advertisement()}
	}
	if service == "git-upload-pack" {
		return &Response{Status: 200, ContentType: "application/x-git-upload-pack-advertisement", Body: v0Advertisement(r, service, uploadPackCapsV0())}
	}
	if service == "git-receive-pack" {
		return &Response{Status: 200, ContentType: "application/x-git-receive-pack-advertisement", Body: v0Advertisement(r, service, receivePackCapsV0())}
	}
	return jsonError(404, "Not Found")
}

// --- upload-pack ---

// lsRefsResponse builds the v2 ls-refs response.
func lsRefsResponse(r *Repo, prefixes []string, peel, symrefs, unborn bool) []byte {
	refs := r.listRefs()
	names := sortedRefs(refs)
	var buf bytes.Buffer
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
	headTip, hasHead := refs["refs/heads/"+r.DefaultBranch]
	if !hasHead && len(names) == 0 && unborn {
		// Unborn HEAD.
		line := "unborn HEAD symref-target:refs/heads/" + r.DefaultBranch + "\n"
		buf.Write(pktLine([]byte(line)))
	} else if hasHead {
		if keep("HEAD") {
			line := headTip + " HEAD symref-target:refs/heads/" + r.DefaultBranch + "\n"
			buf.Write(pktLine([]byte(line)))
		}
	}
	for _, name := range names {
		if !keep(name) {
			continue
		}
		oid := refs[name]
		line := oid + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if peeled := peelTag(r, oid); peeled != "" && peeled != oid {
				line += " peeled:" + peeled
			}
		}
		line += "\n"
		buf.Write(pktLine([]byte(line)))
	}
	buf.Write(pktFlush())
	return buf.Bytes()
}

// collectHeld returns the objects the client holds, given its haves and its
// shallow boundaries. A shallow boundary commit is held with its tree, but
// its parents are NOT held, so the traversal stops there.
func collectHeld(haves []string, shallowBounds []string, lookup func(string) *Object) map[string]bool {
	held := make(map[string]bool)
	shallowSet := make(map[string]bool)
	for _, sb := range shallowBounds {
		shallowSet[sb] = true
	}
	// Start from the haves and the shallow boundaries themselves.
	var queue []string
	queue = append(queue, haves...)
	queue = append(queue, shallowBounds...)
	seen := make(map[string]bool)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		held[oid] = true
		if obj.Type == "commit" {
			cv, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			// The commit's tree and blobs are held.
			for k := range collectReachable([]string{cv.Tree}, lookup) {
				held[k] = true
			}
			// Do not traverse into a shallow boundary's parents.
			if shallowSet[oid] {
				continue
			}
			queue = append(queue, cv.Parents...)
		} else if obj.Type == "tag" {
			tv, err := parseTag(obj.Data)
			if err == nil {
				queue = append(queue, tv.Object)
			}
		} else {
			// A tree or blob root: hold its full subtree.
			for k := range collectReachable([]string{oid}, lookup) {
				held[k] = true
			}
		}
	}
	return held
}

// fetchObjects computes the objects to send for a fetch.
// wants: the wanted oids. haves: oids the client holds. depth: 0 = full.
// shallowBounds: the client's current shallow boundaries (their parents are wanted).
// Returns the objects (as *Object list), the shallow boundaries, and the
// unshallow boundaries.
func fetchObjects(r *Repo, wants []string, haves []string, depth int, shallowBounds []string) ([]*Object, []string, []string) {
	lookup := r.lookup
	// What the client already holds: reachable from haves, stopping at the
	// shallow boundaries (their parents are not held).
	haveSet := collectHeld(haves, shallowBounds, lookup)
	// What to send.
	var sendSet map[string]bool
	var shallow []string
	if depth > 0 {
		sendSet, shallow = reachableWithinDepth(wants, lookup, depth)
	} else {
		sendSet = collectReachable(wants, lookup)
	}
	// Remove what the client holds.
	var objs []*Object
	for oid := range sendSet {
		if haveSet[oid] {
			continue
		}
		if obj := lookup(oid); obj != nil {
			objs = append(objs, obj)
		}
	}
	// Deterministic order: sort by oid for a stable pack.
	sort.Slice(objs, func(i, j int) bool {
		return hashObject(objs[i].Type, objs[i].Data) < hashObject(objs[j].Type, objs[j].Data)
	})
	// Unshallow: boundaries whose parents are now all sent (or held).
	var unshallow []string
	for _, sb := range shallowBounds {
		obj := lookup(sb)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		cv, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		// If every parent is in sendSet (being sent) or haveSet, the
		// boundary is no longer shallow.
		allCovered := true
		for _, p := range cv.Parents {
			if !sendSet[p] && !haveSet[p] {
				allCovered = false
				break
			}
		}
		if allCovered {
			unshallow = append(unshallow, sb)
		}
	}
	// The shallow list for the response: the depth edge, excluding any that
	// became unshallow (they are not shallow anymore).
	if depth > 0 {
		var filtered []string
		for _, sh := range shallow {
			skip := false
			for _, u := range unshallow {
				if u == sh {
					skip = true
					break
				}
			}
			if !skip {
				filtered = append(filtered, sh)
			}
		}
		shallow = filtered
	}
	return objs, shallow, unshallow
}

// handleGitUploadPack serves POST git-upload-pack (v2 or v0).
func handleGitUploadPack(s *Server, req *Request, owner, repoName string) *Response {
	r := s.store.getRepo(owner, repoName)
	if r == nil {
		return jsonError(404, "Not Found")
	}
	if req.GitProtocol == "version=2" {
		return handleUploadPackV2(s, req, r)
	}
	return handleUploadPackV0(s, req, r)
}

func handleUploadPackV2(s *Server, req *Request, r *Repo) *Response {
	packets, err := pktDecode(req.Body)
	if err != nil {
		return jsonError(400, "bad pkt-line")
	}
	var command string
	var wants, haves, shallowBounds []string
	var depth int
	var deepenSince int64
	var deepenNot []string
	var done bool
	var filter string
	var prefixes []string
	peel, symrefs, unborn := true, true, true
	inArgs := false
	for _, p := range packets {
		if p == "flush" {
			break
		}
		if p == "delim" {
			inArgs = true
			continue
		}
		data, ok := p.([]byte)
		if !ok {
			continue
		}
		line := strings.TrimSuffix(string(data), "\n")
		if !inArgs {
			if strings.HasPrefix(line, "command=") {
				command = line[len("command="):]
			}
			continue
		}
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[len("ref-prefix "):])
		case strings.HasPrefix(line, "want "):
			wants = append(wants, line[len("want "):])
		case strings.HasPrefix(line, "have "):
			haves = append(haves, line[len("have "):])
		case strings.HasPrefix(line, "shallow "):
			shallowBounds = append(shallowBounds, line[len("shallow "):])
		case strings.HasPrefix(line, "deepen "):
			fmt.Sscanf(line[len("deepen "):], "%d", &depth)
		case strings.HasPrefix(line, "deepen-since "):
			fmt.Sscanf(line[len("deepen-since "):], "%d", &deepenSince)
		case strings.HasPrefix(line, "deepen-not "):
			deepenNot = append(deepenNot, line[len("deepen-not "):])
		case strings.HasPrefix(line, "filter "):
			filter = line[len("filter "):]
		case line == "done":
			done = true
		}
	}
	_ = deepenSince
	_ = deepenNot
	_ = filter
	_ = done
	if command == "ls-refs" {
		body := lsRefsResponse(r, prefixes, peel, symrefs, unborn)
		return &Response{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: body}
	}
	if command != "fetch" {
		return jsonError(400, "unknown command")
	}
	if len(wants) == 0 {
		return &Response{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: pktFlush()}
	}
	// Cache key for the pack.
	cacheKey := "v2|" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + strings.Join(shallowBounds, ",") + "|" + fmt.Sprint(depth)
	if cached := r.packCacheGet(cacheKey); cached != nil {
		return &Response{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: cached}
	}
	objs, shallow, unshallow := fetchObjects(r, wants, haves, depth, shallowBounds)
	_ = unshallow
	pack := writePack(objs)
	var buf bytes.Buffer
	// acknowledgments
	buf.Write(pktLine([]byte("acknowledgments\n")))
	if len(haves) == 0 && len(shallowBounds) == 0 {
		buf.Write(pktLine([]byte("NAK\n")))
	} else {
		// Acknowledge the haves that are ancestors of the wants.
		acked := false
		for _, h := range haves {
			if r.isAncestor(h, wants[0]) {
				buf.Write(pktLine([]byte("ACK " + h + "\n")))
				acked = true
			}
		}
		if acked {
			buf.Write(pktLine([]byte("ready\n")))
		} else {
			buf.Write(pktLine([]byte("NAK\n")))
		}
	}
	buf.Write(pktDelim())
	// shallow-info
	if len(shallow) > 0 || len(unshallow) > 0 {
		buf.Write(pktLine([]byte("shallow-info\n")))
		for _, sh := range shallow {
			buf.Write(pktLine([]byte("shallow " + sh + "\n")))
		}
		for _, u := range unshallow {
			buf.Write(pktLine([]byte("unshallow " + u + "\n")))
		}
		buf.Write(pktDelim())
	}
	// packfile
	buf.Write(pktLine([]byte("packfile\n")))
	buf.Write(sidebandPackets(1, pack))
	buf.Write(pktFlush())
	body := buf.Bytes()
	r.packCachePut(cacheKey, body)
	return &Response{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: body}
}

func handleUploadPackV0(s *Server, req *Request, r *Repo) *Response {
	packets, err := pktDecode(req.Body)
	if err != nil {
		return jsonError(400, "bad pkt-line")
	}
	var wants, haves, shallowBounds []string
	var depth int
	// v0 has two sections separated by a flush.
	section := 0
	for _, p := range packets {
		if p == "flush" {
			section++
			continue
		}
		data, ok := p.([]byte)
		if !ok {
			continue
		}
		line := strings.TrimSuffix(string(data), "\n")
		if section == 0 {
			switch {
			case strings.HasPrefix(line, "want "):
				// First want may carry capabilities after a space.
				rest := line[len("want "):]
				fields := strings.Fields(rest)
				if len(fields) > 0 {
					wants = append(wants, fields[0])
				}
			case strings.HasPrefix(line, "shallow "):
				shallowBounds = append(shallowBounds, line[len("shallow "):])
			case strings.HasPrefix(line, "deepen "):
				fmt.Sscanf(line[len("deepen "):], "%d", &depth)
			}
		} else {
			switch {
			case strings.HasPrefix(line, "have "):
				haves = append(haves, line[len("have "):])
			}
		}
	}
	if len(wants) == 0 {
		return &Response{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: pktFlush()}
	}
	cacheKey := "v0|" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + strings.Join(shallowBounds, ",") + "|" + fmt.Sprint(depth)
	if cached := r.packCacheGet(cacheKey); cached != nil {
		return &Response{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: cached}
	}
	objs, shallow, unshallow := fetchObjects(r, wants, haves, depth, shallowBounds)
	pack := writePack(objs)
	var buf bytes.Buffer
	// NAK / ACK lines (plain pkt-lines, not sideband).
	if len(haves) == 0 && len(shallowBounds) == 0 {
		buf.Write(pktLine([]byte("NAK\n")))
	} else {
		acked := false
		for _, h := range haves {
			if r.isAncestor(h, wants[0]) {
				buf.Write(pktLine([]byte("ACK " + h + "\n")))
				acked = true
			}
		}
		if !acked {
			buf.Write(pktLine([]byte("NAK\n")))
		}
	}
	for _, sh := range shallow {
		buf.Write(pktLine([]byte("shallow " + sh + "\n")))
	}
	for _, u := range unshallow {
		buf.Write(pktLine([]byte("unshallow " + u + "\n")))
	}
	// Pack in sideband.
	buf.Write(sidebandPackets(1, pack))
	buf.Write(pktFlush())
	body := buf.Bytes()
	r.packCachePut(cacheKey, body)
	return &Response{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: body}
}

// --- receive-pack (push) ---

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

type pushRefResult struct {
	ref    string
	ok     bool
	reason string
}

// handleGitReceivePack serves POST git-receive-pack.
func handleGitReceivePack(s *Server, req *Request, owner, repoName string) *Response {
	r := s.store.getRepo(owner, repoName)
	if r == nil {
		return jsonError(404, "Not Found")
	}
	// Split the body into the pkt-line command section and the pack.
	// The command section ends at the first flush packet.
	packets, packStart, err := pktDecodeWithOffset(req.Body)
	if err != nil {
		return jsonError(400, "bad pkt-line")
	}
	var commands []pushCommand
	var capabilities []string
	for _, p := range packets {
		if p == "flush" {
			break
		}
		data, ok := p.([]byte)
		if !ok {
			continue
		}
		line := strings.TrimSuffix(string(data), "\n")
		// First line may carry capabilities after NUL.
		caps := ""
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			caps = line[nul+1:]
			line = line[:nul]
		}
		if caps != "" {
			capabilities = strings.Fields(caps)
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		commands = append(commands, pushCommand{oldOID: fields[0], newOID: fields[1], ref: fields[2]})
	}
	_ = capabilities
	// Parse and store the pack (if any).
	var packObjs map[string]*Object
	if packStart < len(req.Body) {
		packBytes := req.Body[packStart:]
		// The pack may be empty (delete-only pushes send no pack).
		if len(packBytes) >= 12 {
			objs, err := readPack(packBytes, func(oid string) *Object { return r.getObject(oid) })
			if err != nil {
				// Unpack failure: report it.
				return pushReportResponse(true, nil, "unpack failed", commands)
			}
			packObjs = objs
		}
	}
	// Store the objects first.
	if len(packObjs) > 0 {
		r.mu.Lock()
		r.putObjectsLocked(packObjs)
		r.mu.Unlock()
	}
	// Validate and apply each command.
	var results []pushRefResult
	r.mu.Lock()
	for _, cmd := range commands {
		cur, exists := r.refs[cmd.ref]
		if cmd.newOID == zeroOID {
			// Delete.
			if !exists {
				results = append(results, pushRefResult{cmd.ref, false, "remote ref does not exist"})
				continue
			}
			if cmd.oldOID != zeroOID && cur != cmd.oldOID {
				results = append(results, pushRefResult{cmd.ref, false, "old value does not match"})
				continue
			}
			delete(r.refs, cmd.ref)
			results = append(results, pushRefResult{cmd.ref, true, ""})
			continue
		}
		// Create or update: the new object must exist.
		if r.objects[cmd.newOID] == nil {
			results = append(results, pushRefResult{cmd.ref, false, "object not found"})
			continue
		}
		if cmd.oldOID == zeroOID {
			// Create.
			if exists {
				results = append(results, pushRefResult{cmd.ref, false, "already exists"})
				continue
			}
			r.refs[cmd.ref] = cmd.newOID
			results = append(results, pushRefResult{cmd.ref, true, ""})
			continue
		}
		// Update.
		if !exists {
			results = append(results, pushRefResult{cmd.ref, false, "remote ref does not exist"})
			continue
		}
		if cur != cmd.oldOID {
			results = append(results, pushRefResult{cmd.ref, false, "non-fast-forward"})
			continue
		}
		if !r.isAncestorLocked(cmd.oldOID, cmd.newOID) {
			results = append(results, pushRefResult{cmd.ref, false, "non-fast-forward"})
			continue
		}
		r.refs[cmd.ref] = cmd.newOID
		results = append(results, pushRefResult{cmd.ref, true, ""})
	}
	// Any ref change invalidates the pack cache.
	r.packCache = make(map[string][]byte)
	r.mu.Unlock()
	return pushReportResponse(false, results, "", commands)
}

// isAncestorLocked is isAncestor under the repo write lock.
func (r *Repo) isAncestorLocked(ancestor, oid string) bool {
	if ancestor == oid {
		return true
	}
	if ancestor == zeroOID {
		return true
	}
	seen := make(map[string]bool)
	queue := []string{oid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == ancestor {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.objects[cur]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		cv, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, cv.Parents...)
	}
	return false
}

// pushReportResponse builds the report-status reply.
func pushReportResponse(unpackFailed bool, results []pushRefResult, unpackDetail string, commands []pushCommand) *Response {
	var report bytes.Buffer
	if unpackFailed {
		report.Write(pktLine([]byte("unpack " + unpackDetail + "\n")))
		for _, cmd := range commands {
			report.Write(pktLine([]byte("ng " + cmd.ref + " unpack failed\n")))
		}
	} else {
		report.Write(pktLine([]byte("unpack ok\n")))
		for _, res := range results {
			if res.ok {
				report.Write(pktLine([]byte("ok " + res.ref + "\n")))
			} else {
				report.Write(pktLine([]byte("ng " + res.ref + " " + res.reason + "\n")))
			}
		}
	}
	report.Write(pktFlush())
	// Wrap in sideband band 1 (the client negotiated side-band-64k).
	var body bytes.Buffer
	body.Write(sidebandPackets(1, report.Bytes()))
	body.Write(pktFlush())
	return &Response{Status: 200, ContentType: "application/x-git-receive-pack-result", Body: body.Bytes()}
}

// pktDecodeWithOffset decodes pkt-lines and returns the offset where the
// raw pack bytes begin (just past the first flush packet).
func pktDecodeWithOffset(buf []byte) ([]interface{}, int, error) {
	var out []interface{}
	off := 0
	for off < len(buf) {
		if off+4 > len(buf) {
			return nil, 0, errPack
		}
		hdr := string(buf[off : off+4])
		var n int
		_, err := fmt.Sscanf(hdr, "%04x", &n)
		if err != nil {
			return nil, 0, errPack
		}
		if n == 0 {
			out = append(out, "flush")
			off += 4
			return out, off, nil
		}
		if n == 1 {
			out = append(out, "delim")
			off += 4
			continue
		}
		if n == 2 {
			out = append(out, "response-end")
			off += 4
			continue
		}
		if n == 3 || n > 65520 || off+n > len(buf) {
			return nil, 0, errPack
		}
		out = append(out, buf[off+4:off+n])
		off += n
	}
	return out, off, nil
}

// jsonError renders a JSON error response.
func jsonError(status int, msg string) *Response {
	return &Response{Status: status, ContentType: "application/json", Body: errorJSON(msg)}
}
