// JSON serializers for the REST/GraphQL/MCP surfaces. Hand-built with a
// bytes.Buffer for speed on the hot paths; the shapes match the suite's
// expectations (number, title, body, state, labels, head/base, ...).
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
)

// globalID returns a stable opaque id for a GraphQL node.
func globalID(kind, key string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + key))
}

func repoGlobalID(r *Repo) string { return globalID("Repository", r.Owner+"/"+r.Name) }
func issueGlobalID(r *Repo, num int) string {
	return globalID("Issue", r.Owner+"/"+r.Name+":"+strconv.Itoa(num))
}
func pullGlobalID(r *Repo, num int) string {
	return globalID("PullRequest", r.Owner+"/"+r.Name+":"+strconv.Itoa(num))
}
func commentGlobalID(r *Repo, num, cid int) string {
	return globalID("IssueComment", r.Owner+"/"+r.Name+":"+strconv.Itoa(num)+":"+strconv.Itoa(cid))
}

func userJSON(u *User) []byte {
	var b bytes.Buffer
	b.WriteString(`{"login":`)
	escapeJSONString(&b, u.Login)
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(u.ID))
	b.WriteString(`,"name":`)
	escapeJSONString(&b, u.Name)
	b.WriteString(`,"email":`)
	escapeJSONString(&b, u.Email)
	b.WriteString(`,"type":"User"}`)
	return b.Bytes()
}

func serializeRepo(r *Repo) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return serializeRepoLocked(r)
}

func serializeRepoLocked(r *Repo) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(r.ID))
	b.WriteString(`,"name":`)
	escapeJSONString(&b, r.Name)
	b.WriteString(`,"full_name":`)
	escapeJSONString(&b, r.Owner+"/"+r.Name)
	b.WriteString(`,"owner":{"login":`)
	escapeJSONString(&b, r.Owner)
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(viewerUser.ID))
	b.WriteString(`,"type":"User"}`)
	b.WriteString(`,"private":`)
	if r.Private {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	b.WriteString(`,"description":`)
	if r.Description == "" {
		b.WriteString("null")
	} else {
		escapeJSONString(&b, r.Description)
	}
	b.WriteString(`,"default_branch":`)
	escapeJSONString(&b, r.DefaultBranch)
	b.WriteString(`,"html_url":`)
	escapeJSONString(&b, "https://example.invalid/"+r.Owner+"/"+r.Name)
	b.WriteString(`,"url":`)
	escapeJSONString(&b, "/api/v3/repos/"+r.Owner+"/"+r.Name)
	b.WriteString(`,"created_at":`)
	escapeJSONString(&b, timestampFor(r.Seq))
	b.WriteString(`,"updated_at":`)
	escapeJSONString(&b, timestampFor(r.Seq))
	b.WriteString(`,"has_issues":true,"has_projects":false,"has_wiki":false}`)
	return b.Bytes()
}

func labelJSON(l *Label) []byte {
	var b bytes.Buffer
	b.WriteString(`{"name":`)
	escapeJSONString(&b, l.Name)
	b.WriteString(`,"color":`)
	escapeJSONString(&b, l.Color)
	b.WriteString(`}`)
	return b.Bytes()
}

func serializeIssue(r *Repo, iss *Issue) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(iss.Number))
	b.WriteString(`,"node_id":`)
	escapeJSONString(&b, issueGlobalID(r, iss.Number))
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(iss.Number))
	b.WriteString(`,"title":`)
	escapeJSONString(&b, iss.Title)
	b.WriteString(`,"body":`)
	if iss.Body == "" {
		b.WriteString(`""`)
	} else {
		escapeJSONString(&b, iss.Body)
	}
	b.WriteString(`,"state":`)
	escapeJSONString(&b, iss.State)
	b.WriteString(`,"labels":[`)
	for i, name := range iss.Labels {
		if i > 0 {
			b.WriteByte(',')
		}
		color := "ededed"
		if l, ok := r.labels[name]; ok {
			color = l.Color
		}
		b.WriteString(`{"name":`)
		escapeJSONString(&b, name)
		b.WriteString(`,"color":`)
		escapeJSONString(&b, color)
		b.WriteString(`}`)
	}
	b.WriteString(`],"user":`)
	b.Write(userJSON(viewerUser))
	b.WriteString(`,"created_at":`)
	escapeJSONString(&b, timestampFor(iss.Seq))
	b.WriteString(`,"updated_at":`)
	escapeJSONString(&b, timestampFor(iss.Seq))
	b.WriteString(`,"comments":`)
	b.WriteString(strconv.Itoa(len(iss.Comments)))
	b.WriteString(`}`)
	return b.Bytes()
}

