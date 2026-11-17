// In-memory store: repositories, git objects, issues, pull requests, labels.
// Deterministic: timestamps come from a fixed epoch plus a sequence counter,
// so two runs that perform the same operations produce identical bytes.
package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const scenarioEpoch = 1735689600 // wire.ts SCENARIO_EPOCH_SEC

// The viewer: the login the bearer token names (wire.ts scenario viewer).
var viewerUser = &User{ID: 1000, Login: "adaiversen", Name: "ada iversen", Email: "adaiversen@example.invalid"}

type User struct {
	ID    int
	Login string
	Name  string
	Email string
}

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID   int
	Body string
	Seq  int64
}

type Issue struct {
	Number   int
	Title    string
	Body     string
	State    string // open | closed
	Labels   []string
	Comments []*Comment
	Seq      int64
}

type Pull struct {
	Number        int
	Title         string
	Body          string
	State         string // open | closed
	Head          string // branch name
	Base          string // branch name
	HeadSHA       string
	BaseSHA       string
	Merged        bool
	MergedAt      string
	MergeCommitSHA string
	ChangedPaths  []string
	Seq           int64
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Seq           int64

	mu         sync.RWMutex
	refs       map[string]string // ref name -> oid
	objects    map[string]*Object
	issues     []*Issue
	issueByNum map[int]*Issue
	pulls      []*Pull
	pullByNum  map[int]*Pull
	labels     map[string]*Label
	nextIssue  int
	nextPull   int
	nextComment int

	// Caches, invalidated on mutation.
	packCache map[string][]byte // key: wants|haves|depth -> pack bytes
	issueJSON map[int][]byte    // issue number -> serialized JSON
	repoJSON  []byte
}

type Store struct {
	mu     sync.RWMutex
	repos  map[string]*Repo // "owner/name" -> repo
	nextID int64
	seq    int64 // global deterministic sequence for timestamps
}

func newStore() *Store {
	return &Store{repos: make(map[string]*Repo)}
}

// nextSeq returns the next deterministic sequence number.
func (s *Store) nextSeq() int64 {
	return atomic.AddInt64(&s.seq, 1)
}

// timestampFor returns a deterministic ISO timestamp for a sequence number.
func timestampFor(seq int64) string {
	// Fixed epoch + seq seconds, formatted as ISO 8601 UTC.
	sec := scenarioEpoch + seq
	return formatISO(sec)
}

func formatISO(sec int64) string {
	// Days since epoch to civil date (Howard Hinnant's algorithm).
	days := sec / 86400
	rem := sec % 86400
	if rem < 0 {
		rem += 86400
		days--
	}
	hour := rem / 3600
	min := (rem % 3600) / 60
	second := rem % 60
	// Civil from days.
	z := days + 719468
	era := z / 146097
	if z < 0 {
		era = (z - 146096) / 146097
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02dZ", y, m, d, hour, min, second)
}

func repoKey(owner, name string) string { return owner + "/" + name }

// createRepo creates a repository owned by the viewer. Returns nil when it exists.
func (s *Store) createRepo(name, description string, private bool) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(viewerUser.Login, name)
	if _, ok := s.repos[key]; ok {
		return nil
	}
	id := int(atomic.AddInt64(&s.nextID, 1))
	seq := s.nextSeq()
	r := &Repo{
		ID:            id,
		Owner:         viewerUser.Login,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		Seq:           seq,
		refs:          make(map[string]string),
		objects:       make(map[string]*Object),
		issueByNum:    make(map[int]*Issue),
		pullByNum:     make(map[int]*Pull),
		labels:        make(map[string]*Label),
		nextIssue:     1,
		nextPull:      1,
		nextComment:   1,
		packCache:     make(map[string][]byte),
		issueJSON:     make(map[int][]byte),
	}
	s.repos[key] = r
	return r
}

// getRepo returns the repository, or nil. Only the viewer's repos exist.
func (s *Store) getRepo(owner, name string) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if owner != viewerUser.Login {
		return nil
	}
	return s.repos[repoKey(owner, name)]
}

// listViewerRepos returns the viewer's repositories in creation order.
func (s *Store) listViewerRepos() []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Repo
	for _, r := range s.repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// deleteRepo removes a repository. Returns false when absent.
