// GitHub GraphQL v4 executor for the fixed document set the suite sends
// (gh's query shapes). A small tokenizer + recursive-descent parser + a
// resolver over the in-memory store. Not on a timed hot path.
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------

type gqlTokKind int

const (
	tokEOF gqlTokKind = iota
	tokName
	tokString
	tokInt
	tokFloat
	tokPunct
	tokSpread // ...
)

type gqlTok struct {
	kind gqlTokKind
	text string
}

func gqlTokenize(src string) ([]gqlTok, bool) {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			// Comment to end of line.
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' || c == '$' || c == '!' || c == '=' || c == '@':
			toks = append(toks, gqlTok{tokPunct, string(c)})
			i++
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlTok{tokSpread, "..."})
			i += 3
		case c == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				return nil, false
			}
			toks = append(toks, gqlTok{tokString, src[i+1 : j]})
			i = j + 1
		case c >= '0' && c <= '9' || c == '-':
			j := i
			isFloat := false
			for j < len(src) {
				d := src[j]
				if d >= '0' && d <= '9' || d == '-' || d == '+' {
					j++
				} else if d == '.' || d == 'e' || d == 'E' {
					isFloat = true
					j++
				} else {
					break
				}
			}
			if isFloat {
				toks = append(toks, gqlTok{tokFloat, src[i:j]})
			} else {
				toks = append(toks, gqlTok{tokInt, src[i:j]})
			}
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(src) {
				d := src[j]
				if d == '_' || d >= 'a' && d <= 'z' || d >= 'A' && d <= 'Z' || d >= '0' && d <= '9' {
					j++
				} else {
					break
				}
			}
			toks = append(toks, gqlTok{tokName, src[i:j]})
			i = j
		default:
			return nil, false
		}
	}
	toks = append(toks, gqlTok{tokEOF, ""})
	return toks, true
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type gqlArgVal struct {
	kind string // var, string, int, float, bool, enum, list, object, null
	s    string
	f    float64
	list []gqlArgVal
	obj  map[string]gqlArgVal
}

type gqlField struct {
	alias    string
	name     string
	args     map[string]gqlArgVal
	sel      []*gqlField
	fragType string // set for inline fragments
	isFrag   bool
}

type gqlParser struct {
	toks []gqlTok
	i    int
}

func (p *gqlParser) peek() gqlTok { return p.toks[p.i] }
func (p *gqlParser) next() gqlTok {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}
func (p *gqlParser) expectPunct(s string) bool {
	if p.peek().kind == tokPunct && p.peek().text == s {
		p.next()
		return true
	}
	return false
}
func (p *gqlParser) expectName() (string, bool) {
	if p.peek().kind == tokName {
		return p.next().text, true
	}
	return "", false
}

// parseDocument parses one operation: optional query/mutation keyword, optional
// name, optional variable definitions, then the root selection set.
func gqlParseDocument(src string) ([]*gqlField, bool) {
	toks, ok := gqlTokenize(src)
	if !ok {
		return nil, false
	}
	p := &gqlParser{toks: toks}
	// Optional operation type keyword.
	if p.peek().kind == tokName && (p.peek().text == "query" || p.peek().text == "mutation" || p.peek().text == "subscription") {
		p.next()
		// Optional operation name.
		if p.peek().kind == tokName {
			p.next()
		}
		// Optional variable definitions.
		if p.peek().kind == tokPunct && p.peek().text == "(" {
			if !p.skipBalanced('(', ')') {
				return nil, false
			}
		}
	}
	sel, ok := p.parseSelectionSet()
	if !ok {
		return nil, false
	}
	return sel, true
}

func (p *gqlParser) skipBalanced(open, close byte) bool {
	depth := 0
	for {
		t := p.next()
		if t.kind == tokEOF {
			return false
		}
		if t.kind == tokPunct {
			if len(t.text) == 1 && t.text[0] == open {
				depth++
			} else if len(t.text) == 1 && t.text[0] == close {
				depth--
				if depth == 0 {
					return true
				}
			}
		}
	}
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, bool) {
	if !p.expectPunct("{") {
		return nil, false
	}
	var fields []*gqlField
	for {
		t := p.peek()
		if t.kind == tokPunct && t.text == "}" {
			p.next()
			return fields, true
		}
		if t.kind == tokEOF {
			return nil, false
		}
		f, ok := p.parseField()
		if !ok {
			return nil, false
		}
		fields = append(fields, f)
	}
}

