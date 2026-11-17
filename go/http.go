// Hand-rolled HTTP/1.1 over raw TCP: single-pass request-line and header
// parsing with a precomputed byte class table, keep-alive connections, and
// zero-copy body handling on the hot paths. Standard library only.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// byteClass classifies bytes for the request parser.
var byteClass [256]uint8

const (
	bcToken = 1 << iota // valid in a token (method / header name)
	bcSpace
	bcDigit
	bcHex
)

func init() {
	for c := 0; c < 256; c++ {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '!' || c == '#' || c == '$' || c == '%' || c == '&' || c == '\'' ||
			c == '*' || c == '+' || c == '-' || c == '.' || c == '^' || c == '_' ||
			c == '`' || c == '|' || c == '~' {
			byteClass[c] |= bcToken
		}
		if c == ' ' || c == '\t' {
			byteClass[c] |= bcSpace
		}
		if c >= '0' && c <= '9' {
			byteClass[c] |= bcDigit
		}
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			byteClass[c] |= bcHex
		}
	}
}

// Request is one parsed HTTP request. Header values are the fields the
// handlers need; anything else is ignored (no allocation for it).
type Request struct {
	Method      string
	Path        string // decoded path, no query
	RawPath     string
	Query       string
	Version     string
	Host        string
	Auth        string // bearer token, "" when absent
	ContentType string
	Accept      string
	GitProtocol string
	ContentLen  int64
	Chunked     bool
	Close       bool
	Body        []byte
	KeepAlive   bool
}

// Response is one HTTP response to write.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
	Extra       [][2]string // extra headers, name/value
	NoBody      bool        // HEAD-like: headers only
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	}
	return "Status"
}

type Server struct {
	store   *Store
	token   string
	ln      net.Listener
	reqID   int64
	handler func(w *ConnWriter, req *Request)
}

type ConnWriter struct {
	w *bufio.Writer
}

var bufioReaderPool = sync.Pool{New: func() interface{} { return bufio.NewReaderSize(nil, 65536) }}
var bufioWriterPool = sync.Pool{New: func() interface{} { return bufio.NewWriterSize(nil, 65536) }}

// serveConn handles one TCP connection for its whole keep-alive lifetime.
func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	br := bufioReaderPool.Get().(*bufio.Reader)
	br.Reset(conn)
	defer bufioReaderPool.Put(br)
	bw := bufioWriterPool.Get().(*bufio.Writer)
	bw.Reset(conn)
	defer bufioWriterPool.Put(bw)

	for {
		req, err := readRequest(br)
		if err != nil {
			return
		}
		resp := s.dispatch(req)
		if !writeResponse(bw, req, resp, s.nextReqID()) {
			return
		}
		if !req.KeepAlive || resp.NoBody && false {
			return
		}
		if req.Close {
			return
		}
	}
}

func (s *Server) nextReqID() string {
	n := atomic.AddInt64(&s.reqID, 1)
	return "req-" + strconv.FormatInt(n, 10)
}

func (s *Server) dispatch(req *Request) *Response {
	// Route by path. The handler table lives in router.go's dispatch.
	return routeRequest(s, req)
}

// readRequest reads and parses one request (head + body).
func readRequest(br *bufio.Reader) (*Request, error) {
	// Read the head: up to \r\n\r\n, bounded.
	head, err := readHead(br)
	if err != nil {
		return nil, err
	}
	req := &Request{ContentLen: -1}
	// Request line: METHOD SP PATH SP VERSION CRLF
	sp1 := bytes.IndexByte(head, ' ')
	if sp1 < 0 {
		return nil, errBadRequest
	}
	req.Method = string(head[:sp1])
	rest := head[sp1+1:]
	sp2 := bytes.IndexByte(rest, ' ')
	if sp2 < 0 {
		return nil, errBadRequest
	}
	rawTarget := string(rest[:sp2])
	// The version is the rest of the request line only, up to the first CRLF.
	ver := rest[sp2+1:]
	if i := bytes.IndexByte(ver, '\n'); i >= 0 {
		ver = ver[:i]
	}
	req.Version = string(bytes.TrimRight(ver, "\r\n"))
	// Split query.
	if q := strings.IndexByte(rawTarget, '?'); q >= 0 {
		req.RawPath = rawTarget[:q]
		req.Query = rawTarget[q+1:]
	} else {
		req.RawPath = rawTarget
	}
	req.Path = decodePath(req.RawPath)
	// Headers: lines after the request line.
	lines := bytes.Split(head, []byte("\r\n"))
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := bytes.ToLower(line[:colon])
		value := bytes.Trim(line[colon+1:], " \t")
		switch string(name) {
		case "host":
			req.Host = string(value)
		case "authorization":
			if bytes.HasPrefix(value, []byte("Bearer ")) {
				req.Auth = string(value[7:])
			} else {
				req.Auth = string(value)
			}
		case "content-length":
			n, _ := strconv.ParseInt(string(value), 10, 64)
			req.ContentLen = n
		case "content-type":
			req.ContentType = string(value)
		case "accept":
			req.Accept = string(value)
		case "git-protocol":
			req.GitProtocol = string(value)
		case "transfer-encoding":
			if bytes.EqualFold(value, []byte("chunked")) {
				req.Chunked = true
			}
		case "connection":
			if bytes.EqualFold(value, []byte("close")) {
				req.Close = true
			}
		}
	}
	// Keep-alive: HTTP/1.1 defaults to keep-alive unless Connection: close.
	req.KeepAlive = req.Version == "HTTP/1.1" && !req.Close
	// Body.
	if req.Chunked {
		body, err := readChunked(br)
		if err != nil {
			return nil, err
		}
		req.Body = body
	} else if req.ContentLen > 0 {
		if req.ContentLen > 64<<20 {
			return nil, errBadRequest
		}
		body := make([]byte, req.ContentLen)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, err
		}
		req.Body = body
	}
	return req, nil
}