func (s *Store) deleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner != viewerUser.Login {
		return false
	}
	key := repoKey(owner, name)
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	return true
}

// --- repo object helpers (caller holds r.mu or uses the locked variants) ---

func (r *Repo) getObject(oid string) *Object {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.objects[oid]
}

func (r *Repo) putObjectLocked(obj *Object) string {
	oid := hashObject(obj.Type, obj.Data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = obj
	}
	return oid
}

func (r *Repo) putObjectsLocked(objs map[string]*Object) {
	for oid, obj := range objs {
		if _, ok := r.objects[oid]; !ok {
			r.objects[oid] = obj
		}
	}
}

func (r *Repo) getRef(ref string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	oid, ok := r.refs[ref]
	return oid, ok
}

func (r *Repo) setRefLocked(ref, oid string) {
	r.refs[ref] = oid
	// Any ref change invalidates the pack cache.
	r.packCache = make(map[string][]byte)
}

func (r *Repo) deleteRefLocked(ref string) {
	delete(r.refs, ref)
	r.packCache = make(map[string][]byte)
}

func (r *Repo) listRefs() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.refs))
	for k, v := range r.refs {
		out[k] = v
	}
	return out
}

func (r *Repo) headOID() (string, bool) {
	return r.getRef("refs/heads/" + r.DefaultBranch)
}

// lookup is the collectReachable lookup for this repo.
func (r *Repo) lookup(oid string) *Object {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.objects[oid]
}

// --- tree update ---

// updateTreePath returns the oid of a new tree equal to baseTree but with
// path set to blobOID (or removed when blobOID is ""). It creates the needed
// intermediate tree objects. baseTree may be "" for an empty tree.
func (r *Repo) updateTreePath(baseTree, path, blobOID string) (string, error) {
	segments := strings.Split(path, "/")
	return r.updateTreeRec(baseTree, segments, blobOID)
}

func (r *Repo) updateTreeRec(treeOID string, segments []string, blobOID string) (string, error) {
	var entries []TreeEntry
	if treeOID != "" {
		obj := r.objects[treeOID]
		if obj == nil || obj.Type != "tree" {
			return "", fmt.Errorf("tree %s not found", treeOID)
		}
		var err error
		entries, err = parseTree(obj.Data)
		if err != nil {
			return "", err
		}
	}
	name := segments[0]
	isLast := len(segments) == 1
	// Find the existing entry.
	idx := -1
	for i, e := range entries {
		if e.Name == name {
			idx = i
			break
		}
	}
	if isLast {
		if blobOID == "" {
			// Delete.
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			if idx >= 0 {
				entries[idx].OID = blobOID
				entries[idx].Mode = "100644"
			} else {
				entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: blobOID})
			}
		}
	} else {
		var childTree string
		if idx >= 0 {
			childTree = entries[idx].OID
		}
		newChild, err := r.updateTreeRec(childTree, segments[1:], blobOID)
		if err != nil {
			return "", err
		}
		if newChild == "" {
			// Child became empty: remove the directory entry.
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			if idx >= 0 {
				entries[idx].OID = newChild
				entries[idx].Mode = "40000"
			} else {
				entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: newChild})
			}
		}
	}
	if len(entries) == 0 {
		return "", nil
	}
	data := buildTree(entries)
	return r.putObjectLocked(&Object{Type: "tree", Data: data}), nil
}

// treeEntryAt returns the blob oid and mode for path in the tree at treeOID.
func (r *Repo) treeEntryAt(treeOID, path string) (string, string, bool) {
	segments := strings.Split(path, "/")
	cur := treeOID
	for i, seg := range segments {
		obj := r.objects[cur]
		if obj == nil || obj.Type != "tree" {
			return "", "", false
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return "", "", false
		}
		found := false
		for _, e := range entries {
			if e.Name == seg {
				if i == len(segments)-1 {
					return e.OID, e.Mode, true
				}
				cur = e.OID
				found = true
				break
			}
		}
		if !found {
			return "", "", false
		}
	}
	return "", "", false
}

