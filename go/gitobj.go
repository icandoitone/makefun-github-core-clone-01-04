// Git object model: SHA-1 ids, tree/commit/tag objects, packfile read/write,
// delta application and pkt-line framing. Standard library only.
package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
)

const zeroOID = "0000000000000000000000000000000000000000"

// Object types and their pack type codes.
const (
	typeCommit = 1
	typeTree   = 2
	typeBlob   = 3
	typeTag    = 4
	typeOfsDelta = 6
	typeRefDelta = 7
)

func typeName(code int) string {
	switch code {
	case typeCommit:
		return "commit"
	case typeTree:
		return "tree"
	case typeBlob:
		return "blob"
	case typeTag:
		return "tag"
	}
	return ""
}

func typeCode(name string) int {
	switch name {
	case "commit":
		return typeCommit
	case "tree":
		return typeTree
	case "blob":
		return typeBlob
	case "tag":
		return typeTag
	}
	return 0
}

// Object is one git object: its type and its exact bytes.
type Object struct {
	Type string
	Data []byte
}

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func isZeroOID(s string) bool { return s == zeroOID }

// hashObject returns git's object id: SHA-1 over "<type> <size>\0" + data.
func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(fmt.Sprintf("%d", len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// oidBytes decodes a 40-hex oid to 20 bytes.
func oidBytes(s string) ([]byte, error) {
	if !isOID(s) {
		return nil, errors.New("bad oid")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// hexOID encodes 20 bytes as lowercase hex.
func hexOID(b []byte) string { return hex.EncodeToString(b) }

// --- trees ---

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// parseTree parses a tree object's entries in stored order.
func parseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry has a truncated object id")
		}
		mode := string(data[off:sp])
		name := string(data[sp+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
		off = nul + 21
	}
	return out, nil
}

// treeSortKey is git's tree-entry sort: a directory compares as if its name ended in '/'.
func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		b, _ := hex.DecodeString(e.OID)
		buf.Write(b)
	}
	return buf.Bytes()
}

// --- commits and tags ---

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) (CommitView, error) {
	var cv CommitView
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
		cv.Message = string(data[blank+2:])
	}
	for _, line := range bytes.Split(data[:headEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key := string(line[:sp])
		val := string(line[sp+1:])
		switch key {
		case "tree":
			cv.Tree = val
		case "parent":
			cv.Parents = append(cv.Parents, val)
		case "author":
			cv.Author = val
		case "committer":
			cv.Committer = val
		}
	}
	if !isOID(cv.Tree) {
		return cv, errors.New("commit has no valid tree header")
	}
	return cv, nil
}

func buildCommit(tree string, parents []string, author, committer, message string, timestamp int64) []byte {
	var buf bytes.Buffer
	buf.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		buf.WriteString("parent " + p + "\n")
	}
	buf.WriteString("author " + author + " " + fmt.Sprintf("%d", timestamp) + " +0000\n")
	buf.WriteString("committer " + committer + " " + fmt.Sprintf("%d", timestamp) + " +0000\n")
	buf.WriteString("\n")
	buf.WriteString(message)
	return buf.Bytes()
}

type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) (TagView, error) {
	var tv TagView
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
		tv.Message = string(data[blank+2:])
	}
	for _, line := range bytes.Split(data[:headEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key := string(line[:sp])
		val := string(line[sp+1:])
		switch key {
		case "object":
			tv.Object = val
		case "type":
			tv.Type = val
		case "tag":
			tv.Tag = val
		case "tagger":
			tv.Tagger = val
		}
	}
	if !isOID(tv.Object) {
		return tv, errors.New("tag has no valid object header")
	}
	return tv, nil
}

func buildTag(object, typ, tag, tagger, message string, timestamp int64) []byte {
	return []byte(fmt.Sprintf("object %s\ntype %s\ntag %s\ntagger %s %d +0000\n\n%s",
		object, typ, tag, tagger, timestamp, message))
}

// --- reachability ---

