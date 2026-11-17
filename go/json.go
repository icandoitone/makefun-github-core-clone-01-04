// A small generic JSON parser producing Go values (map[string]any, []any,
// string, float64, bool, nil). Used for GraphQL and MCP request bodies, which
// are not on a timed hot path. Allocation is acceptable there.
package main

import (
	"bytes"
	"strconv"
	"strings"
)

type jsonParser struct {
	b []byte
	i int
}

// parseJSON parses a JSON value. Returns nil, false on malformed input.
func parseJSON(b []byte) (any, bool) {
	p := &jsonParser{b: b}
	p.skipWS()
	v, ok := p.value()
	if !ok {
		return nil, false
	}
	p.skipWS()
	if p.i != len(p.b) {
		return nil, false
	}
	return v, true
}

func (p *jsonParser) skipWS() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jsonParser) value() (any, bool) {
	if p.i >= len(p.b) {
		return nil, false
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, ok := p.str()
		return s, ok
	case c == 't':
		if p.i+4 <= len(p.b) && string(p.b[p.i:p.i+4]) == "true" {
			p.i += 4
			return true, true
		}
		return nil, false
	case c == 'f':
		if p.i+5 <= len(p.b) && string(p.b[p.i:p.i+5]) == "false" {
			p.i += 5
			return false, true
		}
		return nil, false
	case c == 'n':
		if p.i+4 <= len(p.b) && string(p.b[p.i:p.i+4]) == "null" {
			p.i += 4
			return nil, true
		}
		return nil, false
	default:
		return p.number()
	}
}

func (p *jsonParser) object() (any, bool) {
	p.i++ // {
	m := make(map[string]any)
	p.skipWS()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return m, true
	}
	for {
		p.skipWS()
		k, ok := p.str()
		if !ok {
			return nil, false
		}
		p.skipWS()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, false
		}
		p.i++
		p.skipWS()
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		m[k] = v
		p.skipWS()
		if p.i >= len(p.b) {
			return nil, false
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return m, true
		default:
			return nil, false
		}
	}
}

func (p *jsonParser) array() (any, bool) {
	p.i++ // [
	var arr []any
	p.skipWS()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return arr, true
	}
	for {
		p.skipWS()
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		arr = append(arr, v)
		p.skipWS()
		if p.i >= len(p.b) {
			return nil, false
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return arr, true
		default:
			return nil, false
		}
	}
}

func (p *jsonParser) str() (string, bool) {
	if p.i >= len(p.b) || p.b[p.i] != '"' {
		return "", false
	}
	p.i++
	start := p.i
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			s := string(p.b[start:p.i])
			p.i++
			return s, true
		}
		if c == '\\' {
			// A string with escapes: fall back to a slow path.
			return p.strEscaped(start)
		}
		p.i++
	}
	return "", false
}

func (p *jsonParser) strEscaped(start int) (string, bool) {
	// p.i is at the backslash; start is the first content byte.
	var sb strings.Builder
	sb.Write(p.b[start:p.i])
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			p.i++
			return sb.String(), true
		}
		if c != '\\' {
			sb.WriteByte(c)
			p.i++
			continue
		}
		p.i++
		if p.i >= len(p.b) {
			return "", false
		}
		switch e := p.b[p.i]; e {
		case '"', '\\', '/':
			sb.WriteByte(e)
			p.i++
		case 'b':
			sb.WriteByte('\b')
			p.i++
		case 'f':
			sb.WriteByte('\f')
			p.i++
		case 'n':
			sb.WriteByte('\n')
			p.i++
		case 'r':
			sb.WriteByte('\r')
			p.i++
		case 't':
			sb.WriteByte('\t')
			p.i++
		case 'u':
			if p.i+4 >= len(p.b) {
				return "", false
			}
			n, err := strconv.ParseUint(string(p.b[p.i+1:p.i+5]), 16, 32)
			if err != nil {
				return "", false
			}
			sb.WriteRune(rune(n))
			p.i += 5
		default:
			return "", false
		}
	}
	return "", false
}

func (p *jsonParser) number() (any, bool) {
	start := p.i
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.i++
		} else {
			break
		}
	}
	if p.i == start {
		return nil, false
	}
	f, err := strconv.ParseFloat(string(p.b[start:p.i]), 64)
	if err != nil {
		return nil, false
	}
	return f, true
}

// marshalJSONValue serializes a parsed JSON value back to JSON. Used to echo
// a JSON-RPC request id verbatim.
func marshalJSONValue(v any, buf *bytes.Buffer) {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		escapeJSONString(buf, t)
	case float64:
		buf.WriteString(strconv.FormatFloat(t, 'f', -1, 64))
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			marshalJSONValue(e, buf)
		}
		buf.WriteByte(']')
	case map[string]any:
		buf.WriteByte('{')
		first := true
		for k, e := range t {
			if !first {
				buf.WriteByte(',')
			}
			first = false
			escapeJSONString(buf, k)
			buf.WriteByte(':')
			marshalJSONValue(e, buf)
		}
		buf.WriteByte('}')
	default:
		buf.WriteString("null")
	}
}
func jsonGet(v any, keys ...any) any {
	cur := v
	for _, k := range keys {
		switch key := k.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[key]
		case int:
			a, ok := cur.([]any)
			if !ok || key < 0 || key >= len(a) {
				return nil
			}
			cur = a[key]
		default:
			return nil
		}
	}
	return cur
}

func jsonString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func jsonFloat(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func jsonInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

func jsonBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}
