package upstream

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"

	"golang.org/x/net/proxy"
)

// Upstream identifies a remote proxy.
type Upstream struct {
	Protocol string // "http" or "socks5"
	Host     string // ip:port
	Username string
	Password string
}

func (u *Upstream) Key() string {
	return fmt.Sprintf("%s://%s:%s@%s", u.Protocol, u.Username, u.Password, u.Host)
}

// Dial connects to target through the upstream proxy.
func Dial(ctx context.Context, up *Upstream, target string) (net.Conn, error) {
	switch up.Protocol {
	case "socks5":
		return dialSOCKS5(ctx, up, target)
	case "http", "https":
		return dialHTTPProxy(ctx, up, target)
	default:
		return nil, fmt.Errorf("unsupported upstream protocol: %s", up.Protocol)
	}
}

func dialSOCKS5(ctx context.Context, up *Upstream, target string) (net.Conn, error) {
	auth := &proxy.Auth{User: up.Username, Password: up.Password}
	dialer, err := proxy.SOCKS5("tcp", up.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("socks5 dialer: %w", err)
	}

	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := dialer.Dial("tcp", target)
		ch <- result{conn, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.conn, r.err
	}
}

func dialHTTPProxy(ctx context.Context, up *Upstream, target string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", up.Host)
	if err != nil {
		return nil, fmt.Errorf("connect to http proxy: %w", err)
	}

	cred := base64.StdEncoding.EncodeToString([]byte(up.Username + ":" + up.Password))
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n",
		target, target, cred)

	if _, err := fmt.Fprint(conn, req); err != nil {
		conn.Close()
		return nil, err
	}

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, err
	}

	resp := string(buf[:n])
	if len(resp) < 12 || resp[9:12] != "200" {
		conn.Close()
		return nil, fmt.Errorf("http proxy CONNECT failed: %s", resp)
	}

	return conn, nil
}
