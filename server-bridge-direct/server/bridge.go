package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baowk/bridge-direct/config"
)

const (
	// stopTimeout 是等待桥完全停止的上限。
	// DelBridgeHandler 在 HTTP handler 里同步调用，无上限等待会让请求永不返回。
	stopTimeout = 10 * time.Second

	// dialTimeout 只覆盖与目标建立连接的阶段
	dialTimeout = 10 * time.Second

	// bind / accept 失败后的退避区间
	retryMin = 50 * time.Millisecond
	retryMax = 5 * time.Second
)

// globalConnSem 是进程级并发连接配额，nil 表示不限制。
// 单端口上限拦不住桥的数量，全局这道墙才是 fd 的真正兜底。
//
// 用 atomic.Pointer 而不是裸变量：正常启动时只在起桥之前写一次，但配额一旦要
// 在运行期重设（测试、后续的配置热加载），裸变量就是和 accept/release 路径的
// 数据竞争——-race 能直接抓到。
var globalConnSem atomic.Pointer[chan struct{}]

// InitConnLimits 必须在起任何桥之前调用一次
func InitConnLimits() {
	if n := config.Cfg.MaxConns; n > 0 {
		sem := make(chan struct{}, n)
		globalConnSem.Store(&sem)
		slog.Info("global connection limit enabled", "maxConns", n)
		return
	}
	globalConnSem.Store(nil)
}

func acquireGlobalConn() bool {
	sem := globalConnSem.Load()
	if sem == nil {
		return true
	}
	select {
	case *sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// releaseGlobalConn 归还配额。配额若在连接存活期间被换过一次，这里归还的是
// 新配额的位置——只会让上限短暂偏松一个连接，不会卡死，可以接受。
func releaseGlobalConn() {
	sem := globalConnSem.Load()
	if sem == nil {
		return
	}
	select {
	case <-*sem:
	default:
	}
}

// globalConnInUse 返回已占用的全局配额，仅用于观测
func globalConnInUse() int {
	sem := globalConnSem.Load()
	if sem == nil {
		return 0
	}
	return len(*sem)
}

type bridgeListener struct {
	// done 在 supervisor 完全退出后关闭
	done   chan struct{}
	stopCh chan struct{}
	once   sync.Once

	// closed 标记「由我们主动下线」，用于区分正常下线和 accept 出错
	closed atomic.Bool
	// listening 表示当前是否真的在监听（bind 重试期间为 false）
	listening atomic.Bool
	// target 可以在不重建监听的前提下原子替换
	target atomic.Pointer[string]
	// bindErr 保存最近一次 bind 失败原因，bind 成功后清空。
	// 管理接口要据此回报「为什么没监听上」，光记日志不够。
	bindErr atomic.Pointer[string]

	accepted atomic.Int64
	rejected atomic.Int64
	dialOK   atomic.Int64
	dialFail atomic.Int64

	// lmu 保护 listener；bind 重试期间为 nil
	lmu      sync.Mutex
	listener net.Listener

	// mu 保护 connSet；connSet 为 nil 表示已停止接受新连接
	mu      sync.Mutex
	connSet map[net.Conn]struct{}
}

func newBridgeListener(toAddr string) *bridgeListener {
	l := &bridgeListener{
		done:    make(chan struct{}),
		stopCh:  make(chan struct{}),
		connSet: make(map[net.Conn]struct{}),
	}
	l.target.Store(&toAddr)
	return l
}

func (l *bridgeListener) currentTarget() string {
	if p := l.target.Load(); p != nil {
		return *p
	}
	return ""
}

func (l *bridgeListener) setTarget(toAddr string) {
	l.target.Store(&toAddr)
}

func (l *bridgeListener) setBindErr(err error) {
	s := err.Error()
	l.bindErr.Store(&s)
}

func (l *bridgeListener) clearBindErr() {
	l.bindErr.Store(nil)
}

func (l *bridgeListener) lastBindErr() string {
	if p := l.bindErr.Load(); p != nil {
		return *p
	}
	return ""
}

// stop 请求下线：停止重试、关闭当前 listener
func (l *bridgeListener) stop() {
	l.once.Do(func() {
		l.closed.Store(true)
		close(l.stopCh)
		l.lmu.Lock()
		if l.listener != nil {
			l.listener.Close()
			l.listener = nil
		}
		l.listening.Store(false)
		l.lmu.Unlock()
	})
}

// installListener 把 bind 出来的 listener 和 stop 串行化。
// stop 可能恰好发生在 net.Listen 返回之后；如果此时不再检查 closed，
// stop 看不到这个 listener，supervisor 就会进入永远未关闭的 Accept。
func (l *bridgeListener) installListener(ln net.Listener) bool {
	l.lmu.Lock()
	defer l.lmu.Unlock()
	if l.closed.Load() {
		ln.Close()
		return false
	}
	l.listener = ln
	l.listening.Store(true)
	return true
}

func (l *bridgeListener) clearListener() {
	l.lmu.Lock()
	l.listener = nil
	l.listening.Store(false)
	l.lmu.Unlock()
}

// addConn 登记一条在途连接。返回 false 表示已停止服务或撞上限额，
// 此时全局配额已经归还，调用方只需要关掉连接。
func (l *bridgeListener) addConn(c net.Conn) bool {
	limit := maxConnsPerPort()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.connSet == nil {
		return false
	}
	if limit > 0 && len(l.connSet) >= limit {
		return false
	}
	l.connSet[c] = struct{}{}
	return true
}

func (l *bridgeListener) removeConn(c net.Conn) {
	l.mu.Lock()
	_, tracked := l.connSet[c]
	delete(l.connSet, c) // connSet 为 nil 时 delete 是空操作
	l.mu.Unlock()
	if tracked {
		releaseGlobalConn()
	}
}

func (l *bridgeListener) connCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.connSet)
}

