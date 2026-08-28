package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"

	"dbridge/internal/auth"
	"dbridge/internal/upstream"
)

// Server listens on a single port and routes connections to HTTP or SOCKS5
// handlers based on the first byte of the connection.
type Server struct {
	addr          string
	authenticator *auth.Authenticator
	pool          *upstream.Pool
}

func NewServer(addr string, a *auth.Authenticator, p *upstream.Pool) *Server {
	return &Server{addr: addr, authenticator: a, pool: p}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	slog.Info("proxy listening", "addr", s.addr)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	http := &HTTPServer{authenticator: s.authenticator, pool: s.pool}
	socks := &SOCKS5Server{authenticator: s.authenticator, pool: s.pool}

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				slog.Warn("accept error", "err", err)
				continue
			}
		}
		go s.dispatch(ctx, conn, http, socks)
	}
}

func (s *Server) dispatch(ctx context.Context, conn net.Conn, http *HTTPServer, socks *SOCKS5Server) {
	// peek first byte to detect protocol
	buf := make([]byte, 1)
	if _, err := io.ReadFull(conn, buf); err != nil {
		conn.Close()
		return
	}

	pc := &peekedConn{Conn: conn, buf: buf}

	if buf[0] == 0x05 {
		socks.handle(ctx, pc)
	} else {
		http.handle(ctx, pc)
	}
}

// peekedConn wraps a net.Conn and prepends already-read bytes back into Read.
type peekedConn struct {
	net.Conn
	buf []byte
}

func (c *peekedConn) Read(b []byte) (int, error) {
	if len(c.buf) > 0 {
		n := copy(b, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}
