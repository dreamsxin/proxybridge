package server

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/baowk/gproxy/internal/socks5"
	"github.com/baowk/gproxy/internal/store"
)

// DefaultHandle implements Handler interface
type DefaultHandle struct {
}

// TCPHandle auto handle request. You may prefer to do yourself.
func (h *DefaultHandle) TCPHandle(s *Server, c *MixedConn, r *socks5.Request) error {
	if r.Cmd == socks5.CmdConnect {
		var localAddr string
		if s.pi.Ip != "" {
			localAddr = fmt.Sprintf("%s:0", s.pi.Ip)
		}
		rc, err := r.Connect(c, localAddr)
		if err != nil {
			return err
		}
		defer rc.Close()
		slog.Info("socks", "client", c.RemoteAddr().String(), "local", s.pi.Ip, "remote", r.Address())

		go func() {
			var bf [1024 * 2]byte
			for {
				if s.TCPTimeout != 0 {
					if err := rc.SetDeadline(time.Now().Add(s.TCPTimeout)); err != nil {
						return
					}
				}
				i, err := rc.Read(bf[:])
				if err != nil {
					return
				}
				if _, err := c.Write(bf[0:i]); err != nil {
					return
				}
			}
		}()
		var bf [1024 * 2]byte
		for {
			if s.TCPTimeout != 0 {
				if err := c.SetDeadline(time.Now().Add(s.TCPTimeout)); err != nil {
					return nil
				}
			}
			i, err := c.Read(bf[:])
			if err != nil {
				return nil
			}
			if _, err := rc.Write(bf[0:i]); err != nil {
				return nil
			}
		}
		//return nil
	}
	if r.Cmd == socks5.CmdUDP {
		caddr, err := r.UDP(c, s.ServerAddr)
		if err != nil {
			return err
		}
		ch := make(chan byte)
		defer close(ch)
		s.AssociatedUDP.Set(caddr.String(), ch, -1)
		defer s.AssociatedUDP.Delete(caddr.String())
		io.Copy(io.Discard, c)
		slog.Debug("a tcp connection that udp associated closed\n", "addr", caddr.String())
		return nil
	}
	return socks5.ErrUnsupportedCmd
}

// UDPHandle auto handle packet. You may prefer to do yourself.
func (h *DefaultHandle) UDPHandle(s *Server, addr *net.UDPAddr, d *socks5.Datagram) error {
	src := addr.String()
	var ch chan byte
	if s.LimitUDP {
		any, ok := s.AssociatedUDP.Get(src)
		if !ok {
			return fmt.Errorf("this udp address %s is not associated with tcp", src)
		}
		ch = any.(chan byte)
	}
	send := func(ue *socks5.UDPExchange, data []byte) error {
		select {
		case <-ch:
			return fmt.Errorf("this udp address %s is not associated with tcp", src)
		default:
			_, err := ue.RemoteConn.Write(data)
			if err != nil {
				return err
			}
			slog.Debug("Sent UDP data to remote", "client", ue.ClientAddr.String(), "server", ue.RemoteConn.LocalAddr().String(), "remote", ue.RemoteConn.RemoteAddr().String(), "data", data)
		}
		return nil
	}

	dst := d.Address()
	var ue *socks5.UDPExchange
	iue, ok := s.UDPExchanges.Get(src + dst)
	if ok {
		ue = iue.(*socks5.UDPExchange)
		return send(ue, d.Data)
	}

	slog.Debug("call udp", "dst", dst)
	var laddr string
	any, ok := s.UDPSrc.Get(src + dst)
	if ok {
		laddr = any.(string)
	}
	rc, err := socks5.DialUDP("udp", laddr, dst)
	if err != nil {
		if !strings.Contains(err.Error(), "address already in use") && !strings.Contains(err.Error(), "can't assign requested address") {
			return err
		}
		rc, err = socks5.DialUDP("udp", "", dst)
		if err != nil {
			return err
		}
		laddr = ""
	}
	if laddr == "" {
		s.UDPSrc.Set(src+dst, rc.LocalAddr().String(), -1)
	}
	ue = &socks5.UDPExchange{
		ClientAddr: addr,
		RemoteConn: rc,
	}
	slog.Debug("Created remote UDP conn for client", "client", addr.String(), "server", ue.RemoteConn.LocalAddr().String(), "remote", d.Address())
	if err := send(ue, d.Data); err != nil {
		ue.RemoteConn.Close()
		return err
	}
	s.UDPExchanges.Set(src+dst, ue, -1)
	go func(ue *socks5.UDPExchange, dst string) {
		defer func() {
			ue.RemoteConn.Close()
			s.UDPExchanges.Delete(ue.ClientAddr.String() + dst)
		}()
		var b [65507]byte
		for {
			select {
			case <-ch:
				slog.Debug("The tcp that udp address associated closed", "addr", ue.ClientAddr.String())
				return
			default:
				if s.UDPTimeout != 0 {
					if err := ue.RemoteConn.SetDeadline(time.Now().Add(s.UDPTimeout)); err != nil {
						log.Println(err)
						return
					}
				}
				n, err := ue.RemoteConn.Read(b[:])
				if err != nil {
					return
				}
				slog.Debug("Got UDP data from remote.", "client", ue.ClientAddr.String(), "server", ue.RemoteConn.LocalAddr().String(), "remote", ue.RemoteConn.RemoteAddr().String(), "data", b[0:n])
				a, addr, port, err := socks5.ParseAddress(dst)
				if err != nil {
					log.Println(err)
					return
				}
				if a == socks5.ATYPDomain {
					addr = addr[1:]
				}
				d1 := socks5.NewDatagram(a, addr, port, b[0:n])
				if _, err := s.UDPConn.WriteToUDP(d1.Bytes(), ue.ClientAddr); err != nil {
					return
				}
				slog.Debug("Sent Datagram.", "client", ue.ClientAddr.String(), "server", ue.RemoteConn.LocalAddr().String(), "remote", ue.RemoteConn.RemoteAddr().String(), "rsv", d1.Rsv, "frag", d1.Frag, "atyp", d1.Atyp, "dstAddr", d1.DstAddr, "dstPort", d1.DstPort, "data", d1.Data, "address", d1.Address())
			}
		}
	}(ue, dst)
	return nil
}