func serializeIssueList(r *Repo, issues []*Issue) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, iss := range issues {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(serializeIssue(r, iss))
	}
	b.WriteByte(']')
	return b.Bytes()
}

func serializeComment(r *Repo, issueNum int, c *Comment) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(c.ID))
	b.WriteString(`,"node_id":`)
	escapeJSONString(&b, commentGlobalID(r, issueNum, c.ID))
	b.WriteString(`,"body":`)
	escapeJSONString(&b, c.Body)
	b.WriteString(`,"user":`)
	b.Write(userJSON(viewerUser))
	b.WriteString(`,"created_at":`)
	escapeJSONString(&b, timestampFor(c.Seq))
	b.WriteString(`,"updated_at":`)
	escapeJSONString(&b, timestampFor(c.Seq))
	b.WriteString(`}`)
	return b.Bytes()
}

func serializePull(r *Repo, p *Pull) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(p.Number))
	b.WriteString(`,"node_id":`)
	escapeJSONString(&b, pullGlobalID(r, p.Number))
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(p.Number))
	b.WriteString(`,"title":`)
	escapeJSONString(&b, p.Title)
	b.WriteString(`,"body":`)
	if p.Body == "" {
		b.WriteString(`""`)
	} else {
		escapeJSONString(&b, p.Body)
	}
	b.WriteString(`,"state":`)
	escapeJSONString(&b, p.State)
	b.WriteString(`,"head":{"ref":`)
	escapeJSONString(&b, p.Head)
	b.WriteString(`,"sha":`)
	escapeJSONString(&b, p.HeadSHA)
	b.WriteString(`,"label":`)
	escapeJSONString(&b, r.Owner+":"+p.Head)
	b.WriteString(`}`)
	b.WriteString(`,"base":{"ref":`)
	escapeJSONString(&b, p.Base)
	b.WriteString(`,"sha":`)
	escapeJSONString(&b, p.BaseSHA)
	b.WriteString(`,"label":`)
	escapeJSONString(&b, r.Owner+":"+p.Base)
	b.WriteString(`}`)
	b.WriteString(`,"merged":`)
	if p.Merged {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	b.WriteString(`,"mergeable":true`)
	if p.Merged {
		b.WriteString(`,"merged_at":`)
		escapeJSONString(&b, p.MergedAt)
		b.WriteString(`,"merge_commit_sha":`)
		escapeJSONString(&b, p.MergeCommitSHA)
	} else {
		b.WriteString(`,"merged_at":null,"merge_commit_sha":null`)
	}
	b.WriteString(`,"created_at":`)
	escapeJSONString(&b, timestampFor(p.Seq))
	b.WriteString(`,"updated_at":`)
	escapeJSONString(&b, timestampFor(p.Seq))
	b.WriteString(`}`)
	return b.Bytes()
}

func serializePullList(r *Repo, pulls []*Pull) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, p := range pulls {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(serializePull(r, p))
	}
	b.WriteByte(']')
	return b.Bytes()
}

// serializePullFile renders one pull file entry.
func serializePullFile(r *Repo, d treeDiff) []byte {
	additions, deletions := lineDiffCounts(r, d)
	var b bytes.Buffer
	b.WriteString(`{"filename":`)
	escapeJSONString(&b, d.Path)
	b.WriteString(`,"status":`)
	escapeJSONString(&b, d.Status)
	b.WriteString(`,"additions":`)
	b.WriteString(strconv.Itoa(additions))
	b.WriteString(`,"deletions":`)
	b.WriteString(strconv.Itoa(deletions))
	b.WriteString(`,"changes":`)
	b.WriteString(strconv.Itoa(additions + deletions))
	b.WriteString(`}`)
	return b.Bytes()
}