func (p *gqlParser) parseField() (*gqlField, bool) {
	// Inline fragment: ... on Type { ... }
	if p.peek().kind == tokSpread {
		p.next()
		if p.peek().kind == tokName && p.peek().text == "on" {
			p.next()
			typ, ok := p.expectName()
			if !ok {
				return nil, false
			}
			sel, ok := p.parseSelectionSet()
			if !ok {
				return nil, false
			}
			return &gqlField{isFrag: true, fragType: typ, sel: sel}, true
		}
		// A named fragment spread (not used by the suite): skip its name.
		if _, ok := p.expectName(); !ok {
			return nil, false
		}
		return &gqlField{isFrag: true, fragType: ""}, true
	}
	name, ok := p.expectName()
	if !ok {
		return nil, false
	}
	f := &gqlField{name: name, alias: name}
	// Alias: name : name
	if p.peek().kind == tokPunct && p.peek().text == ":" {
		p.next()
		real, ok := p.expectName()
		if !ok {
			return nil, false
		}
		f.alias = name
		f.name = real
	}
	// Arguments.
	if p.peek().kind == tokPunct && p.peek().text == "(" {
		p.next()
		args := make(map[string]gqlArgVal)
		for {
			if p.peek().kind == tokPunct && p.peek().text == ")" {
				p.next()
				break
			}
			argName, ok := p.expectName()
			if !ok {
				return nil, false
			}
			if !p.expectPunct(":") {
				return nil, false
			}
			v, ok := p.parseValue()
			if !ok {
				return nil, false
			}
			args[argName] = v
			if p.peek().kind == tokPunct && p.peek().text == "," {
				p.next()
			}
		}
		f.args = args
	}
	// Nested selection set.
	if p.peek().kind == tokPunct && p.peek().text == "{" {
		sel, ok := p.parseSelectionSet()
		if !ok {
			return nil, false
		}
		f.sel = sel
	}
	// Directives (@include/@skip): skip them.
	for p.peek().kind == tokPunct && p.peek().text == "@" {
		p.next()
		if _, ok := p.expectName(); !ok {
			return nil, false
		}
		if p.peek().kind == tokPunct && p.peek().text == "(" {
			if !p.skipBalanced('(', ')') {
				return nil, false
			}
		}
	}
	return f, true
}

func (p *gqlParser) parseValue() (gqlArgVal, bool) {
	t := p.peek()
	switch t.kind {
	case tokPunct:
		if t.text == "$" {
			p.next()
			name, ok := p.expectName()
			if !ok {
				return gqlArgVal{}, false
			}
			return gqlArgVal{kind: "var", s: name}, true
		}
		if t.text == "[" {
			p.next()
			var list []gqlArgVal
			for {
				if p.peek().kind == tokPunct && p.peek().text == "]" {
					p.next()
					return gqlArgVal{kind: "list", list: list}, true
				}
				v, ok := p.parseValue()
				if !ok {
					return gqlArgVal{}, false
				}
				list = append(list, v)
				if p.peek().kind == tokPunct && p.peek().text == "," {
					p.next()
				}
			}
		}
		if t.text == "{" {
			p.next()
			obj := make(map[string]gqlArgVal)
			for {
				if p.peek().kind == tokPunct && p.peek().text == "}" {
					p.next()
					return gqlArgVal{kind: "object", obj: obj}, true
				}
				k, ok := p.expectName()
				if !ok {
					return gqlArgVal{}, false
				}
				if !p.expectPunct(":") {
					return gqlArgVal{}, false
				}
				v, ok := p.parseValue()
				if !ok {
					return gqlArgVal{}, false
				}
				obj[k] = v
				if p.peek().kind == tokPunct && p.peek().text == "," {
					p.next()
				}
			}
		}
	case tokString:
		p.next()
		return gqlArgVal{kind: "string", s: t.text}, true
	case tokInt:
		p.next()
		f, _ := strconv.ParseFloat(t.text, 64)
		return gqlArgVal{kind: "int", f: f, s: t.text}, true
	case tokFloat:
		p.next()
		f, _ := strconv.ParseFloat(t.text, 64)
		return gqlArgVal{kind: "float", f: f}, true
	case tokName:
		p.next()
		switch t.text {
		case "true":
			return gqlArgVal{kind: "bool", s: "true"}, true
		case "false":
			return gqlArgVal{kind: "bool", s: "false"}, true
		case "null":
			return gqlArgVal{kind: "null"}, true
		default:
			return gqlArgVal{kind: "enum", s: t.text}, true
		}
	}
	return gqlArgVal{}, false
}

// resolveArg substitutes variables into a parsed argument value.
func resolveArg(v gqlArgVal, vars map[string]any) any {
	switch v.kind {
	case "var":
		if vars == nil {
			return nil
		}
		return vars[v.s]
	case "string", "enum":
		return v.s
	case "int", "float":
		return v.f
	case "bool":
		return v.s == "true"
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, e := range v.list {
			out = append(out, resolveArg(e, vars))
		}
		return out
	case "object":
		out := make(map[string]any, len(v.obj))
		for k, e := range v.obj {
			out[k] = resolveArg(e, vars)
		}
		return out
	}
	return nil
}

func argString(args map[string]gqlArgVal, name string, vars map[string]any) (string, bool) {
	v, ok := args[name]
	if !ok {
		return "", false
	}
	rv := resolveArg(v, vars)
	s, ok := rv.(string)
	return s, ok
}

