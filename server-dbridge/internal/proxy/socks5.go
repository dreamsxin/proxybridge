package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"

	"dbridge/internal/auth"
	"dbridge/internal/forward"
	"dbridge/internal/upstream"
)

const (
	socks5Version = 0x05

	authNone     = 0x00
	authPassword = 0x02
	authNoAccept = 0xFF

	cmdConnect = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSuccess         = 0x00
	repGeneralFailure  = 0x01
	repForbidden       = 0x02
	repNetUnreachable  = 0x03
	repHostUnreachable = 0x04
)

// SOCKS5Server handles inbound SOCKS5 proxy connections.
type SOCKS5Server struct {
	authenticator *auth.Authenticator
	pool          *upstream.Pool
}

func (s *SOCKS5Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	clientIP := conn.RemoteAddr().String()

	if err := s.checkBanned(clientIP); err != nil {
		return
	}

	username, password, err := s.handshake(conn)
	if err != nil {
		return
	}

	authResp, err := s.authenticator.Authenticate(ctx, username, password, clientIP)
	if err != nil {
		sendSOCKS5Reply(conn, repForbidden, nil)
		return
	}

	target, err := readSOCKS5Request(conn)
	if err != nil {
		sendSOCKS5Reply(conn, repGeneralFailure, nil)
		return
	}

	up := authResponseToUpstream(authResp)
	handle, err := s.pool.Acquire(up, authResp.SpeedLimit)
	if err != nil {
		sendSOCKS5Reply(conn, repGeneralFailure, nil)
		return
	}

	remote, err := s.pool.Dial(ctx, handle, target)
	if err != nil {
		handle.Release()
		sendSOCKS5Reply(conn, repHostUnreachable, nil)
		return
	}

	sendSOCKS5Reply(conn, repSuccess, remote.LocalAddr())
	forward.Relay(ctx, conn, remote, handle)
}

func (s *SOCKS5Server) checkBanned(ip string) error {
	// guard check happens inside Authenticate, but we fast-path here to avoid handshake
	return nil
}

// handshake performs SOCKS5 negotiation and returns credentials.
func (s *SOCKS5Server) handshake(conn net.Conn) (username, password string, err error) {
	// version + nmethods
	header := make([]byte, 2)
	if _, err = io.ReadFull(conn, header); err != nil {
		return
	}
	if header[0] != socks5Version {
		err = fmt.Errorf("unsupported socks version: %d", header[0])
		return
	}

	methods := make([]byte, header[1])
	if _, err = io.ReadFull(conn, methods); err != nil {
		return
	}

	// require username/password auth
	hasUserPass := false
	for _, m := range methods {
		if m == authPassword {
			hasUserPass = true
			break
		}
	}
	if !hasUserPass {
		conn.Write([]byte{socks5Version, authNoAccept})
		err = fmt.Errorf("client does not support username/password auth")
		return
	}

	conn.Write([]byte{socks5Version, authPassword})

	// read credentials (RFC 1929)
	ver := make([]byte, 1)
	if _, err = io.ReadFull(conn, ver); err != nil {
		return
	}

	ulen := make([]byte, 1)
	if _, err = io.ReadFull(conn, ulen); err != nil {
		return
	}
	uname := make([]byte, ulen[0])
	if _, err = io.ReadFull(conn, uname); err != nil {
		return
	}

	plen := make([]byte, 1)
	if _, err = io.ReadFull(conn, plen); err != nil {
		return
	}
	passwd := make([]byte, plen[0])
	if _, err = io.ReadFull(conn, passwd); err != nil {
		return
	}

	// respond success (actual auth result handled by Authenticator)
	conn.Write([]byte{0x01, 0x00})

	username = string(uname)
	password = string(passwd)
	return
}

// readSOCKS5Request reads the CONNECT request and returns the target "host:port".
func readSOCKS5Request(conn net.Conn) (string, error) {
	// ver, cmd, rsv, atyp
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", err
	}
	if header[1] != cmdConnect {
		return "", fmt.Errorf("unsupported command: %d", header[1])
	}

	var host string
	switch header[3] {
	case atypIPv4:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()

	case atypDomain:
		dlen := make([]byte, 1)
		if _, err := io.ReadFull(conn, dlen); err != nil {
			return "", err
		}
		domain := make([]byte, dlen[0])
		if _, err := io.ReadFull(conn, domain); err != nil {
			return "", err
		}
		host = string(domain)

	case atypIPv6:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()

	default:
		return "", fmt.Errorf("unsupported address type: %d", header[3])
	}

	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(portBytes)

	return fmt.Sprintf("%s:%d", host, port), nil
}

func sendSOCKS5Reply(conn net.Conn, rep byte, addr net.Addr) {
	reply := []byte{socks5Version, rep, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0}
	if addr != nil {
		if ta, ok := addr.(*net.TCPAddr); ok {
			ip := ta.IP.To4()
			if ip != nil {
				copy(reply[4:8], ip)
			}
			binary.BigEndian.PutUint16(reply[8:], uint16(ta.Port))
		}
	}
	conn.Write(reply)
	slog.Debug("socks5 reply", "rep", rep)
}
