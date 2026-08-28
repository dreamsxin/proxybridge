package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"

	"dbridge/internal/auth"
	"dbridge/internal/center"
	"dbridge/internal/forward"
	"dbridge/internal/upstream"
)

// HTTPServer handles inbound HTTP and HTTPS (CONNECT tunnel) proxy connections.
type HTTPServer struct {
	authenticator *auth.Authenticator
	pool          *upstream.Pool
}

func (s *HTTPServer) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	clientIP := conn.RemoteAddr().String()
	br := bufio.NewReader(conn)

	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}

	// extract credentials from Proxy-Authorization header
	username, password, ok := parseProxyAuth(req.Header.Get("Proxy-Authorization"))
	if !ok {
		writeHTTPError(conn, http.StatusProxyAuthRequired, "Proxy Authentication Required")
		return
	}

	authResp, err := s.authenticator.Authenticate(ctx, username, password, clientIP)
	if err != nil {
		switch err {
		case auth.ErrBanned:
			writeHTTPError(conn, http.StatusForbidden, "Forbidden")
		case auth.ErrAuthFailed:
			writeHTTPError(conn, http.StatusProxyAuthRequired, "Invalid credentials")
		default:
			writeHTTPError(conn, http.StatusServiceUnavailable, "Service unavailable")
		}
		return
	}

	up := authResponseToUpstream(authResp)
	handle, err := s.pool.Acquire(up, authResp.SpeedLimit)
	if err != nil {
		writeHTTPError(conn, http.StatusServiceUnavailable, err.Error())
		return
	}

	if req.Method == http.MethodConnect {
		s.handleCONNECT(ctx, conn, req.Host, handle)
	} else {
		s.handleHTTP(ctx, conn, req, handle)
	}
}

func (s *HTTPServer) handleCONNECT(ctx context.Context, client net.Conn, target string, handle *upstream.ConnHandle) {
	remote, err := s.pool.Dial(ctx, handle, target)
	if err != nil {
		handle.Release()
		fmt.Fprint(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	forward.Relay(ctx, client, remote, handle)
}

func (s *HTTPServer) handleHTTP(ctx context.Context, client net.Conn, req *http.Request, handle *upstream.ConnHandle) {
	// for plain HTTP, determine target from request
	host := req.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}

	remote, err := s.pool.Dial(ctx, handle, host)
	if err != nil {
		handle.Release()
		writeHTTPError(client, http.StatusBadGateway, "upstream unreachable")
		return
	}

	// forward the original request to upstream
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")
	if err := req.Write(remote); err != nil {
		handle.Release()
		remote.Close()
		return
	}

	forward.Relay(ctx, client, remote, handle)
}

func parseProxyAuth(header string) (username, password string, ok bool) {
	if !strings.HasPrefix(header, "Basic ") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func writeHTTPError(conn net.Conn, code int, msg string) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", code, msg)
}

func authResponseToUpstream(r *center.AuthResponse) *upstream.Upstream {
	return &upstream.Upstream{
		Protocol: r.Protocol,
		Host:     fmt.Sprintf("%s:%d", r.IP, r.Port),
		Username: r.ProxyUser,
		Password: r.ProxyPass,
	}
}