// collectReachable returns every object reachable from roots.
// lookup returns the object for an oid, or nil.
func collectReachable(roots []string, lookup func(string) *Object) map[string]bool {
	seen := make(map[string]bool)
	var stack []string
	for _, r := range roots {
		if isOID(r) {
			stack = append(stack, r)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case "commit":
			cv, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			stack = append(stack, cv.Tree)
			stack = append(stack, cv.Parents...)
		case "tag":
			tv, err := parseTag(obj.Data)
			if err != nil {
				continue
			}
			stack = append(stack, tv.Object)
		case "tree":
			entries, err := parseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				stack = append(stack, e.OID)
			}
		}
	}
	return seen
}

// reachableWithinDepth returns objects reachable from roots within depth
// generations of commits (depth 1 = only the tip commits, their trees and blobs).
// It also returns the shallow boundary oids (the commits at the depth edge).
func reachableWithinDepth(roots []string, lookup func(string) *Object, depth int) (map[string]bool, []string) {
	seen := make(map[string]bool)
	var shallow []string
	// BFS over commits by generation.
	type item struct {
		oid string
		gen int
	}
	var queue []item
	for _, r := range roots {
		if isOID(r) {
			queue = append(queue, item{r, 0})
		}
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if seen[it.oid] {
			continue
		}
		obj := lookup(it.oid)
		if obj == nil {
			continue
		}
		seen[it.oid] = true
		if obj.Type != "commit" {
			// A non-commit root: include its tree/blobs fully.
			sub := collectReachable([]string{it.oid}, lookup)
			for k := range sub {
				seen[k] = true
			}
			continue
		}
		cv, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		// Include the commit's tree and blobs.
		sub := collectReachable([]string{cv.Tree}, lookup)
		for k := range sub {
			seen[k] = true
		}
		if it.gen+1 >= depth {
			// This commit is at the shallow edge.
			shallow = append(shallow, it.oid)
			continue
		}
		for _, p := range cv.Parents {
			queue = append(queue, item{p, it.gen + 1})
		}
	}
	return seen, shallow
}

// --- packfile ---

var errPack = errors.New("pack error")

// readPackVarint reads a pack entry's type/size varint.
func readPackVarint(b []byte, start int) (size int, first byte, next int, err error) {
	if start >= len(b) {
		return 0, 0, 0, errPack
	}
	by := b[start]
	first = by
	size = int(by & 0x0f)
	shift := uint(4)
	off := start + 1
	for by&0x80 != 0 {
		if off >= len(b) {
			return 0, 0, 0, errPack
		}
		by = b[off]
		off++
		size += int(by&0x7f) << shift
		shift += 7
		if shift > 63 {
			return 0, 0, 0, errPack
		}
	}
	return size, first, off, nil
}

// readOfsBase reads an ofs-delta base offset.
func readOfsBase(b []byte, start int) (baseOff int, next int, err error) {
	if start >= len(b) {
		return 0, 0, errPack
	}
	by := b[start]
	off := start + 1
	val := int(by & 0x7f)
	for by&0x80 != 0 {
		if off >= len(b) {
			return 0, 0, errPack
		}
		by = b[off]
		off++
		val = ((val + 1) << 7) | int(by&0x7f)
	}
	return val, off, nil
}

