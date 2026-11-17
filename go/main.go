// The forge server: a static Go core (stdlib only) serving git smart HTTP,
// REST v3, GraphQL, and MCP over a hand-rolled HTTP/1.1 layer.
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	port := 0
	if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			port = n
		}
	}
	if len(os.Args) > 1 {
		if n, err := strconv.Atoi(os.Args[1]); err == nil && n > 0 {
			port = n
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: listen: %v\n", err)
		os.Exit(1)
	}
	addr := ln.Addr().(*net.TCPAddr)
	s := &Server{store: newStore(), token: token, ln: ln}
	// The launcher reads this line to learn the ready port.
	fmt.Printf("PORT=%d\n", addr.Port)
	_ = os.Stdout.Sync()
	serve(s)
}

// serve accepts connections for the server's lifetime.
func serve(s *Server) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serveConn(conn)
	}
}

// routeRequest dispatches one parsed request to the right surface.
func routeRequest(s *Server, req *Request) *Response {
	path := req.Path
	if path == "/healthz" {
		if req.Method != "GET" {
			return jsonError(405, "Method Not Allowed")
		}
		return &Response{Status: 200, ContentType: "text/plain", Body: []byte("ok")}
	}
	if path == "/api/graphql" {
		return handleGraphQL(s, req)
	}
	if path == "/mcp" {
		return handleMCP(s, req)
	}
	if strings.HasPrefix(path, "/api/v3") {
		return handleREST(s, req)
	}
	// Git smart HTTP: /{owner}/{repo}.git/...
	if segs := splitPath(path); len(segs) >= 2 && strings.HasSuffix(segs[1], ".git") {
		return handleGit(s, req, segs[0], strings.TrimSuffix(segs[1], ".git"), segs[2:])
	}
	return notFound("Not Found")
}