// closeConns 关闭该桥上所有在途连接。
// 关闭 listener 只会让 Accept 退出，不会断开已建立的连接，
// 少了这一步会留下「桥已删除、流量仍在转发」。
func (l *bridgeListener) closeConns() int {
	l.mu.Lock()
	conns := make([]net.Conn, 0, len(l.connSet))
	for c := range l.connSet {
		conns = append(conns, c)
	}
	l.connSet = nil
	l.mu.Unlock()

	for _, c := range conns {
		c.Close()
		releaseGlobalConn()
	}
	return len(conns)
}

var runListens = make(map[uint16]*bridgeListener)
var runMu sync.RWMutex

func InitBridgeHandler() {
	bs := cf.All()
	for _, b := range bs {
		if err := SetBridgeHandler(b.Port, b.ProxyAddr); err != nil {
			slog.Error("init bridge handler", "port", b.Port, "toAddr", b.ProxyAddr, "err", err)
		}
	}
}

// SetBridgeHandler 幂等地把 port 上的流量指向 toAddr。
//
// 已有桥时只原子替换目标地址，不重建监听。重建的代价是：
//   - 存在一个「端口未监听」的窗口，期间新连接会被 RST；
//   - 重新 bind 可能失败（端口被抢占、fd 耗尽）。
//
// 已建立的连接会继续走老目标直到自然结束——TCP 中继是字节管道，
// 中途换后端会破坏会话（SOCKS5 握手状态、流位置都无法迁移），不能也不该重连。
//
// 新建桥时会起一个 supervisor：bind 失败不再是一次性放弃，而是退避重试到成功，
// 直到 DelBridgeHandler 取消。
func SetBridgeHandler(port uint16, toAddr string) error {
	runMu.Lock()
	if l, ok := runListens[port]; ok && !l.closed.Load() {
		old := l.currentTarget()
		l.setTarget(toAddr)
		runMu.Unlock()
		if old != toAddr {
			slog.Info("SetBridgeHandler retarget", "port", port, "from", old, "to", toAddr)
		} else {
			slog.Debug("SetBridgeHandler unchanged", "port", port, "toAddr", toAddr)
		}
		return nil
	}

	l := newBridgeListener(toAddr)
	runListens[port] = l
	runMu.Unlock()

	slog.Info("SetBridgeHandler start", "port", port, "toAddr", toAddr)
	go l.supervise(port, handlerBridge)
	return nil
}

// hasBridgeHandler 判断 port 上是否已有正在监听的桥。
// 注意用的是 listening 而不是「map 里有没有」：bind 重试中的桥不算就绪，
// 否则 AddBridge 会短路返回成功，把「其实没监听」说成 ok。
func hasBridgeHandler(port uint16) bool {
	runMu.RLock()
	defer runMu.RUnlock()
	l, ok := runListens[port]
	return ok && !l.closed.Load() && l.listening.Load()
}