// lineDiffCounts counts added/removed lines between the old and new blob.
func lineDiffCounts(r *Repo, d treeDiff) (additions, deletions int) {
	var oldData, newData []byte
	if d.OldOID != "" {
		if obj := r.objects[d.OldOID]; obj != nil {
			oldData = obj.Data
		}
	}
	if d.NewOID != "" {
		if obj := r.objects[d.NewOID]; obj != nil {
			newData = obj.Data
		}
	}
	oldLines := countLines(oldData)
	newLines := countLines(newData)
	// Count lines present in one but not the other (multiset difference).
	oldCount := make(map[string]int)
	for _, ln := range splitLines(oldData) {
		oldCount[ln]++
	}
	newCount := make(map[string]int)
	for _, ln := range splitLines(newData) {
		newCount[ln]++
	}
	for ln, n := range newCount {
		if oldCount[ln] < n {
			additions += n - oldCount[ln]
		}
	}
	for ln, n := range oldCount {
		if newCount[ln] < n {
			deletions += n - newCount[ln]
		}
	}
	_ = oldLines
	_ = newLines
	if d.Status == "added" && additions == 0 {
		additions = len(splitLines(newData))
		if additions == 0 {
			additions = 1
		}
	}
	if additions == 0 && deletions == 0 && d.Status != "deleted" {
		additions = 1
	}
	return additions, deletions
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	var out []string
	start := 0
	for i, c := range data {
		if c == '\n' {
			out = append(out, string(data[start:i]))
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, string(data[start:]))
	}
	return out
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := 0
	for _, c := range data {
		if c == '\n' {
			n++
		}
	}
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// serializeBranch renders a branch entry for GET /branches.
func serializeBranch(name, sha string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"name":`)
	escapeJSONString(&b, name)
	b.WriteString(`,"commit":{"sha":`)
	escapeJSONString(&b, sha)
	b.WriteString(`}}`)
	return b.Bytes()
}

// serializeCommit renders a commit entry for GET /commits.
func serializeCommit(oid string, cv CommitView) []byte {
	var b bytes.Buffer
	b.WriteString(`{"sha":`)
	escapeJSONString(&b, oid)
	b.WriteString(`,"commit":{"message":`)
	escapeJSONString(&b, cv.Message)
	b.WriteString(`,"author":{"name":`)
	escapeJSONString(&b, authorName(cv.Author))
	b.WriteString(`},"committer":{"name":`)
	escapeJSONString(&b, authorName(cv.Committer))
	b.WriteString(`}},"author":`)
	b.Write(userJSON(viewerUser))
	b.WriteString(`}`)
	return b.Bytes()
}

func authorName(ident string) string {
	// "Name <email> timestamp tz" -> "Name".
	if i := indexByte(ident, '<'); i > 0 {
		return trimSpace(ident[:i])
	}
	return ident
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// serializeContentFile renders a contents-API file entry.
func serializeContentFile(r *Repo, path, sha string, size int, content []byte) []byte {
	var b bytes.Buffer
	b.WriteString(`{"type":"file","encoding":"base64","name":`)
	escapeJSONString(&b, baseName(path))
	b.WriteString(`,"path":`)
	escapeJSONString(&b, path)
	b.WriteString(`,"sha":`)
	escapeJSONString(&b, sha)
	b.WriteString(`,"size":`)
	b.WriteString(strconv.Itoa(size))
	b.WriteString(`,"content":`)
	escapeJSONString(&b, base64.StdEncoding.EncodeToString(content))
	b.WriteString(`}`)
	return b.Bytes()
}

func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

// serializeContentDir renders a contents-API directory listing.
func serializeContentDir(r *Repo, dirPath string, entries []TreeEntry) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for i, e := range sorted {
		if i > 0 {
			b.WriteByte(',')
		}
		typ := "file"
		if e.Mode == "40000" {
			typ = "dir"
		}
		size := 0
		if typ == "file" {
			if obj := r.objects[e.OID]; obj != nil {
				size = len(obj.Data)
			}
		}
		b.WriteString(`{"type":`)
		escapeJSONString(&b, typ)
		b.WriteString(`,"name":`)
		escapeJSONString(&b, e.Name)
		b.WriteString(`,"path":`)
		if dirPath == "" {
			escapeJSONString(&b, e.Name)
		} else {
			escapeJSONString(&b, dirPath+"/"+e.Name)
		}
		b.WriteString(`,"sha":`)
		escapeJSONString(&b, e.OID)
		b.WriteString(`,"size":`)
		b.WriteString(strconv.Itoa(size))
		b.WriteString(`}`)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// errorJSON renders {"message": "..."}.
func errorJSON(msg string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"message":`)
	escapeJSONString(&b, msg)
	b.WriteString(`}`)
	return b.Bytes()
}

var _ = fmt.Sprintf