// listTreeDir returns the entries of the directory at dirPath in treeOID.
// dirPath "" is the root.
func (r *Repo) listTreeDir(treeOID, dirPath string) ([]TreeEntry, bool) {
	cur := treeOID
	if dirPath != "" {
		for _, seg := range strings.Split(dirPath, "/") {
			obj := r.objects[cur]
			if obj == nil || obj.Type != "tree" {
				return nil, false
			}
			entries, err := parseTree(obj.Data)
			if err != nil {
				return nil, false
			}
			found := false
			for _, e := range entries {
				if e.Name == seg && e.Mode == "40000" {
					cur = e.OID
					found = true
					break
				}
			}
			if !found {
				return nil, false
			}
		}
	}
	obj := r.objects[cur]
	if obj == nil || obj.Type != "tree" {
		return nil, false
	}
	entries, err := parseTree(obj.Data)
	if err != nil {
		return nil, false
	}
	return entries, true
}

// commitTree returns the tree oid of a commit.
func (r *Repo) commitTree(commitOID string) (string, bool) {
	obj := r.objects[commitOID]
	if obj == nil || obj.Type != "commit" {
		return "", false
	}
	cv, err := parseCommit(obj.Data)
	if err != nil {
		return "", false
	}
	return cv.Tree, true
}

// listCommits returns commit oids reachable from ref, newest first.
func (r *Repo) listCommits(ref string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tip, ok := r.refs[ref]
	if !ok || tip == "" || tip == zeroOID {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	queue := []string{tip}
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
		out = append(out, oid)
		cv, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, cv.Parents...)
	}
	return out
}