// WaitBridgeListening 等到 port 上的桥真正开始监听。
// 返回 false 时第二个返回值是最近一次 bind 失败原因（可能为空：还没轮到重试）。
//
// supervisor 是异步起的，SetBridgeHandler 返回 nil 只代表「已接管这个端口」，
// 不代表 bind 成功；管理接口需要据此如实报错，而不是一律说成功。
func WaitBridgeListening(port uint16, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	for {
		runMu.RLock()
		l, ok := runListens[port]
		runMu.RUnlock()
		if !ok {
			return false, "bridge was removed"
		}
		if !l.closed.Load() && l.listening.Load() {
			return true, ""
		}
		if time.Now().After(deadline) {
			return false, l.lastBindErr()
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// BridgeStats 返回运行水位，用于判断是否存在泄漏
type BridgeStats struct {
	Bridges   int
	Listening int
	Conns     int
	Accepted  int64
	Rejected  int64
	DialOK    int64
	DialFail  int64
}

func CollectBridgeStats() BridgeStats {
	runMu.RLock()
	ls := make([]*bridgeListener, 0, len(runListens))
	for _, l := range runListens {
		ls = append(ls, l)
	}
	runMu.RUnlock()

	// 不在持有 runMu 时去拿 l.mu，避免锁序交叉
	var s BridgeStats
	for _, l := range ls {
		s.Bridges++
		if l.listening.Load() {
			s.Listening++
		}
		s.Conns += l.connCount()
		s.Accepted += l.accepted.Load()
		s.Rejected += l.rejected.Load()
		s.DialOK += l.dialOK.Load()
		s.DialFail += l.dialFail.Load()
	}
	return s
}

func DelBridgeHandler(port uint16) error {
	runMu.Lock()
	l, ok := runListens[port]
	if ok {
		delete(runListens, port)
	}
	runMu.Unlock()

	if !ok {
		slog.Debug("DelBridgeHandler no bridge", "port", port)
		return nil
	}

	slog.Info("DelBridgeHandler", "port", port)
	l.stop()

	var err error
	// 必须在释放 runMu 之后再等 done：supervisor 退出时的 defer 需要获取 runMu，
	// 持锁等待会造成死锁。
	select {
	case <-l.done:
	case <-time.After(stopTimeout):
		err = fmt.Errorf("stop bridge on port %d timeout after %s", port, stopTimeout)
		slog.Error("DelBridgeHandler", "port", port, "err", err)
	}

	if n := l.closeConns(); n > 0 {
		slog.Info("DelBridgeHandler closed active conns", "port", port, "count", n)
	}
	return err
}

// supervise 维持 port 上的监听，对应 Rust 版 run() 的外层循环。
//
// 两类失败都会退避重试，而不是放弃：
//   - bind 失败（端口被短暂占用、TIME_WAIT、fd 耗尽）
//   - accept 循环因非 ErrClosed 错误退出（最典型是 EMFILE）
//
// 旧实现里 bind 失败只打一条日志，那个桥就永久缺失，只能等中心侧重推。
func (l *bridgeListener) supervise(port uint16, fn func(conn net.Conn, toAddr string)) {
	defer close(l.done)
	defer func() {
		runMu.Lock()
		if runListens[port] == l {
			delete(runListens, port)
		}
		runMu.Unlock()
	}()

	var backoff time.Duration
	for {
		if l.stopRequested() {
			return
		}

		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			l.setBindErr(err)
			backoff = nextBackoff(backoff)
			slog.Error("bind failed, retrying", "port", port,
				"toAddr", l.currentTarget(), "backoff", backoff, "err", err)
			if !l.sleepOrStop(backoff) {
				return
			}
			continue
		}

		backoff = 0
		l.clearBindErr()
		if !l.installListener(ln) {
			return
		}
		slog.Info("listen", "port", port, "toAddr", l.currentTarget())

		acceptErr := l.acceptLoop(port, ln, fn)

		l.clearListener()
		ln.Close()

		if l.stopRequested() {
			return
		}
		backoff = nextBackoff(backoff)
		slog.Error("accept loop exited, rebinding", "port", port,
			"backoff", backoff, "err", acceptErr)
		if !l.sleepOrStop(backoff) {
			return
		}
	}
}

// acceptLoop 跑到 listener 失效为止，返回导致退出的错误（主动下线时为 nil）
func (l *bridgeListener) acceptLoop(port uint16, ln net.Listener, fn func(conn net.Conn, toAddr string)) error {
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if l.closed.Load() || errors.Is(err, net.ErrClosed) {
				slog.Info("accept loop closed", "port", port)
				return nil
			}
			// 可恢复错误（fd 耗尽等）先就地退避重试，避免为此重建 listener
			backoff = nextBackoff(backoff)
			slog.Error("accept failed, retrying", "port", port, "backoff", backoff, "err", err)
			if !l.sleepOrStop(backoff) {
				return nil
			}
			// 连续失败到上限仍不恢复，交回 supervise 重建 listener
			if backoff >= retryMax {
				return err
			}
			continue
		}
		backoff = 0
		l.accepted.Add(1)

		if !acquireGlobalConn() {
			l.rejected.Add(1)
			slog.Warn("conn rejected by global limit", "port", port,
				"srcaddr", conn.RemoteAddr().String(), "maxConns", config.Cfg.MaxConns)
			conn.Close()
			continue
		}
		if !l.addConn(conn) {
			releaseGlobalConn()
			l.rejected.Add(1)
			slog.Warn("conn rejected by port limit", "port", port,
				"srcaddr", conn.RemoteAddr().String(),
				"active", l.connCount(), "limit", maxConnsPerPort())
			conn.Close()
			continue
		}

		toAddr := l.currentTarget()
		go func() {
			defer l.removeConn(conn)
			fn(conn, toAddr)
		}()
	}
}

