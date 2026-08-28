// #file:/root/works/gos/gproxy/server/server.go
package server

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/baowk/gproxy/internal/socks5"
	"github.com/baowk/gproxy/internal/store"
	"github.com/patrickmn/go-cache"
	"github.com/txthinking/runnergroup"
)

//var Servers sync.Map

func init() {
}

// Server holds the server state and configuration
type Server struct {
	pi                *store.ProxyInfo
	Method            byte
	SupportedCommands []byte
	ServerAddr        net.Addr
	UDPConn           *net.UDPConn
	UDPExchanges      *cache.Cache
	TCPTimeout        time.Duration
	UDPTimeout        time.Duration
	Handle            Handler
	AssociatedUDP     *cache.Cache
	UDPSrc            *cache.Cache
	RunnerGroup       *runnergroup.RunnerGroup
	// RFC: [UDP ASSOCIATE] The server MAY use this information to limit access to the association. Default false, no limit.
	LimitUDP bool
}

func (s *Server) Auth(username, password string) bool {
	return s.pi.Auth(username, password)
}

type Handler interface {
	// Request has not been replied yet
	TCPHandle(*Server, *MixedConn, *socks5.Request) error
	UDPHandle(*Server, *net.UDPAddr, *socks5.Datagram) error
	HttpHandle(*Server, *MixedConn) error
}

// NewServer creates a new server instance with the given configuration
func NewServer(pi *store.ProxyInfo, tcpTimeout, udpTimeout time.Duration) error {
	sAddr, err := socks5.Resolve("udp", net.JoinHostPort(pi.Ip, fmt.Sprintf("%d", pi.Port)))
	if err != nil {
		return err
	}
	// m := socks5.MethodNone
	// if len(pi.Users) > 0 {
	// 	m = socks5.MethodUsernamePassword
	// }
	cs := cache.New(cache.NoExpiration, cache.NoExpiration)
	cs1 := cache.New(cache.NoExpiration, cache.NoExpiration)
	cs2 := cache.New(cache.NoExpiration, cache.NoExpiration)
	s := &Server{
		pi:                pi,
		Method:            socks5.MethodUsernamePassword,
		SupportedCommands: []byte{socks5.CmdConnect, socks5.CmdUDP},
		ServerAddr:        sAddr,
		UDPExchanges:      cs,
		TCPTimeout:        tcpTimeout,
		UDPTimeout:        udpTimeout,
		AssociatedUDP:     cs1,
		UDPSrc:            cs2,
		RunnerGroup:       runnergroup.New(),
	}
	//Servers.Store(pi.GetKey(), s)
	slog.Debug("Proxy starting", "ip", pi.Ip, "port", pi.Port)
	if err = s.listenAndServe(); err != nil {
		slog.Error("Error starting server", "error", err, "ip", pi.Ip, "port", pi.Port)
		//Servers.Delete(pi.GetKey())
		return err
	}

	return nil
}