// isAncestor reports whether ancestor is an ancestor of (or equal to) oid.
func (r *Repo) isAncestor(ancestor, oid string) bool {
	if ancestor == oid {
		return true
	}
	if ancestor == zeroOID {
		return true // creating a ref: any new tip is fine
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

// mergeBase returns the merge base of a and b (first common ancestor found
// by BFS from both sides), or "" when there is none.
func (r *Repo) mergeBase(a, b string) string {
	if a == b {
		return a
	}
	// Collect ancestors of a.
	ancA := make(map[string]bool)
	queue := []string{a}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if ancA[cur] {
			continue
		}
		ancA[cur] = true
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
	// BFS from b; first hit in ancA is the merge base.
	queue = []string{b}
	seen := make(map[string]bool)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if ancA[cur] {
			return cur
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
	return ""
}

// diffTrees returns the paths that differ between baseTree and headTree.
// Each result is (path, status, oldBlob, newBlob); status is added/modified/deleted.
type treeDiff struct {
	Path    string
	Status  string
	OldOID  string
	NewOID  string
}

func (r *Repo) diffTrees(baseTree, headTree string) []treeDiff {
	baseFiles := make(map[string]string)
	headFiles := make(map[string]string)
	r.flattenTree(baseTree, "", baseFiles)
	r.flattenTree(headTree, "", headFiles)
	var out []treeDiff
	for path, oid := range headFiles {
		if old, ok := baseFiles[path]; !ok {
			out = append(out, treeDiff{Path: path, Status: "added", NewOID: oid})
		} else if old != oid {
			out = append(out, treeDiff{Path: path, Status: "modified", OldOID: old, NewOID: oid})
		}
	}
	for path, oid := range baseFiles {
		if _, ok := headFiles[path]; !ok {
			out = append(out, treeDiff{Path: path, Status: "deleted", OldOID: oid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (r *Repo) flattenTree(treeOID, prefix string, out map[string]string) {
	if treeOID == "" {
		return
	}
	obj := r.objects[treeOID]
	if obj == nil || obj.Type != "tree" {
		return
	}
	entries, err := parseTree(obj.Data)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := prefix + e.Name
		if e.Mode == "40000" {
			r.flattenTree(e.OID, p+"/", out)
		} else if e.Mode != "160000" {
			out[p] = e.OID
		}
	}
}

// --- issues ---

func (r *Repo) createIssue(title, body string, labels []string, seq int64) *Issue {
	r.mu.Lock()
	defer r.mu.Unlock()
	num := r.nextIssue
	r.nextIssue++
	iss := &Issue{Number: num, Title: title, Body: body, State: "open", Labels: labels, Seq: seq}
	r.issues = append(r.issues, iss)
	r.issueByNum[num] = iss
	// Invalidate the repo JSON (issue count changes) but keep other issue JSONs.
	r.repoJSON = nil
	return iss
}

func (r *Repo) getIssue(num int) *Issue {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.issueByNum[num]
}

func (r *Repo) listIssues(state, sortDir string) []*Issue {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Issue
	for _, iss := range r.issues {
		if state != "" && state != "all" && iss.State != state {
			continue
		}
		out = append(out, iss)
	}
	if sortDir == "asc" {
		// Already in creation order (ascending).
	} else {
		// Default: descending (newest first).
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

func (r *Repo) setIssueState(num int, state string) *Issue {
	r.mu.Lock()
	defer r.mu.Unlock()
	iss := r.issueByNum[num]
	if iss == nil {
		return nil
	}
	iss.State = state
	delete(r.issueJSON, num)
	r.repoJSON = nil
	return iss
}

func (r *Repo) addIssueComment(num int, body string, seq int64) *Comment {
	r.mu.Lock()
	defer r.mu.Unlock()
	iss := r.issueByNum[num]
	if iss == nil {
		return nil
	}
	c := &Comment{ID: r.nextComment, Body: body, Seq: seq}
	r.nextComment++
	iss.Comments = append(iss.Comments, c)
	delete(r.issueJSON, num)
	return c
}

func (r *Repo) issueComments(num int) []*Comment {
	r.mu.RLock()
	defer r.mu.RUnlock()
	iss := r.issueByNum[num]
	if iss == nil {
		return nil
	}
	return iss.Comments
}

// --- labels ---

func (r *Repo) createLabel(name, color string) *Label {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.labels[name]; ok {
		return l
	}
	l := &Label{Name: name, Color: color}
	r.labels[name] = l
	return l
}

func (r *Repo) hasLabel(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.labels[name]
	return ok
}

// --- pulls ---

func (r *Repo) createPull(title, body, head, base, headSHA, baseSHA string, changed []string, seq int64) *Pull {
	r.mu.Lock()
	defer r.mu.Unlock()
	num := r.nextPull
	r.nextPull++
	p := &Pull{
		Number: num, Title: title, Body: body, State: "open",
		Head: head, Base: base, HeadSHA: headSHA, BaseSHA: baseSHA,
		ChangedPaths: changed, Seq: seq,
	}
	r.pulls = append(r.pulls, p)
	r.pullByNum[num] = p
	r.repoJSON = nil
	return p
}

func (r *Repo) getPull(num int) *Pull {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pullByNum[num]
}

func (r *Repo) listPulls(state string) []*Pull {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Pull
	for _, p := range r.pulls {
		if state == "all" || state == "" {
			out = append(out, p)
		} else if p.State == state {
			out = append(out, p)
		}
	}
	// Newest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// invalidateCaches clears the derived caches after a mutation.
func (r *Repo) invalidateCaches() {
	r.packCache = make(map[string][]byte)
	r.issueJSON = make(map[int][]byte)
	r.repoJSON = nil
}

// cachedIssueJSON returns the pre-serialized issue JSON, building it on a miss.
func (r *Repo) cachedIssueJSON(num int) ([]byte, bool) {
	r.mu.RLock()
	if b, ok := r.issueJSON[num]; ok {
		r.mu.RUnlock()
		return b, true
	}
	iss := r.issueByNum[num]
	r.mu.RUnlock()
	if iss == nil {
		return nil, false
	}
	b := serializeIssue(r, iss)
	r.mu.Lock()
	// Only store when the issue still exists and is unchanged.
	if cur, ok := r.issueByNum[num]; ok && cur == iss {
		if r.issueJSON == nil {
			r.issueJSON = make(map[int][]byte)
		}
		r.issueJSON[num] = b
	}
	r.mu.Unlock()
	return b, true
}

// cachedRepoJSON returns the pre-serialized repository JSON.
func (r *Repo) cachedRepoJSON() []byte {
	r.mu.RLock()
	if r.repoJSON != nil {
		defer r.mu.RUnlock()
		return r.repoJSON
	}
	r.mu.RUnlock()
	b := serializeRepo(r)
	r.mu.Lock()
	if r.repoJSON == nil {
		r.repoJSON = b
	} else {
		b = r.repoJSON
	}
	r.mu.Unlock()
	return b
}

// packCacheGet returns a cached pack for key, or nil.
func (r *Repo) packCacheGet(key string) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.packCache[key]
}

func (r *Repo) packCachePut(key string, pack []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.packCache == nil {
		r.packCache = make(map[string][]byte)
	}
	r.packCache[key] = pack
}

var _ = bytes.MinRead
