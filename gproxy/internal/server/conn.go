package server

import (
	"bufio"
	"net"
	"strings"
)

type MixedConn struct {
	*net.TCPConn
	HeadBytes []byte
	r         *bufio.Reader
}

func NewMixedConn(c *net.TCPConn) (*MixedConn, error) {
	r := bufio.NewReader(c)
	hbs, err := r.Peek(2)
	if err != nil {
		return nil, err
	}
	return &MixedConn{c, hbs, r}, nil
}

func (c *MixedConn) Read(b []byte) (int, error) {
	//slog.Debug("Reading from MixedConn")
	return c.r.Read(b)
}

func (c *MixedConn) IsSocks5() bool {
	// SOCKS5 requests start with version 5 (0x05)
	return len(c.HeadBytes) >= 1 && c.HeadBytes[0] == 0x05
}

var (
	methods = []string{"GE", "PO", "PU", "DE", "HE", "OP", "CO", "TR", "PA"}
)

func (c *MixedConn) IsHTTP() bool {
	// HTTP requests start with method names
	for _, method := range methods {
		if strings.HasPrefix(string(c.HeadBytes), method) {
			return true
		}
	}
	return false
}