// Run server
func (s *Server) listenAndServe() error {
	if s.Handle == nil {
		s.Handle = &DefaultHandle{}
	}
	curAddr := fmt.Sprintf("%s:%d", s.pi.Ip, s.pi.Port)
	addr, err := net.ResolveTCPAddr("tcp", curAddr)
	if err != nil {
		return err
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return err
	}
	s.RunnerGroup.Add(&runnergroup.Runner{
		Start: func() error {
			for {
				c, err := l.AcceptTCP()
				if err != nil {
					return err
				}
				// Another way to get IP without port
				remoteAddr := c.RemoteAddr().String()
				if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
					// Use 'host' which contains only the IP
					if !store.CanAccess(host) {
						slog.Error("[block]", "ip", host)
						c.Close()
						continue
					}
				} else {
					// If splitting fails, use the whole address (less ideal)
					if !store.CanAccess(remoteAddr) {
						slog.Error("[block]", "ip", remoteAddr)
						c.Close()
						continue
					}
				}

				go func(c *net.TCPConn) {
					mixedConn, err := NewMixedConn(c)
					if err != nil {
						slog.Error("Error", "local", s.pi.Ip, "remote", remoteAddr, "err", err)
						return
					}

					defer mixedConn.Close()

					if mixedConn.IsSocks5() {
						if err := s.Negotiate(mixedConn); err != nil {
							slog.Error("Negotiate", "err", err)
							return
						}
						r, err := s.GetRequest(mixedConn)
						if err != nil {
							slog.Error("GetRequest", "err", err)
							return
						}
						if err := s.Handle.TCPHandle(s, mixedConn, r); err != nil {
							slog.Error("TCPHandle", "err", err)
						}
					} else if mixedConn.IsHTTP() {
						err := s.Handle.HttpHandle(s, mixedConn)
						if err != nil {
							slog.Error("HttpHandle", "err", err)
							return
						}
					} else {
						slog.Error("Error", "err", "Not socks5 or http")
						return
					}

				}(c)
			}
			//return nil
		},
		Stop: func() error {
			return l.Close()
		},
	})
	addr1, err := net.ResolveUDPAddr("udp", curAddr)
	if err != nil {
		l.Close()
		return err
	}
	s.UDPConn, err = net.ListenUDP("udp", addr1)
	if err != nil {
		l.Close()
		return err
	}
	s.RunnerGroup.Add(&runnergroup.Runner{
		Start: func() error {
			for {
				b := make([]byte, 65507)
				n, addr, err := s.UDPConn.ReadFromUDP(b)
				if err != nil {
					return err
				}
				go func(addr *net.UDPAddr, b []byte) {
					d, err := socks5.NewDatagramFromBytes(b)
					if err != nil {
						slog.Error("NewDatagramFromBytes", "err", err)
						return
					}
					if d.Frag != 0x00 {
						slog.Info("Ignore", "frag", d.Frag)
						return
					}
					if err := s.Handle.UDPHandle(s, addr, d); err != nil {
						slog.Error("UDPHandle", "err", err)
						return
					}
				}(addr, b[0:n])
			}
			//return nil
		},
		Stop: func() error {
			return s.UDPConn.Close()
		},
	})
	return s.RunnerGroup.Wait()
}

// Negotiate handle negotiate packet.
// This method do not handle gssapi(0x01) method now.
// Error or OK both replied.
func (s *Server) Negotiate(rw io.ReadWriter) error {
	rq, err := socks5.NewNegotiationRequestFrom(rw)
	if err != nil {
		return err
	}
	var got bool
	var m byte
	for _, m = range rq.Methods {
		if m == s.Method {
			got = true
		}
	}
	if !got {
		rp := socks5.NewNegotiationReply(socks5.MethodUnsupportedAll)
		if _, err := rp.WriteTo(rw); err != nil {
			return err
		}
	}
	rp := socks5.NewNegotiationReply(s.Method)
	if _, err := rp.WriteTo(rw); err != nil {
		return err
	}

	if s.Method == socks5.MethodUsernamePassword {
		urq, err := socks5.NewUserPassNegotiationRequestFrom(rw)
		if err != nil {
			return err
		}

		if !s.Auth(string(urq.Uname), string(urq.Passwd)) {
			urp := socks5.NewUserPassNegotiationReply(socks5.UserPassStatusFailure)
			if _, err := urp.WriteTo(rw); err != nil {
				return err
			}
			return socks5.ErrUserPassAuth
		}
		urp := socks5.NewUserPassNegotiationReply(socks5.UserPassStatusSuccess)
		if _, err := urp.WriteTo(rw); err != nil {
			return err
		}
	}
	return nil
}

// GetRequest get request packet from client, and check command according to SupportedCommands
// Error replied.
func (s *Server) GetRequest(rw io.ReadWriter) (*socks5.Request, error) {
	r, err := socks5.NewRequestFrom(rw)
	if err != nil {
		return nil, err
	}
	var supported bool
	for _, c := range s.SupportedCommands {
		if r.Cmd == c {
			supported = true
			break
		}
	}
	if !supported {
		var p *socks5.Reply
		if r.Atyp == socks5.ATYPIPv4 || r.Atyp == socks5.ATYPDomain {
			p = socks5.NewReply(socks5.RepCommandNotSupported, socks5.ATYPIPv4, []byte{0x00, 0x00, 0x00, 0x00}, []byte{0x00, 0x00})
		} else {
			p = socks5.NewReply(socks5.RepCommandNotSupported, socks5.ATYPIPv6, []byte(net.IPv6zero), []byte{0x00, 0x00})
		}
		if _, err := p.WriteTo(rw); err != nil {
			return nil, err
		}
		return nil, socks5.ErrUnsupportedCmd
	}
	return r, nil
}

// Stop server
func (s *Server) Shutdown() error {
	return s.RunnerGroup.Done()
}