// inflateFlate inflates one zlib stream starting at offset and reports the
// offset just past the stream. It uses a bytes.Reader (an io.ByteReader), so
// the flate reader consumes exactly the stream's bytes and no more.
func inflateFlate(b []byte, offset int) ([]byte, int, error) {
	if offset >= len(b) {
		return nil, 0, errPack
	}
	br := bytes.NewReader(b[offset:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed := (len(b) - offset) - br.Len()
	return data, offset + consumed, nil
}

// applyDelta applies a git delta to a base object's bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		val := 0
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errPack
			}
			by := delta[off]
			off++
			val += int(by&0x7f) << shift
			shift += 7
			if by&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, errPack
			}
		}
		return val, nil
	}
	srcSize, err := readSize()
	if err != nil {
		return nil, err
	}
	tgtSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errPack
	}
	out := make([]byte, 0, tgtSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			var copyOff, copySize int
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errPack
					}
					copyOff |= int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errPack
					}
					copySize |= int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOff+copySize > len(base) || len(out)+copySize > tgtSize {
				return nil, errPack
			}
			out = append(out, base[copyOff:copyOff+copySize]...)
		} else if op != 0 {
			lit := int(op & 0x7f)
			if off+lit > len(delta) || len(out)+lit > tgtSize {
				return nil, errPack
			}
			out = append(out, delta[off:off+lit]...)
			off += lit
		} else {
			return nil, errPack
		}
	}
	if len(out) != tgtSize {
		return nil, errPack
	}
	return out, nil
}

// packChecksum returns the SHA-1 of a pack's bytes before its trailer.
func packChecksum(pack []byte) string {
	if len(pack) < 20 {
		return ""
	}
	h := sha1.Sum(pack[:len(pack)-20])
	return hex.EncodeToString(h[:])
}

// readPack parses a packfile (version 2/3), resolving deltas, and returns the
// objects it carries. resolveBase supplies a base object a thin pack leaves out.
func readPack(pack []byte, resolveBase func(string) *Object) (map[string]*Object, error) {
	if len(pack) < 32 {
		return nil, errPack
	}
	if string(pack[:4]) != "PACK" {
		return nil, errPack
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, errPack
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	if trailer != packChecksum(pack) {
		return nil, errPack
	}
	type rawEntry struct {
		offset     int
		typeCode   int
		size       int
		data       []byte
		baseOffset int
		baseOID    string
		resolved   *Object
		oid        string
	}
	raw := make([]*rawEntry, 0, count)
	byOffset := make(map[int]*rawEntry)
	off := 12
	for i := 0; i < count; i++ {
		entryOff := off
		size, first, next, err := readPackVarint(pack, off)
		if err != nil {
			return nil, err
		}
		off = next
		typeCode := int((first >> 4) & 0x07)
		e := &rawEntry{offset: entryOff, typeCode: typeCode, size: size}
		if typeCode == typeOfsDelta {
			baseOff, n, err := readOfsBase(pack, off)
			if err != nil {
				return nil, err
			}
			off = n
			if baseOff <= 0 || baseOff > entryOff {
				return nil, errPack
			}
			e.baseOffset = entryOff - baseOff
		} else if typeCode == typeRefDelta {
			if off+20 > trailerStart {
				return nil, errPack
			}
			e.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		} else if typeName(typeCode) == "" {
			return nil, errPack
		}
		data, nextOff, err := inflateFlate(pack, off)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, errPack
		}
		e.data = data
		off = nextOff
		raw = append(raw, e)
		byOffset[entryOff] = e
	}
	if off != trailerStart {
		return nil, errPack
	}
	objects := make(map[string]*Object)
	resolvedByOID := make(map[string]*rawEntry)
	resolving := make(map[int]bool)
	var resolve func(e *rawEntry) (*Object, string, error)
	resolve = func(e *rawEntry) (*Object, string, error) {
		if e.resolved != nil {
			return e.resolved, e.oid, nil
		}
		if resolving[e.offset] {
			return nil, "", errPack
		}
		resolving[e.offset] = true
		var typ string
		var data []byte
		switch e.typeCode {
		case typeOfsDelta:
			baseEntry := byOffset[e.baseOffset]
			if baseEntry == nil {
				return nil, "", errPack
			}
			base, _, err := resolve(baseEntry)
			if err != nil {
				return nil, "", err
			}
			typ = base.Type
			data, err = applyDelta(base.Data, e.data)
			if err != nil {
				return nil, "", err
			}
		case typeRefDelta:
			if baseEntry, ok := resolvedByOID[e.baseOID]; ok {
				base, _, err := resolve(baseEntry)
				if err != nil {
					return nil, "", err
				}
				typ = base.Type
				data, err = applyDelta(base.Data, e.data)
				if err != nil {
					return nil, "", err
				}
			} else {
				ext := resolveBase(e.baseOID)
				if ext == nil {
					return nil, "", errPack
				}
				typ = ext.Type
				var err error
				data, err = applyDelta(ext.Data, e.data)
				if err != nil {
					return nil, "", err
				}
			}
		default:
			typ = typeName(e.typeCode)
			data = e.data
		}
		oid := hashObject(typ, data)
		obj := &Object{Type: typ, Data: data}
		e.resolved = obj
		e.oid = oid
		resolvedByOID[oid] = e
		delete(resolving, e.offset)
		return obj, oid, nil
	}
	for _, e := range raw {
		obj, oid, err := resolve(e)
		if err != nil {
			return nil, err
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = obj
		}
	}
	return objects, nil
}