func argInt(args map[string]gqlArgVal, name string, vars map[string]any) (int, bool) {
	v, ok := args[name]
	if !ok {
		return 0, false
	}
	rv := resolveArg(v, vars)
	f, ok := rv.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

func argMap(args map[string]gqlArgVal, name string, vars map[string]any) (map[string]any, bool) {
	v, ok := args[name]
	if !ok {
		return nil, false
	}
	rv := resolveArg(v, vars)
	m, ok := rv.(map[string]any)
	return m, ok
}

// ---------------------------------------------------------------------------
// Execution context and value model
// ---------------------------------------------------------------------------

type gqlCtx struct {
	s      *Server
	vars   map[string]any
	errors []string
}

func (c *gqlCtx) errf(format string, args ...any) {
	c.errors = append(c.errors, fmt.Sprintf(format, args...))
}

// A resolved field value.
type gqlVal struct {
	kind   string // scalar, object, list, null
	scalar any    // string, int, bool
	obj    any    // a typed object pointer
	list   []gqlVal
}

// Object wrapper types for connections and scalars the store lacks.
type gqlIssueConn struct {
	repo   *Repo
	issues []*Issue
	cursor string // the end cursor ("" when no page)
	more   bool
}

type gqlPRConn struct {
	pulls []*Pull
}

type gqlFilesConn struct {
	paths []string
}

type gqlFileNode struct {
	path string
}

type gqlPageInfo struct {
	hasNext bool
	end     string
}

type gqlRef struct {
	name string
}

type gqlOwner struct {
	login string
}

type gqlTypeInfo struct {
	name       string
	fields     []string
	enumValues []string
}

func gqlTypeName(obj any) string {
	switch obj.(type) {
	case *gqlQuery:
		return "Query"
	case *gqlMutation:
		return "Mutation"
	case *User:
		return "User"
	case *Repo:
		return "Repository"
	case *Issue:
		return "Issue"
	case *Pull:
		return "PullRequest"
	case *gqlTypeInfo:
		return "__Type"
	case *gqlIssueConn:
		return "IssueConnection"
	case *gqlPRConn:
		return "PullRequestConnection"
	case *gqlFilesConn:
		return "PullRequestFileConnection"
	case *gqlFileNode:
		return "PullRequestFile"
	case *gqlPageInfo:
		return "PageInfo"
	case *gqlRef:
		return "Ref"
	case *gqlOwner:
		return "User"
	case *gqlMutationResult:
		return "MutationPayload"
	case *gqlCommentEdge:
		return "IssueCommentEdge"
	case *gqlCommentNode:
		return "IssueComment"
	case *gqlFieldInfo:
		return "__Field"
	}
	return ""
}

type gqlQuery struct{}
type gqlMutation struct{}

// ---------------------------------------------------------------------------
// HTTP entry point
// ---------------------------------------------------------------------------

func handleGraphQL(s *Server, req *Request) *Response {
	if !s.requireAuth(req) {
		return unauthorized()
	}
	if req.Method != "POST" {
		return jsonError(405, "Method Not Allowed")
	}
	body, ok := parseJSON(req.Body)
	if !ok {
		return &Response{Status: 400, ContentType: "application/json", Body: []byte(`{"errors":[{"message":"request body is not valid JSON"}]}`)}
	}
	// Batched operations: a JSON array of {query, variables}.
	if arr, isArr := body.([]any); isArr {
		var out bytes.Buffer
		out.WriteByte('[')
		for i, op := range arr {
			if i > 0 {
				out.WriteByte(',')
			}
			out.Write(gqlExecuteOne(s, op))
		}
		out.WriteByte(']')
		return &Response{Status: 200, ContentType: "application/json", Body: out.Bytes()}
	}
	return &Response{Status: 200, ContentType: "application/json", Body: gqlExecuteOne(s, body)}
}

func gqlExecuteOne(s *Server, op any) []byte {
	m, ok := op.(map[string]any)
	if !ok {
		return []byte(`{"data":null,"errors":[{"message":"operation must be an object"}]}`)
	}
	query, _ := jsonString(jsonGet(m, "query"))
	if query == "" {
		return []byte(`{"data":null,"errors":[{"message":"query is required"}]}`)
	}
	vars, _ := jsonGet(m, "variables").(map[string]any)
	sel, ok := gqlParseDocument(query)
	if !ok {
		return []byte(`{"data":null,"errors":[{"message":"could not parse the query document"}]}`)
	}
	isMutation := strings.HasPrefix(strings.TrimSpace(query), "mutation")
	ctx := &gqlCtx{s: s, vars: vars}
	var root any
	if isMutation {
		root = &gqlMutation{}
	} else {
		root = &gqlQuery{}
	}
	var buf bytes.Buffer
	buf.WriteString(`{"data":`)
	writeGQLSelection(&buf, root, sel, ctx)
	if len(ctx.errors) > 0 {
		buf.WriteString(`,"errors":[`)
		for i, e := range ctx.errors {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(`{"message":`)
			writeJSONString(&buf, e)
			buf.WriteByte('}')
		}
		buf.WriteByte(']')
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// writeGQLSelection writes a JSON object for the selection set applied to obj.
func writeGQLSelection(buf *bytes.Buffer, obj any, sel []*gqlField, ctx *gqlCtx) {
	buf.WriteByte('{')
	first := true
	for _, f := range sel {
		if f.isFrag {
			// Inline fragment: include its fields when the type matches.
			if f.fragType != "" && gqlTypeName(obj) != f.fragType {
				continue
			}
			// Write the fragment's fields inline.
			var fragBuf bytes.Buffer
			writeGQLSelection(&fragBuf, obj, f.sel, ctx)
			inner := fragBuf.Bytes()
			if len(inner) > 2 {
				if !first {
					buf.WriteByte(',')
				}
				buf.Write(inner[1 : len(inner)-1])
				first = false
			}
			continue
		}
		val, ok := gqlResolve(obj, f, ctx)
		if !ok {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		writeJSONString(buf, f.alias)
		buf.WriteByte(':')
		writeGQLValue(buf, val, f.sel, ctx)
	}
	buf.WriteByte('}')
}

func writeGQLValue(buf *bytes.Buffer, v gqlVal, sel []*gqlField, ctx *gqlCtx) {
	switch v.kind {
	case "null":
		buf.WriteString("null")
	case "scalar":
		writeJSONScalar(buf, v.scalar)
	case "object":
		if len(sel) == 0 {
			buf.WriteString("null")
			return
		}
		writeGQLSelection(buf, v.obj, sel, ctx)
	case "list":
		buf.WriteByte('[')
		for i, item := range v.list {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeGQLValue(buf, item, sel, ctx)
		}
		buf.WriteByte(']')
	}
}

func writeJSONScalar(buf *bytes.Buffer, v any) {
	switch t := v.(type) {
	case string:
		writeJSONString(buf, t)
	case int:
		buf.WriteString(strconv.Itoa(t))
	case int64:
		buf.WriteString(strconv.FormatInt(t, 10))
	case float64:
		buf.WriteString(strconv.FormatFloat(t, 'f', -1, 64))
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case nil:
		buf.WriteString("null")
	default:
		writeJSONString(buf, fmt.Sprintf("%v", v))
	}
}

func writeJSONString(buf *bytes.Buffer, s string) {
	escapeJSONString(buf, s)
}

// ---------------------------------------------------------------------------
// Resolver
// ---------------------------------------------------------------------------

func gqlResolve(obj any, f *gqlField, ctx *gqlCtx) (gqlVal, bool) {
	switch o := obj.(type) {
	case *gqlMutationResult:
		if v, ok := o.fields[f.name]; ok {
			switch t := v.(type) {
			case *Issue:
				return gqlVal{kind: "object", obj: t}, true
			case *Pull:
				return gqlVal{kind: "object", obj: t}, true
			case *gqlCommentEdge:
				return gqlVal{kind: "object", obj: t}, true
			}
		}
		ctx.errf("Cannot query field \"%s\" on mutation result", f.name)
		return gqlVal{kind: "null"}, false
	case *gqlCommentEdge:
		if f.name == "node" {
			return gqlVal{kind: "object", obj: o.node}, true
		}
	case *gqlCommentNode:
		switch f.name {
		case "id":
			return gqlVal{kind: "scalar", scalar: o.id}, true
		case "body":
			return gqlVal{kind: "scalar", scalar: o.body}, true
		}
	case *gqlQuery:
		return gqlResolveQuery(o, f, ctx)
	case *gqlMutation:
		return gqlResolveMutation(o, f, ctx)
	case *User:
		if f.name == "login" {
			return gqlVal{kind: "scalar", scalar: o.Login}, true
		}
	case *Repo:
		return gqlResolveRepo(o, f, ctx)
	case *Issue:
		switch f.name {
		case "id":
			return gqlVal{kind: "scalar", scalar: issueGlobalID(ctx.repoOf(o), o.Number)}, true
		case "number":
			return gqlVal{kind: "scalar", scalar: o.Number}, true
		case "title":
			return gqlVal{kind: "scalar", scalar: o.Title}, true
		case "state":
			return gqlVal{kind: "scalar", scalar: strings.ToUpper(o.State)}, true
		case "body":
			return gqlVal{kind: "scalar", scalar: o.Body}, true
		}
	case *Pull:
		switch f.name {
		case "id":
			return gqlVal{kind: "scalar", scalar: pullGlobalID(ctx.repoOfPull(o), o.Number)}, true
		case "number":
			return gqlVal{kind: "scalar", scalar: o.Number}, true
		case "headRefName":
			return gqlVal{kind: "scalar", scalar: o.Head}, true
		case "baseRefName":
			return gqlVal{kind: "scalar", scalar: o.Base}, true
		case "mergeable":
			if gqlPullMergeable(ctx, o) {
				return gqlVal{kind: "scalar", scalar: "MERGEABLE"}, true
			}
			return gqlVal{kind: "scalar", scalar: "CONFLICTING"}, true
		case "state":
			if o.Merged {
				return gqlVal{kind: "scalar", scalar: "MERGED"}, true
			}
			return gqlVal{kind: "scalar", scalar: strings.ToUpper(o.State)}, true
		case "merged":
			return gqlVal{kind: "scalar", scalar: o.Merged}, true
		case "files":
			paths := gqlPullFiles(ctx, o)
			return gqlVal{kind: "object", obj: &gqlFilesConn{paths: paths}}, true
		}
	case *gqlTypeInfo:
		switch f.name {
		case "name":
			return gqlVal{kind: "scalar", scalar: o.name}, true
		case "fields":
			var list []gqlVal
			for _, name := range o.fields {
				list = append(list, gqlVal{kind: "object", obj: &gqlFieldInfo{name: name}})
			}
			return gqlVal{kind: "list", list: list}, true
		case "enumValues":
			var list []gqlVal
			for _, name := range o.enumValues {
				list = append(list, gqlVal{kind: "object", obj: &gqlFieldInfo{name: name}})
			}
			return gqlVal{kind: "list", list: list}, true
		}
	case *gqlFieldInfo:
		if f.name == "name" {
			return gqlVal{kind: "scalar", scalar: o.name}, true
		}
	case *gqlIssueConn:
		switch f.name {
		case "nodes":
			var list []gqlVal
			for _, iss := range o.issues {
				list = append(list, gqlVal{kind: "object", obj: iss})
			}
			return gqlVal{kind: "list", list: list}, true
		case "pageInfo":
			return gqlVal{kind: "object", obj: &gqlPageInfo{hasNext: o.more, end: o.cursor}}, true
		}
	case *gqlPRConn:
		if f.name == "nodes" {
			var list []gqlVal
			for _, p := range o.pulls {
				list = append(list, gqlVal{kind: "object", obj: p})
			}
			return gqlVal{kind: "list", list: list}, true
		}
	case *gqlFilesConn:
		if f.name == "nodes" {
			var list []gqlVal
			for _, path := range o.paths {
				list = append(list, gqlVal{kind: "object", obj: &gqlFileNode{path: path}})
			}
			return gqlVal{kind: "list", list: list}, true
		}
	case *gqlFileNode:
		if f.name == "path" {
			return gqlVal{kind: "scalar", scalar: o.path}, true
		}
	case *gqlPageInfo:
		switch f.name {
		case "hasNextPage":
			return gqlVal{kind: "scalar", scalar: o.hasNext}, true
		case "endCursor":
			return gqlVal{kind: "scalar", scalar: o.end}, true
		}
	case *gqlRef:
		if f.name == "name" {
			return gqlVal{kind: "scalar", scalar: o.name}, true
		}
	case *gqlOwner:
		if f.name == "login" {
			return gqlVal{kind: "scalar", scalar: o.login}, true
		}
	}
	ctx.errf("Cannot query field \"%s\" on type \"%s\"", f.name, gqlTypeName(obj))
	return gqlVal{kind: "null"}, false
}

type gqlFieldInfo struct {
	name string
}

func gqlResolveQuery(_ *gqlQuery, f *gqlField, ctx *gqlCtx) (gqlVal, bool) {
	switch f.name {
	case "viewer":
		return gqlVal{kind: "object", obj: viewerUser}, true
	case "repository":
		owner, _ := argString(f.args, "owner", ctx.vars)
		name, _ := argString(f.args, "name", ctx.vars)
		r := ctx.s.store.getRepo(owner, name)
		if r == nil {
			return gqlVal{kind: "null"}, true
		}
		return gqlVal{kind: "object", obj: r}, true
	case "node":
		id, _ := argString(f.args, "id", ctx.vars)
		obj := gqlNodeByID(ctx, id)
		if obj == nil {
			return gqlVal{kind: "null"}, true
		}
		return gqlVal{kind: "object", obj: obj}, true
	case "__type":
		name, _ := argString(f.args, "name", ctx.vars)
		info := gqlTypeByName(name)
		if info == nil {
			return gqlVal{kind: "null"}, true
		}
		return gqlVal{kind: "object", obj: info}, true
	}
	ctx.errf("Cannot query field \"%s\" on type \"Query\"", f.name)
	return gqlVal{kind: "null"}, false
}

func gqlResolveRepo(r *Repo, f *gqlField, ctx *gqlCtx) (gqlVal, bool) {
	switch f.name {
	case "id":
		return gqlVal{kind: "scalar", scalar: repoGlobalID(r)}, true
	case "databaseId":
		return gqlVal{kind: "scalar", scalar: r.ID}, true
	case "name":
		return gqlVal{kind: "scalar", scalar: r.Name}, true
	case "nameWithOwner":
		return gqlVal{kind: "scalar", scalar: r.Owner + "/" + r.Name}, true
	case "owner":
		return gqlVal{kind: "object", obj: &gqlOwner{login: r.Owner}}, true
	case "defaultBranchRef":
		return gqlVal{kind: "object", obj: &gqlRef{name: r.DefaultBranch}}, true
	case "viewerPermission":
		return gqlVal{kind: "scalar", scalar: "ADMIN"}, true
	case "hasIssuesEnabled":
		return gqlVal{kind: "scalar", scalar: true}, true
	case "issue":
		num, ok := argInt(f.args, "number", ctx.vars)
		if !ok {
			ctx.errf("issue requires a number argument")
			return gqlVal{kind: "null"}, false
		}
		iss := r.getIssue(num)
		if iss == nil {
			return gqlVal{kind: "null"}, true
		}
		return gqlVal{kind: "object", obj: iss}, true
	case "issues":
		return gqlVal{kind: "object", obj: gqlIssuesConn(r, f, ctx)}, true
	case "pullRequests":
		first, _ := argInt(f.args, "first", ctx.vars)
		pulls := r.listPulls("all")
		if first > 0 && len(pulls) > first {
			pulls = pulls[:first]
		}
		return gqlVal{kind: "object", obj: &gqlPRConn{pulls: pulls}}, true
	case "pullRequest":
		num, ok := argInt(f.args, "number", ctx.vars)
		if !ok {
			ctx.errf("pullRequest requires a number argument")
			return gqlVal{kind: "null"}, false
		}
		p := r.getPull(num)
		if p == nil {
			return gqlVal{kind: "null"}, true
		}
		return gqlVal{kind: "object", obj: p}, true
	}
	ctx.errf("Cannot query field \"%s\" on type \"Repository\"", f.name)
	return gqlVal{kind: "null"}, false
}

// gqlIssuesConn builds the issues connection for a repository.issues field,
// honouring first/states/orderBy/after. Order is CREATED_AT DESC (newest first)
// unless orderBy says otherwise. The cursor is the last issue's number.
func gqlIssuesConn(r *Repo, f *gqlField, ctx *gqlCtx) *gqlIssueConn {
	first, _ := argInt(f.args, "first", ctx.vars)
	// states: a list of enum names (OPEN/CLOSED).
	var wantStates map[string]bool
	if v, ok := f.args["states"]; ok {
		rv := resolveArg(v, ctx.vars)
		if list, isList := rv.([]any); isList {
			wantStates = make(map[string]bool)
			for _, e := range list {
				if s, ok := e.(string); ok {
					wantStates[strings.ToLower(s)] = true
				}
			}
		}
	}
	// orderBy: {field, direction}
	desc := true
	if v, ok := f.args["orderBy"]; ok {
		if m, isMap := resolveArg(v, ctx.vars).(map[string]any); isMap {
			if d, ok := m["direction"].(string); ok {
				desc = strings.ToUpper(d) == "DESC"
			}
		}
	}
	// after: a cursor holding an issue number.
	afterNum := 0
	if s, ok := argString(f.args, "after", ctx.vars); ok && s != "" {
		if n, err := decodeGQLCursor(s); err == nil {
			afterNum = n
		}
	}
	// All issues in the requested order.
	var all []*Issue
	if desc {
		all = r.listIssues("all", "desc")
	} else {
		all = r.listIssues("all", "asc")
	}
	var filtered []*Issue
	for _, iss := range all {
		if wantStates != nil && !wantStates[iss.State] {
			continue
		}
		if afterNum != 0 {
			if desc && iss.Number >= afterNum {
				continue
			}
			if !desc && iss.Number <= afterNum {
				continue
			}
		}
		filtered = append(filtered, iss)
	}
	more := false
	if first > 0 && len(filtered) > first {
		more = true
		filtered = filtered[:first]
	}
	cursor := ""
	if len(filtered) > 0 {
		cursor = encodeGQLCursor(filtered[len(filtered)-1].Number)
	}
	return &gqlIssueConn{repo: r, issues: filtered, cursor: cursor, more: more}
}

func encodeGQLCursor(num int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(num)))
}

func decodeGQLCursor(s string) (int, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(b), "cursor:"))
	return n, err
}

