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

	// dialTimeout 只覆盖与目标建立连接的阶段，见 dialTimeout()

	// bind / accept 失败后的退避区间
	retryMin = 50 * time.Millisecond
	retryMax = 5 * time.Second

	// listenStableFor 是「这次监听算稳定服务过」的门槛。只有跑够这么久再退出，
	// 才把退避归零；否则 EMFILE 这类持续故障会退化成 50ms 的热循环。
	listenStableFor = retryMax
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
	port uint16
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
	bindErr         atomic.Pointer[string]
	portOwnerLogged atomic.Bool
	// notify 里的 channel 在每次状态变化（bind 成功/失败、停止监听、主动下线）时
	// 关闭并换新，供 WaitBridgeListening 等待，替掉原来的 10ms 轮询——轮询是在
	// 持有端口锁的情况下做的，最坏要占满整个 listenReadyTimeout。
	notify atomic.Pointer[chan struct{}]

	accepted        atomic.Int64
	rejected        atomic.Int64
	rejectedGlobal  atomic.Int64
	rejectedPort    atomic.Int64
	rejectedClosing atomic.Int64
	dialOK          atomic.Int64
	dialFail        atomic.Int64
	bindErrors      atomic.Int64
	acceptErrors    atomic.Int64
	relayUpBytes    atomic.Int64
	relayDownBytes  atomic.Int64
	dialDurationNs  atomic.Int64
	dialCount       atomic.Uint64
	dialBuckets     [dialDurationBucketCount]atomic.Uint64

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
	ch := make(chan struct{})
	l.notify.Store(&ch)
	return l
}