// HttpHandle implements HTTP proxy functionality for both HTTP and HTTPS
func (h *DefaultHandle) HttpHandle(s *Server, c *MixedConn) error {
	req, err := http.ReadRequest(c.r)
	if err != nil {
		return err
	}

	// Check if authentication is required and validate
	if !h.validateAuth(s, req) {
		c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"Proxy\"\r\n\r\n"))
		return nil
	}

	if store.IsBlackSuffixes(req.URL.Hostname()) {
		c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"Proxy\"\r\n\r\n"))
		return nil
	}

	slog.Info("http", "client", c.TCPConn.RemoteAddr().String(), "local", s.pi.Ip, "remote", req.RequestURI)

	// Handle CONNECT method for HTTPS
	if req.Method == "CONNECT" {
		return h.handleHTTPS(c, req, s)
	}

	// Handle regular HTTP requests
	return h.handleHTTP(c, req, s)
}

// handleHTTP handles regular HTTP requests with zero-copy
func (h *DefaultHandle) handleHTTP(clientConn *MixedConn, req *http.Request, s *Server) error {
	// Remove proxy headers
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authenticate")
	req.Header.Del("Proxy-Authorization")

	// Fix the RequestURI issue - set it to empty as it should not be set for client requests
	req.RequestURI = ""

	// Create HTTP client with custom transport
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	// Check if there's a specific outbound IP configured
	outboundIP := s.pi.Ip
	if outboundIP != "" {
		// Validate that the outbound IP is a valid IP address and not loopback
		if ip := net.ParseIP(outboundIP); ip != nil {
			// Make sure it's not a loopback address (127.x.x.x)
			if !ip.IsLoopback() && !ip.IsUnspecified() {
				// Create a custom dialer with local address binding
				transport.DialContext = (&net.Dialer{
					LocalAddr: &net.TCPAddr{
						IP: ip,
					},
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext
			} else {
				slog.Debug("Skipping outbound IP binding", "reason", "loopback or unspecified address", "ip", outboundIP)
			}
		} else {
			slog.Debug("Skipping outbound IP binding", "reason", "invalid IP address", "ip", outboundIP)
		}
	}

	httpClient := &http.Client{
		Transport: transport,
	}
	if s.TCPTimeout != 0 {
		httpClient.Timeout = s.TCPTimeout
	} else {
		httpClient.Timeout = 30 * time.Second
	}

	// Forward the request
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Error("Error forwarding HTTP request", "error", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return err
	}
	defer resp.Body.Close()

	// Write response status and headers first
	// Manually write the status line and headers for better control
	statusLine := fmt.Sprintf("%s %s %s\r\n", resp.Proto, resp.Status, "\r\n")
	clientConn.Write([]byte(statusLine))

	// Write headers
	for name, values := range resp.Header {
		for _, value := range values {
			headerLine := fmt.Sprintf("%s: %s\r\n", name, value)
			clientConn.Write([]byte(headerLine))
		}
	}
	clientConn.Write([]byte("\r\n"))

	// Zero-copy transfer of response body
	// Use a shared buffer for better performance
	buffer := make([]byte, 32*1024) // 32KB buffer
	_, err = io.CopyBuffer(clientConn, resp.Body, buffer)
	if err != nil && err != io.EOF {
		slog.Error("Error transferring response body", "error", err)
		return err
	}

	return nil
}

func (h *DefaultHandle) handleHTTPS(clientConn *MixedConn, req *http.Request, s *Server) error {
	// Prepare dialer with outbound IP if specified
	var dialer net.Dialer
	outboundIP := s.pi.Ip
	if outboundIP != "" {
		// Validate that the outbound IP is a valid IP address and not loopback
		if ip := net.ParseIP(outboundIP); ip != nil {
			// Make sure it's not a loopback address (127.x.x.x)
			if !ip.IsLoopback() && !ip.IsUnspecified() {
				dialer.LocalAddr = &net.TCPAddr{
					IP: ip,
				}
			} else {
				slog.Debug("Skipping outbound IP binding for HTTPS", "reason", "loopback or unspecified address", "ip", outboundIP)
			}
		} else {
			slog.Debug("Skipping outbound IP binding for HTTPS", "reason", "invalid IP address", "ip", outboundIP)
		}
	}
	if s.TCPTimeout != 0 {
		dialer.Timeout = s.TCPTimeout
	} else {
		dialer.Timeout = 10 * time.Second
	}
	// Connect to target server
	targetConn, err := dialer.Dial("tcp", req.Host)
	if err != nil {
		slog.Error("Error connecting to target", "target", req.Host, "error", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return err
	}
	defer targetConn.Close()

	// Send success response to client
	_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		return err
	}

	// Reset deadlines
	clientConn.SetDeadline(time.Time{})
	if tcpConn, ok := targetConn.(*net.TCPConn); ok {
		tcpConn.SetDeadline(time.Time{})
	}

	// Create buffered readers for both connections
	targetReader := bufio.NewReader(targetConn)
	clientReader := clientConn.r // Use existing buffered reader

	// Create channels to signal when each direction is done
	clientToTargetDone := make(chan error, 1)
	targetToClientDone := make(chan error, 1)

	// Copy from client to target
	go func() {
		_, err := io.Copy(targetConn, clientReader)
		clientToTargetDone <- err
	}()

	// Copy from target to client
	go func() {
		_, err := io.Copy(clientConn, targetReader)
		targetToClientDone <- err
	}()

	// Wait for either direction to complete or timeout
	select {
	case err := <-clientToTargetDone:
		if err != nil && err != io.EOF {
			return err
		}
	case err := <-targetToClientDone:
		if err != nil && err != io.EOF {
			return err
		}
	case <-time.After(30 * time.Second):
		return fmt.Errorf("timeout")
	}

	return nil
}

// validateAuth validates HTTP basic authentication
func (h *DefaultHandle) validateAuth(s *Server, request *http.Request) bool {
	// Get Authorization header
	authHeader := request.Header.Get("Proxy-Authorization")
	if authHeader == "" {
		authHeader = request.Header.Get("Authorization")
		if authHeader == "" {
			return false
		}
	}

	// Check if it's Basic auth
	if !strings.HasPrefix(authHeader, "Basic ") {
		return false
	}

	// Decode base64 encoded credentials
	encoded := strings.TrimPrefix(authHeader, "Basic ")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}

	// Split username and password
	creds := strings.Split(string(decoded), ":")
	if len(creds) != 2 {
		return false
	}

	return s.Auth(creds[0], creds[1])
}