func gqlTypeByName(name string) *gqlTypeInfo {
	switch name {
	case "Issue":
		return &gqlTypeInfo{name: "Issue", fields: []string{"id", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "labels", "comments", "url"}}
	case "PullRequest":
		return &gqlTypeInfo{name: "PullRequest", fields: []string{"id", "number", "title", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "body", "createdAt", "author"}}
	case "Repository":
		return &gqlTypeInfo{name: "Repository", fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate", "url"}}
	case "SearchType":
		return &gqlTypeInfo{name: "SearchType", enumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"}}
	case "User":
		return &gqlTypeInfo{name: "User", fields: []string{"id", "login", "name", "email"}}
	}
	return nil
}

// gqlNodeByID decodes a global id and returns the object it names.
func gqlNodeByID(ctx *gqlCtx, id string) any {
	kind, key, ok := decodeGlobalID(id)
	if !ok {
		return nil
	}
	switch kind {
	case "Repository":
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 {
			return nil
		}
		return ctx.s.store.getRepo(parts[0], parts[1])
	case "Issue":
		owner, name, num := splitGlobalIssueKey(key)
		if owner == "" {
			return nil
		}
		r := ctx.s.store.getRepo(owner, name)
		if r == nil {
			return nil
		}
		return r.getIssue(num)
	case "PullRequest":
		owner, name, num := splitGlobalIssueKey(key)
		if owner == "" {
			return nil
		}
		r := ctx.s.store.getRepo(owner, name)
		if r == nil {
			return nil
		}
		return r.getPull(num)
	}
	return nil
}

func decodeGlobalID(id string) (kind, key string, ok bool) {
	b, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(b), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func splitGlobalIssueKey(key string) (owner, name string, num int) {
	// The key is "owner/name:number" (issueGlobalID uses a colon).
	colon := strings.LastIndex(key, ":")
	if colon < 0 {
		return "", "", 0
	}
	n, err := strconv.Atoi(key[colon+1:])
	if err != nil {
		return "", "", 0
	}
	parts := strings.SplitN(key[:colon], "/", 2)
	if len(parts) != 2 {
		return "", "", 0
	}
	return parts[0], parts[1], n
}

// repoOf returns the repository an issue belongs to (issues carry no repo
// pointer, so this scans the store; the suite has few repos).
func (c *gqlCtx) repoOf(iss *Issue) *Repo {
	for _, r := range c.s.store.listViewerRepos() {
		if r.getIssue(iss.Number) == iss {
			return r
		}
	}
	return nil
}

func (c *gqlCtx) repoOfPull(p *Pull) *Repo {
	for _, r := range c.s.store.listViewerRepos() {
		if r.getPull(p.Number) == p {
			return r
		}
	}
	return nil
}

func gqlPullFiles(ctx *gqlCtx, p *Pull) []string {
	r := ctx.repoOfPull(p)
	if r == nil {
		return nil
	}
	mb := r.mergeBase(p.HeadSHA, p.BaseSHA)
	if mb == "" {
		return nil
	}
	mbTree, _ := r.commitTree(mb)
	headTree, _ := r.commitTree(p.HeadSHA)
	var paths []string
	for _, d := range r.diffTrees(mbTree, headTree) {
		paths = append(paths, d.Path)
	}
	return paths
}

func gqlPullMergeable(ctx *gqlCtx, p *Pull) bool {
	r := ctx.repoOfPull(p)
	if r == nil {
		return false
	}
	if p.Merged {
		return false
	}
	// A PR is mergeable when a squash merge would not conflict.
	return !pullWouldConflict(r, p)
}

// pullWouldConflict reports whether merging p would hit a file conflict.
func pullWouldConflict(r *Repo, p *Pull) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	baseTip, ok := r.refs["refs/heads/"+p.Base]
	if !ok {
		return true
	}
	headTip, ok := r.refs["refs/heads/"+p.Head]
	if !ok {
		return true
	}
	mb := r.mergeBase(headTip, baseTip)
	if mb == "" {
		return true
	}
	mbTree, _ := r.commitTree(mb)
	headTree, _ := r.commitTree(headTip)
	baseTree, _ := r.commitTree(baseTip)
	for _, d := range r.diffTrees(mbTree, headTree) {
		baseOID, _, baseHas := r.treeEntryAt(baseTree, d.Path)
		mbOID, _, _ := r.treeEntryAt(mbTree, d.Path)
		if baseHas && baseOID != mbOID && baseOID != d.NewOID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

func gqlResolveMutation(_ *gqlMutation, f *gqlField, ctx *gqlCtx) (gqlVal, bool) {
	input, _ := argMap(f.args, "input", ctx.vars)
	switch f.name {
	case "createIssue":
		return gqlMutCreateIssue(f, input, ctx)
	case "closeIssue":
		return gqlMutCloseIssue(f, input, ctx)
	case "createPullRequest":
		return gqlMutCreatePull(f, input, ctx)
	case "mergePullRequest":
		return gqlMutMergePull(f, input, ctx)
	case "addComment":
		return gqlMutAddComment(f, input, ctx)
	}
	ctx.errf("Cannot query field \"%s\" on type \"Mutation\"", f.name)
	return gqlVal{kind: "null"}, false
}

// mutationResult builds the {field: {...}} object the mutation's selection
// reads. The inner object is a small map-like value the resolver reads via
// the gqlMutationResult type.
type gqlMutationResult struct {
	fields map[string]any // name -> *Issue | *Pull | *gqlCommentEdge | scalar
}

type gqlCommentEdge struct {
	node *gqlCommentNode
}

type gqlCommentNode struct {
	id   string
	body string
}

func gqlMutCreateIssue(f *gqlField, input map[string]any, ctx *gqlCtx) (gqlVal, bool) {
	repoID, _ := input["repositoryId"].(string)
	title, _ := input["title"].(string)
	body, _ := input["body"].(string)
	if repoID == "" || title == "" {
		ctx.errf("createIssue requires repositoryId and title")
		return gqlVal{kind: "null"}, false
	}
	kind, key, ok := decodeGlobalID(repoID)
	if !ok || kind != "Repository" {
		ctx.errf("createIssue: unknown repositoryId")
		return gqlVal{kind: "null"}, false
	}
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		ctx.errf("createIssue: unknown repositoryId")
		return gqlVal{kind: "null"}, false
	}
	r := ctx.s.store.getRepo(parts[0], parts[1])
	if r == nil {
		ctx.errf("createIssue: repository not found")
		return gqlVal{kind: "null"}, false
	}
	seq := ctx.s.store.nextSeq()
	iss := r.createIssue(title, body, nil, seq)
	res := &gqlMutationResult{fields: map[string]any{"issue": iss}}
	return gqlVal{kind: "object", obj: res}, true
}

func gqlMutCloseIssue(f *gqlField, input map[string]any, ctx *gqlCtx) (gqlVal, bool) {
	issueID, _ := input["issueId"].(string)
	kind, key, ok := decodeGlobalID(issueID)
	if !ok || kind != "Issue" {
		ctx.errf("closeIssue: unknown issueId")
		return gqlVal{kind: "null"}, false
	}
	owner, name, num := splitGlobalIssueKey(key)
	if owner == "" {
		ctx.errf("closeIssue: unknown issueId")
		return gqlVal{kind: "null"}, false
	}
	r := ctx.s.store.getRepo(owner, name)
	if r == nil {
		ctx.errf("closeIssue: repository not found")
		return gqlVal{kind: "null"}, false
	}
	iss := r.setIssueState(num, "closed")
	if iss == nil {
		ctx.errf("closeIssue: issue not found")
		return gqlVal{kind: "null"}, false
	}
	res := &gqlMutationResult{fields: map[string]any{"issue": iss}}
	return gqlVal{kind: "object", obj: res}, true
}

func gqlMutCreatePull(f *gqlField, input map[string]any, ctx *gqlCtx) (gqlVal, bool) {
	repoID, _ := input["repositoryId"].(string)
	base, _ := input["baseRefName"].(string)
	head, _ := input["headRefName"].(string)
	title, _ := input["title"].(string)
	kind, key, ok := decodeGlobalID(repoID)
	if !ok || kind != "Repository" {
		ctx.errf("createPullRequest: unknown repositoryId")
		return gqlVal{kind: "null"}, false
	}
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		ctx.errf("createPullRequest: unknown repositoryId")
		return gqlVal{kind: "null"}, false
	}
	r := ctx.s.store.getRepo(parts[0], parts[1])
	if r == nil {
		ctx.errf("createPullRequest: repository not found")
		return gqlVal{kind: "null"}, false
	}
	headSHA, ok := r.getRef("refs/heads/" + head)
	if !ok {
		ctx.errf("createPullRequest: head branch does not exist")
		return gqlVal{kind: "null"}, false
	}
	baseSHA, ok := r.getRef("refs/heads/" + base)
	if !ok {
		ctx.errf("createPullRequest: base branch does not exist")
		return gqlVal{kind: "null"}, false
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
	seq := ctx.s.store.nextSeq()
	p := r.createPull(title, "", head, base, headSHA, baseSHA, changed, seq)
	res := &gqlMutationResult{fields: map[string]any{"pullRequest": p}}
	return gqlVal{kind: "object", obj: res}, true
}

func gqlMutMergePull(f *gqlField, input map[string]any, ctx *gqlCtx) (gqlVal, bool) {
	prID, _ := input["pullRequestId"].(string)
	mergeMethod, _ := input["mergeMethod"].(string)
	if mergeMethod != "" && mergeMethod != "SQUASH" && mergeMethod != "MERGE" && mergeMethod != "REBASE" {
		ctx.errf("mergePullRequest: unsupported mergeMethod")
		return gqlVal{kind: "null"}, false
	}
	kind, key, ok := decodeGlobalID(prID)
	if !ok || kind != "PullRequest" {
		ctx.errf("mergePullRequest: unknown pullRequestId")
		return gqlVal{kind: "null"}, false
	}
	owner, name, num := splitGlobalIssueKey(key)
	if owner == "" {
		ctx.errf("mergePullRequest: unknown pullRequestId")
		return gqlVal{kind: "null"}, false
	}
	r := ctx.s.store.getRepo(owner, name)
	if r == nil {
		ctx.errf("mergePullRequest: repository not found")
		return gqlVal{kind: "null"}, false
	}
	p := r.getPull(num)
	if p == nil {
		ctx.errf("mergePullRequest: pull request not found")
		return gqlVal{kind: "null"}, false
	}
	if p.Merged {
		ctx.errf("mergePullRequest: already merged")
		return gqlVal{kind: "null"}, false
	}
	sha, status := mergePullRequestInternal(ctx.s.store, r, p)
	if status == "conflict" {
		ctx.errf("mergePullRequest: merge conflict")
		return gqlVal{kind: "null"}, false
	}
	if status != "ok" {
		ctx.errf("mergePullRequest: %s", status)
		return gqlVal{kind: "null"}, false
	}
	_ = sha
	res := &gqlMutationResult{fields: map[string]any{"pullRequest": p}}
	return gqlVal{kind: "object", obj: res}, true
}

func gqlMutAddComment(f *gqlField, input map[string]any, ctx *gqlCtx) (gqlVal, bool) {
	subjectID, _ := input["subjectId"].(string)
	body, _ := input["body"].(string)
	kind, key, ok := decodeGlobalID(subjectID)
	if !ok || kind != "Issue" {
		ctx.errf("addComment: unknown subjectId")
		return gqlVal{kind: "null"}, false
	}
	owner, name, num := splitGlobalIssueKey(key)
	if owner == "" {
		ctx.errf("addComment: unknown subjectId")
		return gqlVal{kind: "null"}, false
	}
	r := ctx.s.store.getRepo(owner, name)
	if r == nil {
		ctx.errf("addComment: repository not found")
		return gqlVal{kind: "null"}, false
	}
	seq := ctx.s.store.nextSeq()
	c := r.addIssueComment(num, body, seq)
	if c == nil {
		ctx.errf("addComment: issue not found")
		return gqlVal{kind: "null"}, false
	}
	node := &gqlCommentNode{id: commentGlobalID(r, num, c.ID), body: body}
	res := &gqlMutationResult{fields: map[string]any{"commentEdge": &gqlCommentEdge{node: node}}}
	return gqlVal{kind: "object", obj: res}, true
}