// stateChanged 返回一个在下一次状态变化时被关闭的 channel。
// 等待方必须先取 channel、再判定状态，顺序反了会漏掉「判定之后、等待之前」
// 发生的那次变化，然后一直等到超时。
func (l *bridgeListener) stateChanged() <-chan struct{} {
	if p := l.notify.Load(); p != nil {
		return *p
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}

func (l *bridgeListener) notifyStateChange() {
	next := make(chan struct{})
	if old := l.notify.Swap(&next); old != nil {
		close(*old)
	}
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

func (l *bridgeListener) setBindErrMessage(message string) {
	l.bindErr.Store(&message)
	l.notifyStateChange()
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
		l.notifyStateChange()
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
	l.notifyStateChange()
	return true
}

func (l *bridgeListener) clearListener() {
	l.lmu.Lock()
	l.listener = nil
	l.listening.Store(false)
	l.lmu.Unlock()
	l.notifyStateChange()
}

// addConnResult 区分「桥正在下线」和「撞上单端口限额」。
// 两者都要关掉连接，但混成一个 false 会让 port_limit 计数和告警把下线期间的
// 正常丢弃也算成限额打满，排查时完全被带偏。
type addConnResult int

const (
	addConnOK addConnResult = iota
	addConnClosing
	addConnLimit
)

// addConn 登记一条在途连接。返回非 addConnOK 时全局配额已经归还，
// 调用方只需要关掉连接。
func (l *bridgeListener) addConn(c net.Conn) addConnResult {
	limit := maxConnsPerPort()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.connSet == nil {
		return addConnClosing
	}
	if limit > 0 && len(l.connSet) >= limit {
		return addConnLimit
	}
	l.connSet[c] = struct{}{}
	return addConnOK
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
//
// 撞上本进程自己的端口（管理/pprof/metrics）在这里统一拒掉，而不是只在
// /bridge/add 的入口校验：InitBridgeHandler（启动时按 bridge.db 恢复，含中心
// 同步下来的记录）和 StartBridge 都会走到这里，只在 API 入口拦是拦不住的。
func SetBridgeHandler(port uint16, toAddr string) error {
	if who, ok := selfPorts()[port]; ok {
		return fmt.Errorf("port %d conflicts with %s", port, who)
	}

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
	l.port = port
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
//
// 用状态变化通知而不是轮询：调用方（AddBridge/StartBridge）是持着端口锁进来的，
// 轮询会把这把锁按 timeout 上限占住。bind 已经失败过一次就立刻回报——原因此刻
// 就是准确的，没必要把调用方吊到超时，supervisor 仍会在后台退避重试。
func WaitBridgeListening(port uint16, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	for {
		runMu.RLock()
		l, ok := runListens[port]
		runMu.RUnlock()
		if !ok {
			return false, "bridge was removed"
		}

		changed := l.stateChanged()
		if !l.closed.Load() && l.listening.Load() {
			return true, ""
		}
		if bindErr := l.lastBindErr(); bindErr != "" {
			return false, bindErr
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, l.lastBindErr()
		}
		timer := time.NewTimer(remaining)
		select {
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// BridgeStats 返回运行水位，用于判断是否存在泄漏。
//
// 计数口径（Accepted 与 Rejected 有重叠，别相减当「成功连接数」用之外的解读）：
//   - Accepted：accept 成功的次数，在配额判定之前累加，所以包含随后被
//     全局/端口限额拒掉、以及桥下线期间丢弃的连接。这是内核视角的真实到达量。
//   - Rejected：accept 之后被我们主动关掉的连接，按 global_limit / port_limit /
//     closing 三种原因细分（细分值在 metrics 里）。
//   - 真正进入转发的连接数 = Accepted - Rejected；当前在途连接是 Conns。
//
// 另外这些累计值挂在各自的 listener 上，桥被删除时一起消失，所以求和结果会变小。
type BridgeStats struct {
	Bridges   int
	Listening int
	Conns     int
	Accepted  int64
	Rejected  int64
	DialOK    int64
	DialFail  int64
}

type bridgeMetricSnapshot struct {
	BridgePort          uint16
	ProxyAddr           string
	Running             bool
	Listening           bool
	Conns               int
	Accepted            int64
	RejectedGlobal      int64
	RejectedPort        int64
	RejectedClosing     int64
	DialOK              int64
	DialFail            int64
	BindErrors          int64
	AcceptErrors        int64
	RelayUpBytes        int64
	RelayDownBytes      int64
	DialDurationNs      int64
	DialCount           uint64
	DialDurationBuckets [dialDurationBucketCount]uint64
}

type bridgeMetricTotals struct {
	Accepted        atomic.Uint64
	RejectedGlobal  atomic.Uint64
	RejectedPort    atomic.Uint64
	RejectedClosing atomic.Uint64
	DialOK          atomic.Uint64
	DialFail        atomic.Uint64
	BindErrors      atomic.Uint64
	AcceptErrors    atomic.Uint64
	RelayUpBytes    atomic.Uint64
	RelayDownBytes  atomic.Uint64
	DialDurationNs  atomic.Uint64
	DialCount       atomic.Uint64
	DialBuckets     [dialDurationBucketCount]atomic.Uint64
}

var metricTotals bridgeMetricTotals

func collectBridgeMetricSnapshots() []bridgeMetricSnapshot {
	runMu.RLock()
	listenersByPort := make(map[uint16]*bridgeListener, len(runListens))
	for port, l := range runListens {
		listenersByPort[port] = l
	}
	ls := make([]*bridgeListener, 0, len(listenersByPort))
	for _, l := range listenersByPort {
		ls = append(ls, l)
	}
	runMu.RUnlock()

	snapshots := make([]bridgeMetricSnapshot, 0, len(ls))
	for _, l := range ls {
		snapshot := bridgeMetricSnapshot{
			BridgePort:      l.port,
			ProxyAddr:       l.currentTarget(),
			Running:         true,
			Listening:       !l.closed.Load() && l.listening.Load(),
			Conns:           l.connCount(),
			Accepted:        l.accepted.Load(),
			RejectedGlobal:  l.rejectedGlobal.Load(),
			RejectedPort:    l.rejectedPort.Load(),
			RejectedClosing: l.rejectedClosing.Load(),
			DialOK:          l.dialOK.Load(),
			DialFail:        l.dialFail.Load(),
			BindErrors:      l.bindErrors.Load(),
			AcceptErrors:    l.acceptErrors.Load(),
			RelayUpBytes:    l.relayUpBytes.Load(),
			RelayDownBytes:  l.relayDownBytes.Load(),
			DialDurationNs:  l.dialDurationNs.Load(),
			DialCount:       l.dialCount.Load(),
		}
		for i := range snapshot.DialDurationBuckets {
			snapshot.DialDurationBuckets[i] = l.dialBuckets[i].Load()
		}
		snapshots = append(snapshots, snapshot)
	}
	// Keep configured-but-not-running bridges visible with listener_up=0. This
	// is important for bind failures and for a bridge restored from bridge.db
	// that has not yet been started.
	if cf != nil {
		for _, configured := range cf.All() {
			if configured == nil {
				continue
			}
			if _, ok := listenersByPort[configured.Port]; ok {
				continue
			}
			snapshots = append(snapshots, bridgeMetricSnapshot{
				BridgePort: configured.Port,
				ProxyAddr:  configured.ProxyAddr,
			})
		}
	}
	return snapshots
}

func CollectBridgeStats() BridgeStats {
	return bridgeStatsFromMetricSnapshots(collectBridgeMetricSnapshots())
}

func bridgeStatsFromMetricSnapshots(snapshots []bridgeMetricSnapshot) BridgeStats {
	var s BridgeStats
	for _, snapshot := range snapshots {
		if !snapshot.Running {
			continue
		}
		s.Bridges++
		if snapshot.Listening {
			s.Listening++
		}
		s.Conns += snapshot.Conns
		s.Accepted += snapshot.Accepted
		s.Rejected += snapshot.RejectedGlobal + snapshot.RejectedPort
		s.DialOK += snapshot.DialOK
		s.DialFail += snapshot.DialFail
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
			recordListenerError(l, "bind")
			bindMessage := err.Error()
			l.setBindErrMessage(bindMessage)
			// Do not block the supervisor or WaitBridgeListening on a process lookup.
			// The raw bind error is available immediately; the owner details are
			// appended after the platform lookup completes.
			if isAddressInUse(err) && l.portOwnerLogged.CompareAndSwap(false, true) {
				go l.lookupPortOwner(port, bindMessage)
			}
			backoff = nextBackoff(backoff)
			slog.Error("bind failed, retrying", "port", port,
				"toAddr", l.currentTarget(), "backoff", backoff, "err", bindMessage)
			if !l.sleepOrStop(backoff) {
				return
			}
			continue
		}

		// 注意这里不把 backoff 归零：bind 成功不等于故障消失，accept 可能立刻
		// 因为同一个原因（EMFILE 等）退出。归零的判定放在 nextRebindBackoff 里，
		// 以「这次监听是否稳定服务过」为准。
		l.clearBindErr()
		l.portOwnerLogged.Store(false)
		if !l.installListener(ln) {
			return
		}
		listenStarted := time.Now()
		slog.Info("listen", "port", port, "toAddr", l.currentTarget())

		acceptErr := l.acceptLoop(port, ln, fn)

		l.clearListener()
		ln.Close()

		if l.stopRequested() {
			return
		}
		backoff = nextRebindBackoff(backoff, time.Since(listenStarted))
		slog.Error("accept loop exited, rebinding", "port", port,
			"backoff", backoff, "uptime", time.Since(listenStarted), "err", acceptErr)
		if !l.sleepOrStop(backoff) {
			return
		}
	}
}

func (l *bridgeListener) lookupPortOwner(port uint16, bindMessage string) {
	if l.closed.Load() {
		return
	}
	owner := lookupPortOwner(int(port))
	if owner == "" || l.closed.Load() || l.listening.Load() || l.lastBindErr() != bindMessage {
		return
	}
	slog.Error("bind port owner", "port", port, "toAddr", l.currentTarget(), "owner", owner)
	if !l.closed.Load() && !l.listening.Load() {
		l.setBindErrMessage(bindMessage + "; " + owner)
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
			recordListenerError(l, "accept")
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
		recordAccepted(l)

		if !acquireGlobalConn() {
			recordRejected(l, "global_limit")
			slog.Warn("conn rejected by global limit", "port", port,
				"srcaddr", conn.RemoteAddr().String(), "maxConns", config.Cfg.MaxConns)
			conn.Close()
			continue
		}
		switch l.addConn(conn) {
		case addConnOK:
		case addConnClosing:
			releaseGlobalConn()
			recordRejected(l, "closing")
			slog.Info("conn dropped, bridge is shutting down", "port", port,
				"srcaddr", conn.RemoteAddr().String())
			conn.Close()
			continue
		case addConnLimit:
			releaseGlobalConn()
			recordRejected(l, "port_limit")
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
	if cur <= 0 {
		return retryMin
	}
	next := cur * 2
	if next > retryMax {
		return retryMax
	}
	return next
}

// nextRebindBackoff 决定 accept 循环退出后、重建 listener 之前等多久。
// uptime 是这次监听存活的时间：只有稳定服务过 listenStableFor 才认为故障已经
// 恢复、可以从最小值重新开始；否则在上一次的基础上继续指数增长。
//
// 之前的写法是 bind 成功就把 backoff 归零，于是「bind 成功 → accept 立刻失败」
// 这种持续故障永远只退避 50ms，指数退避形同虚设。
func nextRebindBackoff(cur, uptime time.Duration) time.Duration {
	if uptime >= listenStableFor {
		return retryMin
	}
	return nextBackoff(cur)
}

func handlerBridge(conn net.Conn, toAddr string) {
	srcaddr := conn.RemoteAddr().String()
	defer conn.Close()
	listener := listenerForConn(conn)
	dialStarted := time.Now()

	dstConn, err := net.DialTimeout("tcp", toAddr, dialTimeout())
	if err != nil {
		recordDial(listener, false, time.Since(dialStarted))
		slog.Error("dial target", "srcaddr", srcaddr, "toAddr", toAddr, "err", err)
		return
	}
	recordDial(listener, true, time.Since(dialStarted))
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
		recordPipeResult(listener, "flow-up", srcaddr, toAddr, rn, err)
		// 任一方向结束就把两端都关掉，让另一个方向立刻解除阻塞
		dstConn.Close()
		conn.Close()
	}()

	rn, err := pipe(src, dst)
	recordPipeResult(listener, "flow-down", srcaddr, toAddr, rn, err)
	conn.Close()
	dstConn.Close()
	wg.Wait()
}

// recordPipeResult updates flow metrics and logs the result in one place. This
// keeps the byte counter and the corresponding flow log from drifting apart.
func recordPipeResult(l *bridgeListener, direction, srcaddr, toAddr string, amount int64, err error) {
	recordRelayBytes(l, direction, amount)
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

func listenerForConn(conn net.Conn) *bridgeListener {
	addr, isTCP := conn.LocalAddr().(*net.TCPAddr)
	if !isTCP {
		return nil
	}
	runMu.RLock()
	l := runListens[uint16(addr.Port)]
	runMu.RUnlock()
	return l
}

func recordAccepted(l *bridgeListener) {
	if l != nil {
		l.accepted.Add(1)
	}
	metricTotals.Accepted.Add(1)
}

func recordRejected(l *bridgeListener, reason string) {
	if l != nil {
		l.rejected.Add(1)
		switch reason {
		case "global_limit":
			l.rejectedGlobal.Add(1)
		case "port_limit":
			l.rejectedPort.Add(1)
		case "closing":
			l.rejectedClosing.Add(1)
		}
	}
	switch reason {
	case "closing":
		metricTotals.RejectedClosing.Add(1)
	case "global_limit":
		metricTotals.RejectedGlobal.Add(1)
	case "port_limit":
		metricTotals.RejectedPort.Add(1)
	}
}

func recordListenerError(l *bridgeListener, stage string) {
	if l != nil {
		switch stage {
		case "bind":
			l.bindErrors.Add(1)
		case "accept":
			l.acceptErrors.Add(1)
		}
	}
	switch stage {
	case "bind":
		metricTotals.BindErrors.Add(1)
	case "accept":
		metricTotals.AcceptErrors.Add(1)
	}
}

func recordDial(l *bridgeListener, ok bool, duration time.Duration) {
	bucket := dialDurationBucket(duration)
	if l != nil {
		if ok {
			l.dialOK.Add(1)
		} else {
			l.dialFail.Add(1)
		}
		l.dialDurationNs.Add(duration.Nanoseconds())
		l.dialCount.Add(1)
		l.dialBuckets[bucket].Add(1)
	}
	if ok {
		metricTotals.DialOK.Add(1)
	} else {
		metricTotals.DialFail.Add(1)
	}
	metricTotals.DialDurationNs.Add(uint64(duration.Nanoseconds()))
	metricTotals.DialCount.Add(1)
	metricTotals.DialBuckets[bucket].Add(1)
}

func recordRelayBytes(l *bridgeListener, direction string, amount int64) {
	if amount <= 0 {
		return
	}
	if l != nil {
		if direction == "up" {
			l.relayUpBytes.Add(amount)
		} else {
			l.relayDownBytes.Add(amount)
		}
	}
	if direction == "up" {
		metricTotals.RelayUpBytes.Add(uint64(amount))
	} else {
		metricTotals.RelayDownBytes.Add(uint64(amount))
	}
}

var copyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

// pipe 单向搬运。
//
// 关键在于什么时候**不**需要缓冲。io.CopyBuffer 的判定顺序是：先看 src 有没有
// WriteTo、再看 dst 有没有ReadFrom，两者都没有才使用传进来的缓冲
// （见 io/io.go copyBuffer）。而 *net.TCPConn 同时实现了这两个方法，
// TCPConn.writeTo 在 Linux 上直接走 spliceTo → splice(2)，全程不碰用户态内存。
//
// 也就是说 TCP↔TCP 这条主路径上，池化缓冲从来没被读写过，却被持有到连接结束：
// 每个方向白占 32KB。线上一条 stats 日志印证了这一点——conns=15716
// （31432 个方向）时 heapAllocMB=1036，31432×32KB≈982MB，几乎就是全部堆。
//
// 所以这里只在两条快路径都不存在时才取缓冲。判定逻辑与 io.copyBuffer 保持一致，
// 命中的正是包了 idleTimeoutConn 或非 TCP（测试里的 net.Pipe）的回退路径。
func pipe(dst, src net.Conn) (int64, error) {
	if hasKernelCopyFastPath(dst, src) {
		return io.Copy(dst, src)
	}
	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	return io.CopyBuffer(dst, src, *bufp)
}

// hasKernelCopyFastPath 判断 io.Copy 会不会绕过用户态缓冲。
//
// 刻意用接口断言而不是 *net.TCPConn 类型断言：io.copyBuffer 就是这么判的，
// 保持一致才能让「我们不取缓冲」和「它不需要缓冲」永远同时成立。
// idleTimeoutConn 内嵌的是 net.Conn 接口而不是 *net.TCPConn，
// WriteTo/ReadFrom 不会被提升，所以包装过的连接会正确地落到回退路径。
func hasKernelCopyFastPath(dst, src net.Conn) bool {
	if _, ok := src.(io.WriterTo); ok {
		return true
	}
	_, ok := dst.(io.ReaderFrom)
	return ok
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

// dialTimeout 是拨号到上游目标的超时，来自配置，缺省 10 秒。
//
// 只覆盖 TCP 握手：SOCKS5 的方法协商、认证、CONNECT 都发生在握手之后，
// 是被透传的字节流，不受它约束。所以上游「握手很快、CONNECT 慢慢拒绝」的场景
// （实测能到 7~10 秒）不会被这个超时截断。
//
// 不做 ApplyDefaults 之外的兜底：非法值在启动时就已经被换成缺省值；
// 这里再判一次 <=0 只是为了单元测试里直接改 config.Cfg 时不至于变成无超时。
func dialTimeout() time.Duration {
	if config.Cfg.DialTimeout <= 0 {
		return time.Duration(config.DefaultDialTimeoutSeconds) * time.Second
	}
	return time.Duration(config.Cfg.DialTimeout) * time.Second
}

func maxConnsPerPort() int {
	if config.Cfg.MaxConnsPerPort <= 0 {
		return 0
	}
	return config.Cfg.MaxConnsPerPort
}