func (l *bridgeListener) stopRequested() bool {
	select {
	case <-l.stopCh:
		return true
	default:
		return false
	}
}

// sleepOrStop 返回 false 表示收到下线请求
func (l *bridgeListener) sleepOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-l.stopCh:
		return false
	case <-t.C:
		return true
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	if cur == 0 {
		return retryMin
	}
	next := cur * 2
	if next > retryMax {
		return retryMax
	}
	return next
}

func handlerBridge(conn net.Conn, toAddr string) {
	srcaddr := conn.RemoteAddr().String()
	defer conn.Close()

	dstConn, err := net.DialTimeout("tcp", toAddr, dialTimeout)
	if err != nil {
		countDial(conn, false)
		slog.Error("dial target", "srcaddr", srcaddr, "toAddr", toAddr, "err", err)
		return
	}
	countDial(conn, true)
	defer dstConn.Close()

	src, dst := conn, dstConn
	if idle := connIdleTimeout(); idle > 0 {
		src = &idleTimeoutConn{Conn: conn, idle: idle}
		dst = &idleTimeoutConn{Conn: dstConn, idle: idle}
	}

	// 等上行拷贝真正结束再返回，这样调用方减连接计数时两个方向都已收敛
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rn, err := pipe(dst, src)
		logPipeResult("flow-up", srcaddr, toAddr, rn, err)
		// 任一方向结束就把两端都关掉，让另一个方向立刻解除阻塞
		dstConn.Close()
		conn.Close()
	}()

	rn, err := pipe(src, dst)
	logPipeResult("flow-down", srcaddr, toAddr, rn, err)
	conn.Close()
	dstConn.Close()
	wg.Wait()
}

// logPipeResult keeps successful flow accounting at Debug while promoting any
// non-nil copy error to Error so broken or reset connections are visible at the
// default production log level.
func logPipeResult(direction, srcaddr, toAddr string, amount int64, err error) {
	if err != nil && !isExpectedPipeClose(err) {
		slog.Error(direction, "srcaddr", srcaddr, "toAddr", toAddr, "amount", amount, "err", err)
		return
	}
	if err != nil {
		// 任一方向结束后 handler 会主动关闭另一端；该方向经常收到
		// net.ErrClosed（文本通常是 "use of closed network connection"），
		// 这是正常收尾，不应在生产日志中制造 ERROR 噪声。
		slog.Debug(direction, "srcaddr", srcaddr, "toAddr", toAddr, "amount", amount, "err", err)
		return
	}
	slog.Debug(direction, "srcaddr", srcaddr, "toAddr", toAddr, "amount", amount)
}

func isExpectedPipeClose(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF)
}

// countDial 把拨号结果记到对应的桥上。用 LocalAddr 的端口反查是哪个桥，
// 避免把 *bridgeListener 一路传进 handlerBridge 改动签名。
func countDial(conn net.Conn, ok bool) {
	addr, isTCP := conn.LocalAddr().(*net.TCPAddr)
	if !isTCP {
		return
	}
	runMu.RLock()
	l := runListens[uint16(addr.Port)]
	runMu.RUnlock()
	if l == nil {
		return
	}
	if ok {
		l.dialOK.Add(1)
	} else {
		l.dialFail.Add(1)
	}
}

var copyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

// pipe 单向搬运。io.CopyBuffer 会优先使用 ReadFrom/WriteTo 快路径
// （Linux 上是 splice，零拷贝、不碰这个缓冲）；只有回退到用户态拷贝时
// 才用池化缓冲，避免每连接固定 32KB 的分配 churn。
func pipe(dst, src net.Conn) (int64, error) {
	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	return io.CopyBuffer(dst, src, *bufp)
}

// idleTimeoutConn 在每次读写前把 deadline 顺延，实现「空闲超时」而不是「总时长超时」。
//
// 默认不启用（connIdleTimeout=0）。TCP keepalive 已经能回收「对端消失」的死链路，
// 而空闲超时会掐掉「对端还在、只是没数据」的正常长连接（WebSocket、连接池空闲 socket）。
// 另外包一层会丢掉 TCPConn 的 ReadFrom/WriteTo，也就失去 splice 快路径。
type idleTimeoutConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleTimeoutConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *idleTimeoutConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

func connIdleTimeout() time.Duration {
	if config.Cfg.ConnIdleTimeout <= 0 {
		return 0
	}
	return time.Duration(config.Cfg.ConnIdleTimeout) * time.Second
}

func maxConnsPerPort() int {
	if config.Cfg.MaxConnsPerPort <= 0 {
		return 0
	}
	return config.Cfg.MaxConnsPerPort
}
