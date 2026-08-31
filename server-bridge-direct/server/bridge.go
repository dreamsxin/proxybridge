package server

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// stopTimeout 是等待 Accept 循环退出的上限。
// DelBridgeHandler 是在 HTTP handler 里同步调用的，无上限等待会让请求永不返回，
// 表现上和死锁难以区分。
const stopTimeout = 10 * time.Second

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
		if err := AddBridgeHandler(b.Port, b.ProxyAddr); err != nil {
			slog.Error("init bridge handler", "port", b.Port, "toAddr", b.ProxyAddr, "err", err)
		}
	}
}

// AddBridgeHandler 幂等地把 port 上的监听指向 toAddr：
// 已存在的监听先停掉再重建。新增和改目标走同一条路径，调用方不需要（也不应该）
// 依赖缓存里有无记录来选择分支——那份判断会和实际监听状态分叉。
func AddBridgeHandler(port uint16, toAddr string) error {
	slog.Info("AddBridgeHandler", "port", port, "toAddr", toAddr)
	if err := DelBridgeHandler(port); err != nil {
		return err
	}
	return listenProxy(port, toAddr, handlerBridge)
}

func DelBridgeHandler(port uint16) error {
	runMu.Lock()
	curRunL, ok := runListens[port]
	if ok {
		delete(runListens, port)
	}
	runMu.Unlock()

	if !ok {
		slog.Debug("DelBridgeHandler no listener", "port", port)
		return nil
	}

	slog.Info("DelBridgeHandler", "port", port)
	curRunL.stop()
	// 必须在释放 runMu 之后再等 done：Accept 循环退出时的 defer 需要获取 runMu，
	// 持锁等待会造成死锁。
	select {
	case <-curRunL.done:
		return nil
	case <-time.After(stopTimeout):
		err := fmt.Errorf("stop listener on port %d timeout after %s", port, stopTimeout)
		slog.Error("DelBridgeHandler", "port", port, "err", err)
		return err
	}
}

func listenProxy(listenPort uint16, toAddr string, fn func(conn net.Conn, toAddr string)) error {
	// 检查、监听、登记三步在同一把锁内完成，否则并发调用会出现
	// 两边都判定「端口空闲」、其中一个 bind 失败的竞态。
	runMu.Lock()
	if _, ok := runListens[listenPort]; ok {
		runMu.Unlock()
		err := fmt.Errorf("port %d already listening", listenPort)
		slog.Error("listen", "port", listenPort, "toAddr", toAddr, "err", err)
		return err
	}

	listen, err := net.Listen("tcp", fmt.Sprintf(":%d", listenPort))
	if err != nil {
		runMu.Unlock()
		slog.Error("listen", "port", listenPort, "toAddr", toAddr, "err", err)
		return err
	}

	curRunL := &bridgeListener{
		listener: listen,
		done:     make(chan struct{}),
	}
	runListens[listenPort] = curRunL
	runMu.Unlock()

	slog.Info("listen", "port", listenPort, "toAddr", toAddr)

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
				slog.Info("accept loop exit", "port", listenPort, "toAddr", toAddr, "err", err)
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
		slog.Error("dial target", "srcaddr", srcaddr, "toAddr", toAddr, "err", err)
		return
	}
	defer dstConn.Close()

	go func() {
		rn, err := io.Copy(dstConn, conn)
		slog.Debug("flow-up", "srcaddr", srcaddr, "toAddr", toAddr, "amount", rn, "err", err)
		dstConn.Close()
	}()

	rn, err := io.Copy(conn, dstConn)
	slog.Debug("flow-down", "srcaddr", srcaddr, "toAddr", toAddr, "amount", rn, "err", err)
}