var errBadRequest = fmt.Errorf("bad request")

// readHead reads bytes until \r\n\r\n (or \n\n), bounded at 64 KiB.
func readHead(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for buf.Len() < 65536 {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		if len(line) >= 2 && line[len(line)-2] == '\r' && line[len(line)-1] == '\n' {
			// End of head when we see an empty line.
			if buf.Len() >= 2 {
				b := buf.Bytes()
				if len(b) >= 4 && bytes.HasSuffix(b, []byte("\r\n\r\n")) {
					return b, nil
				}
				if len(b) >= 2 && bytes.HasSuffix(b, []byte("\n\n")) {
					return b, nil
				}
			}
			// Check for the empty line: the line just read is "\r\n" alone.
			if len(line) == 2 {
				return buf.Bytes(), nil
			}
		} else if len(line) == 1 {
			return buf.Bytes(), nil
		}
	}
	return nil, errBadRequest
}

// readChunked reads a chunked transfer-encoded body.
func readChunked(br *bufio.Reader) ([]byte, error) {
	var out bytes.Buffer
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		// Strip chunk extensions.
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = line[:semi]
		}
		size, err := strconv.ParseInt(string(line), 16, 64)
		if err != nil {
			return nil, errBadRequest
		}
		if size == 0 {
			// Read the trailing CRLF and any trailers until an empty line.
			for {
				trailer, err := br.ReadBytes('\n')
				if err != nil {
					return nil, err
				}
				if len(trailer) <= 2 {
					break
				}
			}
			return out.Bytes(), nil
		}
		if size > 64<<20 {
			return nil, errBadRequest
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		out.Write(chunk)
		// Trailing CRLF after the chunk.
		if _, err := br.ReadBytes('\n'); err != nil {
			return nil, err
		}
	}
}

// decodePath percent-decodes a URL path.
func decodePath(p string) string {
	if !strings.Contains(p, "%") {
		return p
	}
	var out bytes.Buffer
	for i := 0; i < len(p); i++ {
		if p[i] == '%' && i+2 < len(p) && isHex(p[i+1]) && isHex(p[i+2]) {
			out.WriteByte(hexVal(p[i+1])<<4 | hexVal(p[i+2]))
			i += 2
		} else {
			out.WriteByte(p[i])
		}
	}
	return out.String()
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// writeResponse writes the status line, headers and body. Returns false when
// the connection should close.
func writeResponse(bw *bufio.Writer, req *Request, resp *Response, reqID string) bool {
	var hb bytes.Buffer
	hb.WriteString(req.Version)
	if hb.Len() == 0 {
		hb.WriteString("HTTP/1.1")
	}
	hb.WriteByte(' ')
	hb.WriteString(strconv.Itoa(resp.Status))
	hb.WriteByte(' ')
	hb.WriteString(statusText(resp.Status))
	hb.WriteString("\r\n")
	if resp.ContentType != "" {
		hb.WriteString("Content-Type: ")
		hb.WriteString(resp.ContentType)
		hb.WriteString("\r\n")
	}
	hb.WriteString("Content-Length: ")
	hb.WriteString(strconv.Itoa(len(resp.Body)))
	hb.WriteString("\r\n")
	hb.WriteString("X-GitHub-Request-Id: ")
	hb.WriteString(reqID)
	hb.WriteString("\r\n")
	for _, h := range resp.Extra {
		hb.WriteString(h[0])
		hb.WriteString(": ")
		hb.WriteString(h[1])
		hb.WriteString("\r\n")
	}
	if req.Close || !req.KeepAlive {
		hb.WriteString("Connection: close\r\n")
	} else {
		hb.WriteString("Connection: keep-alive\r\n")
	}
	hb.WriteString("\r\n")
	if _, err := bw.Write(hb.Bytes()); err != nil {
		return false
	}
	if len(resp.Body) > 0 && !resp.NoBody {
		if _, err := bw.Write(resp.Body); err != nil {
			return false
		}
	}
	if err := bw.Flush(); err != nil {
		return false
	}
	return true
}

// queryParam returns the first value of a query parameter.
func queryParam(query, name string) (string, bool) {
	for query != "" {
		var pair string
		if i := strings.IndexByte(query, '&'); i >= 0 {
			pair, query = query[:i], query[i+1:]
		} else {
			pair, query = query, ""
		}
		if i := strings.IndexByte(pair, '='); i >= 0 {
			if pair[:i] == name {
				return decodeQuery(pair[i+1:]), true
			}
		} else if pair == name {
			return "", true
		}
	}
	return "", false
}

func decodeQuery(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	return decodePath(s)
}

// splitPath splits a path into its segments (without empty segments).
func splitPath(p string) []string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}
