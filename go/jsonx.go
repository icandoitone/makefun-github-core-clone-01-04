// Fast, allocation-light JSON helpers for the hot paths: a string-field
// extractor for small request bodies and a byte escaper for response building.
package main

import (
	"bytes"
	"strconv"
	"strings"
)

// jsonStringField extracts the string value of a top-level field from a JSON
// object body. It handles \" escapes and returns ok=false when the field is
// absent or not a string. This avoids a full unmarshal on the write hot path.
func jsonStringField(body []byte, field string) (string, bool) {
	needle := []byte(`"` + field + `"`)
	idx := bytes.Index(body, needle)
	if idx < 0 {
		return "", false
	}
	rest := body[idx+len(needle):]
	// Skip whitespace and the colon.
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
		i++
	}
	if i >= len(rest) || rest[i] != ':' {
		return "", false
	}
	i++
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
		i++
	}
	if i >= len(rest) || rest[i] != '"' {
		return "", false
	}
	i++
	var sb strings.Builder
	for i < len(rest) {
		c := rest[i]
		if c == '"' {
			return sb.String(), true
		}
		if c == '\\' && i+1 < len(rest) {
			n := rest[i+1]
			switch n {
			case '"':
				sb.WriteByte('"')
				i += 2
			case '\\':
				sb.WriteByte('\\')
				i += 2
			case '/':
				sb.WriteByte('/')
				i += 2
			case 'n':
				sb.WriteByte('\n')
				i += 2
			case 't':
				sb.WriteByte('\t')
				i += 2
			case 'r':
				sb.WriteByte('\r')
				i += 2
			case 'b':
				sb.WriteByte('\b')
				i += 2
			case 'f':
				sb.WriteByte('\f')
				i += 2
			case 'u':
				if i+5 < len(rest) {
					hexStr := string(rest[i+2 : i+6])
					if v, err := strconv.ParseUint(hexStr, 16, 32); err == nil {
						sb.WriteRune(rune(v))
						i += 6
						continue
					}
				}
				return "", false
			default:
				return "", false
			}
			continue
		}
		sb.WriteByte(c)
		i++
	}
	return "", false
}

// jsonIntField extracts an integer top-level field; ok=false when absent.
func jsonIntField(body []byte, field string) (int64, bool) {
	needle := []byte(`"` + field + `"`)
	idx := bytes.Index(body, needle)
	if idx < 0 {
		return 0, false
	}
	rest := body[idx+len(needle):]
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
		i++
	}
	if i >= len(rest) || rest[i] != ':' {
		return 0, false
	}
	i++
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
		i++
	}
	j := i
	if j < len(rest) && (rest[j] == '-' || rest[j] == '+') {
		j++
	}
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == i {
		return 0, false
	}
	v, err := strconv.ParseInt(string(rest[i:j]), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// jsonBoolField extracts a boolean top-level field; ok=false when absent.
func jsonBoolField(body []byte, field string) (bool, bool) {
	needle := []byte(`"` + field + `"`)
	idx := bytes.Index(body, needle)
	if idx < 0 {
		return false, false
	}
	rest := body[idx+len(needle):]
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
		i++
	}
	if i >= len(rest) || rest[i] != ':' {
		return false, false
	}
	i++
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
		i++
	}
	if bytes.HasPrefix(rest[i:], []byte("true")) {
		return true, true
	}
	if bytes.HasPrefix(rest[i:], []byte("false")) {
		return false, true
	}
	return false, false
}

// jsonHasField reports whether a top-level field name appears in the body.
func jsonHasField(body []byte, field string) bool {
	return bytes.Index(body, []byte(`"`+field+`"`)) >= 0
}

// jsonStringArrayField extracts a top-level array of strings (e.g. labels).
func jsonStringArrayField(body []byte, field string) ([]string, bool) {
	needle := []byte(`"` + field + `"`)
	idx := bytes.Index(body, needle)
	if idx < 0 {
		return nil, false
	}
	rest := body[idx+len(needle):]
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
		i++
	}
	if i >= len(rest) || rest[i] != ':' {
		return nil, false
	}
	i++
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
		i++
	}
	if i >= len(rest) || rest[i] != '[' {
		return nil, false
	}
	i++
	var out []string
	for i < len(rest) {
		for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r' || rest[i] == ',') {
			i++
		}
		if i < len(rest) && rest[i] == ']' {
			return out, true
		}
		if i >= len(rest) || rest[i] != '"' {
			return nil, false
		}
		i++
		var sb strings.Builder
		ok := false
		for i < len(rest) {
			c := rest[i]
			if c == '"' {
				ok = true
				i++
				break
			}
			if c == '\\' && i+1 < len(rest) {
				n := rest[i+1]
				switch n {
				case '"':
					sb.WriteByte('"')
					i += 2
				case '\\':
					sb.WriteByte('\\')
					i += 2
				case 'n':
					sb.WriteByte('\n')
					i += 2
				case 't':
					sb.WriteByte('\t')
					i += 2
				case 'r':
					sb.WriteByte('\r')
					i += 2
				default:
					sb.WriteByte(n)
					i += 2
				}
				continue
			}
			sb.WriteByte(c)
			i++
		}
		if !ok {
			return nil, false
		}
		out = append(out, sb.String())
	}
	return nil, false
}

// escapeJSONString appends s as a JSON string literal (with quotes) to buf.
func escapeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if c < 0x20 {
				buf.WriteString(`\u00`)
				const hexd = "0123456789abcdef"
				buf.WriteByte(hexd[c>>4])
				buf.WriteByte(hexd[c&0xf])
			} else {
				buf.WriteByte(c)
			}
		}
	}
	buf.WriteByte('"')
}

// jsonQuote returns s as a JSON string literal.
func jsonQuote(s string) string {
	var buf bytes.Buffer
	escapeJSONString(&buf, s)
	return buf.String()
}