// writePackObjectHeader encodes one object's pack header (type/size varint).
func writePackObjectHeader(typ string, size int) []byte {
	code := typeCode(typ)
	var out []byte
	remaining := size
	first := byte((code << 4) | (remaining & 0x0f))
	remaining = remaining >> 4
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining = remaining >> 7
	}
	out = append(out, first)
	return out
}

// deflateBytes compresses data with a fast zlib stream.
func deflateBytes(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, flate.BestSpeed)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// writePack builds a packfile from plain objects (no deltas).
func writePack(objects []*Object) []byte {
	var buf bytes.Buffer
	hdr := make([]byte, 12)
	copy(hdr[0:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(objects)))
	buf.Write(hdr)
	for _, obj := range objects {
		buf.Write(writePackObjectHeader(obj.Type, len(obj.Data)))
		buf.Write(deflateBytes(obj.Data))
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	out := make([]byte, 0, len(body)+20)
	out = append(out, body...)
	out = append(out, sum[:]...)
	return out
}

// --- pkt-line ---

// pktLine encodes one data packet: 4 hex length bytes then the payload.
func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, []byte(fmt.Sprintf("%04x", n))...)
	out = append(out, payload...)
	return out
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

// pktLines encodes a mixed sequence; "flush" and "delim" are control packets.
func pktLines(items []interface{}) []byte {
	var buf bytes.Buffer
	for _, it := range items {
		switch v := it.(type) {
		case string:
			if v == "flush" {
				buf.Write(pktFlush())
			} else if v == "delim" {
				buf.Write(pktDelim())
			} else {
				buf.Write(pktLine([]byte(v)))
			}
		case []byte:
			buf.Write(pktLine(v))
		}
	}
	return buf.Bytes()
}

// pktDecode decodes a complete buffer into data payloads and control markers.
// Returns the data payloads in order; flush/delim/response-end are returned as
// the strings "flush", "delim", "response-end".
func pktDecode(buf []byte) ([]interface{}, error) {
	var out []interface{}
	off := 0
	for off < len(buf) {
		if off+4 > len(buf) {
			return nil, errPack
		}
		hdr := string(buf[off : off+4])
		var n int
		_, err := fmt.Sscanf(hdr, "%04x", &n)
		if err != nil {
			return nil, errPack
		}
		if n == 0 {
			out = append(out, "flush")
			off += 4
			continue
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
			return nil, errPack
		}
		out = append(out, buf[off+4:off+n])
		off += n
	}
	return out, nil
}

// sideband wraps data in sideband-64k packets (band byte + payload), chunked
// so no pkt-line exceeds the maximum.
func sidebandPackets(band byte, data []byte) []byte {
	var buf bytes.Buffer
	const maxPayload = 65520 - 4 - 1 // pkt header + band byte
	for len(data) > 0 {
		chunk := maxPayload
		if len(data) < chunk {
			chunk = len(data)
		}
		payload := make([]byte, 0, chunk+1)
		payload = append(payload, band)
		payload = append(payload, data[:chunk]...)
		buf.Write(pktLine(payload))
		data = data[chunk:]
	}
	return buf.Bytes()
}
