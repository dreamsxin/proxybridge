package server

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

type bridgeListener struct {
	listener net.Listener
	done     chan struct{}
	once     sync.Once
}

func (l *bridgeListener) stop() {
	l.once.Do(func() {
		l.listener.Close()
	})
}

var runListens = make(map[uint16]*bridgeListener)
var runMu sync.RWMutex

func InitBridgeHandler() {
	bs := cf.All()
	for _, b := range bs {
		fmt.Println(b.Port, b.ProxyAddr)
		if err := AddBridgeHandler(b.Port, b.ProxyAddr); err != nil {
			slog.Error("init bridge handler", "port", b.Port, "toAddr", b.ProxyAddr, "err", err)
		}
	}
}

func ChangeBridgeHandler(port uint16, toAddr string) error {
	slog.Info("ChangeBridgeHandler", "port", port, "toAddr", toAddr)
	if err := DelBridgeHandler(port); err != nil {
		return err
	}
	return AddBridgeHandler(port, toAddr)
}

func AddBridgeHandler(port uint16, toAddr string) error {
	slog.Info("AddBridgeHandler", "port", port, "toAddr", toAddr)
	return listenProxy(port, toAddr, handlerBridge)
}

func DelBridgeHandler(port uint16) error {
	slog.Info("DelBridgeHandler", "port", port)
	runMu.Lock()
	curRunL, ok := runListens[port]
	if ok {
		delete(runListens, port)
	}
	runMu.Unlock()
	if ok {
		curRunL.stop()
		<-curRunL.done
	}
	return nil
}

func listenProxy(listenPort uint16, toAddr string, fn func(conn net.Conn, toAddr string)) error {
	slog.Info("listen", "port", listenPort, "toaddr", toAddr)
	listen, err := net.Listen("tcp", fmt.Sprintf(":%d", listenPort))
	if err != nil {
		slog.Error("listen", "port", listenPort, "err", err)
		return err
	}

	runMu.Lock()
	_, ok := runListens[listenPort]
	if ok {
		listen.Close()
		runMu.Unlock()
		return fmt.Errorf("port %d already listening", listenPort)
	}
	curRunL := &bridgeListener{
		listener: listen,
		done:     make(chan struct{}),
	}
	runListens[listenPort] = curRunL
	runMu.Unlock()

	go func() {
		defer close(curRunL.done)
		defer func() {
			runMu.Lock()
			if runListens[listenPort] == curRunL {
				delete(runListens, listenPort)
			}
			runMu.Unlock()
		}()
		for {
			conn, err := listen.Accept()
			if err != nil {
				slog.Info("close", "port", listenPort)
				return
			}
			go fn(conn, toAddr)
		}
	}()
	return nil
}

func handlerBridge(conn net.Conn, toAddr string) {
	srcaddr := conn.RemoteAddr().String()
	defer conn.Close()
	dstConn, err := net.DialTimeout("tcp", toAddr, time.Second*10)
	if err != nil {
		slog.Error("dstConn", "err", err)
		return
	}
	//localport := dstConn.LocalAddr().String()
	//slog.Debug(srcaddr, "localport", localport, "toAddr", toAddr)
	go func() {
		rn, _ := io.Copy(dstConn, conn)
		slog.Info("[flow-up]", "[srcaddr]", srcaddr, "[toAddr]", toAddr, "amout", rn)
		dstConn.Close()
	}()

	rn, _ := io.Copy(conn, dstConn)
	slog.Info("[flow-down]", "[srcaddr]", srcaddr, "[toAddr]", toAddr, "amout", rn)
	dstConn.Close()
}
