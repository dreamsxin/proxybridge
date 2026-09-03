package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baowk/bridge-direct/cachef"
	"github.com/baowk/bridge-direct/config"
	"github.com/baowk/bridge-direct/server/dto"
	"github.com/baowk/bridge-direct/utils"
	"github.com/gin-gonic/gin"
)

const testKey = "abcd1234poiu5678bvbvnbnb"

func TestAddBridgeUpdatesExistingBridgePort(t *testing.T) {
	setupServerTest(t)

	firstTarget := startTCPServer(t, "first")
	secondTarget := startTCPServer(t, "second")
	bridgePort := freeTCPPort(t)

	performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort,
		Ip:         firstTarget.host,
		Port:       firstTarget.port,
	})
	assertProxyResponse(t, bridgePort, "first")

	performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort,
		Ip:         secondTarget.host,
		Port:       secondTarget.port,
	})
	assertProxyResponse(t, bridgePort, "second")
}

func TestGetBridgeStatusReportsHealthyRuntime(t *testing.T) {
	setupServerTest(t)
	target := startTCPServer(t, "status")
	bridgePort := freeTCPPort(t)
	proxyAddr := net.JoinHostPort(target.host, formatPort(target.port))
	if resp := performAddBridge(t, dto.UseBridge{BridgePort: bridgePort, Ip: target.host, Port: target.port}); resp.Code != 200 {
		t.Fatalf("add failed: %+v", resp)
	}

	statuses := RuntimeBridgeStatus(bridgePort, proxyAddr, true)
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one result", statuses)
	}
	status := statuses[0]
	if status.BridgePort != bridgePort || status.ProxyAddr != proxyAddr || !status.Listening || !status.BridgeTCP || !status.ProxyTCP || !status.OK {
		t.Fatalf("unhealthy runtime status: %+v", status)
	}

	req := httptest.NewRequest(http.MethodGet, "/bridge/status?bridgePort="+formatPort(bridgePort)+"&check=1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	GetBridgeStatus(c)
	var resp struct {
		Code int                `json:"code"`
		Data []dto.BridgeStatus `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response %q: %v", rec.Body.String(), err)
	}
	if resp.Code != 200 || len(resp.Data) != 1 || !resp.Data[0].OK {
		t.Fatalf("unexpected status response: %+v", resp)
	}
}

func TestGetBridgeStatusIncludesConfiguredMissingListener(t *testing.T) {
	setupServerTest(t)
	target := startTCPServer(t, "configured")
	bridgePort := freeTCPPort(t)
	proxyAddr := net.JoinHostPort(target.host, formatPort(target.port))
	if err := cf.Add(bridgePort, proxyAddr); err != nil {
		t.Fatal(err)
	}

	statuses := RuntimeBridgeStatus(bridgePort, "", false)
	if len(statuses) != 1 || statuses[0].Listening || statuses[0].OK {
		t.Fatalf("missing-listener status = %+v", statuses)
	}
	if !strings.Contains(statuses[0].FailureReason, "监听器") {
		t.Fatalf("missing-listener reason = %q", statuses[0].FailureReason)
	}
}

func TestBridgeStatusAndStartRejectNonLoopback(t *testing.T) {
	setupServerTest(t)
	for name, test := range map[string]struct {
		handler gin.HandlerFunc
		method  string
		path    string
	}{
		"status": {GetBridgeStatus, http.MethodGet, "/bridge/status"},
		"start":  {StartBridge, http.MethodPost, "/bridge/start?bridgePort=12345"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, nil)
			req.RemoteAddr = "192.0.2.1:12345"
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = req
			test.handler(c)
			var resp dto.Res
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response %q: %v", rec.Body.String(), err)
			}
			if resp.Code != http.StatusForbidden {
				t.Fatalf("response = %+v, want code %d", resp, http.StatusForbidden)
			}
		})
	}
}

func TestStartBridgeRestoresConfiguredListener(t *testing.T) {
	setupServerTest(t)
	target := startTCPServer(t, "started")
	bridgePort := freeTCPPort(t)
	proxyAddr := net.JoinHostPort(target.host, formatPort(target.port))
	if err := cf.Add(bridgePort, proxyAddr); err != nil {
		t.Fatal(err)
	}

	first, firstResult := performStartBridgeRequest(t, "127.0.0.1:12345", bridgePort)
	if first.Code != 200 {
		t.Fatalf("start response = %+v", first)
	}
	if firstResult.Status != "started" {
		t.Fatalf("first start result = %+v", firstResult)
	}
	assertProxyResponse(t, bridgePort, "started")

	second, secondResult := performStartBridgeRequest(t, "127.0.0.1:12345", bridgePort)
	if second.Code != 200 {
		t.Fatalf("repeat start response = %+v", second)
	}
	if secondResult.Status != "alreadyListening" {
		t.Fatalf("repeat start result = %+v", secondResult)
	}
}

func TestStartBridgeReportsTargetConflict(t *testing.T) {
	setupServerTest(t)
	configuredTarget := startTCPServer(t, "configured")
	runtimeTarget := startTCPServer(t, "runtime")
	bridgePort := freeTCPPort(t)
	configuredAddr := net.JoinHostPort(configuredTarget.host, formatPort(configuredTarget.port))
	runtimeAddr := net.JoinHostPort(runtimeTarget.host, formatPort(runtimeTarget.port))
	if err := cf.Add(bridgePort, configuredAddr); err != nil {
		t.Fatal(err)
	}
	if err := SetBridgeHandler(bridgePort, runtimeAddr); err != nil {
		t.Fatal(err)
	}
	if ready, bindErr := WaitBridgeListening(bridgePort, 2*time.Second); !ready {
		t.Fatalf("runtime listener not ready: %s", bindErr)
	}

	resp, result := performStartBridgeRequest(t, "127.0.0.1:12345", bridgePort)
	if resp.Code == 200 || result.Status != "conflict" {
		t.Fatalf("conflict response=%+v result=%+v", resp, result)
	}
	assertProxyResponse(t, bridgePort, "runtime")
}

func TestStartBridgeReportsBindFailureAndKeepsRetrying(t *testing.T) {
	setupServerTest(t)
	target := startTCPServer(t, "recovered")
	blocker, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			blocker.Close()
		}
	}()
	bridgePort := portOf(t, blocker.Addr())
	proxyAddr := net.JoinHostPort(target.host, formatPort(target.port))
	if err := cf.Add(bridgePort, proxyAddr); err != nil {
		t.Fatal(err)
	}

	resp, result := performStartBridgeRequest(t, "127.0.0.1:12345", bridgePort)
	if resp.Code == 200 || result.Status != "failed" || !result.Retrying || result.Err == "" {
		t.Fatalf("bind failure response=%+v result=%+v", resp, result)
	}
	statuses := RuntimeBridgeStatus(bridgePort, "", false)
	if len(statuses) != 1 || statuses[0].Listening || statuses[0].BindErr == "" {
		t.Fatalf("bind-retry status = %+v", statuses)
	}

	blocker.Close()
	closed = true
	waitFor(t, 15*time.Second, func() bool { return hasBridgeHandler(bridgePort) })
	assertProxyResponse(t, bridgePort, "recovered")
}

func TestAddBridgeRejectsInvalidIP(t *testing.T) {
	setupServerTest(t)

	resp := performAddBridge(t, dto.UseBridge{
		BridgePort: freeTCPPort(t),
		Ip:         "not-an-ip",
		Port:       8080,
	})
	if resp.Code == 200 {
		t.Fatalf("expected invalid ip failure, got %+v", resp)
	}
}

// 重复添加同一个桥必须收敛到同一个状态：一条记录、一个监听
func TestAddBridgeIsIdempotent(t *testing.T) {
	setupServerTest(t)

	target := startTCPServer(t, "first")
	bridgePort := freeTCPPort(t)
	bridge := dto.UseBridge{BridgePort: bridgePort, Ip: target.host, Port: target.port}

	for i := 0; i < 3; i++ {
		if resp := performAddBridge(t, bridge); resp.Code != 200 {
			t.Fatalf("add #%d failed: %+v", i+1, resp)
		}
	}

	assertConsistent(t, bridgePort)
	if got := cacheEntryCount(bridgePort); got != 1 {
		t.Fatalf("cache entries = %d, want 1", got)
	}
	assertProxyResponse(t, bridgePort, "first")
}

func TestDelBridgeIsIdempotent(t *testing.T) {
	setupServerTest(t)

	target := startTCPServer(t, "first")
	bridgePort := freeTCPPort(t)
	if resp := performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort, Ip: target.host, Port: target.port,
	}); resp.Code != 200 {
		t.Fatalf("add failed: %+v", resp)
	}

	for i := 0; i < 3; i++ {
		resp, err := doBridgeRequest("/bridge/del", dto.UseBridge{BridgePort: bridgePort}, DelBridge)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Code != 200 {
			t.Fatalf("del #%d failed: %+v", i+1, resp)
		}
	}

	assertConsistent(t, bridgePort)
	if got := cacheEntryCount(bridgePort); got != 0 {
		t.Fatalf("cache entries = %d, want 0", got)
	}
}

// bind 失败（端口被别的进程占用）：管理请求直接收到错误并带上 bind 原因，
// 但记录保留、supervisor 继续退避重试，端口一释放就自动补上监听，无需中心侧重推。
func TestBindFailureReportsErrorAndKeepsRetrying(t *testing.T) {
	setupServerTest(t)

	target := startTCPServer(t, "first")

	// 用一个通配地址的监听占住桥端口，让 net.Listen 必然失败
	blocker, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	blockerClosed := false
	defer func() {
		if !blockerClosed {
			blocker.Close()
		}
	}()
	bridgePort := portOf(t, blocker.Addr())

	bridge := dto.UseBridge{BridgePort: bridgePort, Ip: target.host, Port: target.port}

	resp := performAddBridge(t, bridge)
	if resp.Code == 200 {
		t.Fatalf("port %d is occupied, add must report an error, got %+v", bridgePort, resp)
	}
	if !strings.Contains(resp.Msg, "not listening") {
		t.Fatalf("error message should say why it is not listening, got %q", resp.Msg)
	}
	if listening := respListening(t, resp); listening {
		t.Fatal("failed add should report listening=false")
	}
	assertConsistent(t, bridgePort)
	if got := cacheEntryCount(bridgePort); got != 1 {
		t.Fatalf("cache entries = %d, want 1 (record kept for self-healing)", got)
	}

	blocker.Close()
	blockerClosed = true

	// 无需再次 add：supervisor 自己会重试 bind
	waitFor(t, 15*time.Second, func() bool { return hasBridgeHandler(bridgePort) })
	assertConsistent(t, bridgePort)
	assertProxyResponse(t, bridgePort, "first")
}

// 撞上本进程自己的端口（管理端口/pprof）必须在入口拒掉：
// 这类冲突 bind 永远不可能成功，放进去只会让 supervisor 无限重试
func TestAddBridgeRejectsSelfPortConflict(t *testing.T) {
	setupServerTest(t)

	prevAddr, prevPprof := config.Cfg.Addr, config.Cfg.PprofAddr
	config.Cfg.Addr = ":18080"
	config.Cfg.PprofAddr = "127.0.0.1:16060"
	t.Cleanup(func() {
		config.Cfg.Addr, config.Cfg.PprofAddr = prevAddr, prevPprof
	})

	target := startTCPServer(t, "first")

	for _, port := range []uint16{18080, 16060} {
		resp := performAddBridge(t, dto.UseBridge{
			BridgePort: port, Ip: target.host, Port: target.port,
		})
		if resp.Code == 200 {
			t.Fatalf("port %d conflicts with our own listener, add must fail, got %+v", port, resp)
		}
		if !strings.Contains(resp.Msg, "conflicts with") {
			t.Fatalf("port %d: error message = %q, want a conflict explanation", port, resp.Msg)
		}
		if got := cacheEntryCount(port); got != 0 {
			t.Fatalf("port %d: rejected add left %d cache entries, want 0", port, got)
		}
		if got := bridgeListenerCount(port); got != 0 {
			t.Fatalf("port %d: rejected add left %d listeners, want 0", port, got)
		}
	}
}

// 并发添加同一个桥端口（目标交替）：不能有请求失败，也不能出现重复记录
func TestConcurrentAddSameBridgePort(t *testing.T) {
	setupServerTest(t)

	first := startTCPServer(t, "first")
	second := startTCPServer(t, "second")
	bridgePort := freeTCPPort(t)

	const n = 8
	results := make([]dto.Res, n)
	errs := make([]error, n)

	runWithTimeout(t, 30*time.Second, func() {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				target := first
				if i%2 == 1 {
					target = second
				}
				results[i], errs[i] = doBridgeRequest("/bridge/add", dto.UseBridge{
					BridgePort: bridgePort, Ip: target.host, Port: target.port,
				}, AddBridge)
			}(i)
		}
		wg.Wait()
	})

	for i := range results {
		if errs[i] != nil {
			t.Fatalf("add #%d: %v", i, errs[i])
		}
		if results[i].Code != 200 {
			t.Fatalf("add #%d failed: %+v", i, results[i])
		}
	}

	assertConsistent(t, bridgePort)
	if got := cacheEntryCount(bridgePort); got != 1 {
		t.Fatalf("cache entries = %d, want 1", got)
	}

	// 持久化记录必须和实际转发目标一致
	entry := cf.Get(bridgePort)
	if entry == nil {
		t.Fatal("no cache entry after concurrent adds")
	}
	want := "first"
	if entry.ProxyAddr == net.JoinHostPort(second.host, formatPort(second.port)) {
		want = "second"
	}
	assertProxyResponse(t, bridgePort, want)
}

// 并发 add/del 同一端口：最终是有桥还是无桥取决于调度，
// 但「持久化记录」和「实际监听」两份状态绝不能分叉
func TestConcurrentAddAndDelSameBridgePort(t *testing.T) {
	setupServerTest(t)

	target := startTCPServer(t, "first")
	bridgePort := freeTCPPort(t)

	const rounds = 6
	runWithTimeout(t, 30*time.Second, func() {
		var wg sync.WaitGroup
		for i := 0; i < rounds; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, _ = doBridgeRequest("/bridge/add", dto.UseBridge{
					BridgePort: bridgePort, Ip: target.host, Port: target.port,
				}, AddBridge)
			}()
			go func() {
				defer wg.Done()
				_, _ = doBridgeRequest("/bridge/del", dto.UseBridge{BridgePort: bridgePort}, DelBridge)
			}()
		}
		wg.Wait()
	})

	assertConsistent(t, bridgePort)

	// 收敛检查：并发结束后再来一次 add，必须成功且状态一致
	if resp := performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort, Ip: target.host, Port: target.port,
	}); resp.Code != 200 {
		t.Fatalf("add after concurrent churn should succeed, got %+v", resp)
	}
	assertConsistent(t, bridgePort)
	assertProxyResponse(t, bridgePort, "first")
}

// 中心侧返回空列表不能把本地映射表擦掉：擦掉之后本地再没有副本可恢复
func TestSyncBridgeRejectsEmptyRemoteList(t *testing.T) {
	setupServerTest(t)
	if err := cf.Add(8001, "1.2.3.4:80"); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":200,"msg":"ok","data":[]}`)
	}))
	t.Cleanup(ts.Close)
	prev := config.Cfg.SyncDomain
	config.Cfg.SyncDomain = ts.URL
	t.Cleanup(func() { config.Cfg.SyncDomain = prev })

	if err := syncBridge(); err == nil {
		t.Fatal("expected an empty remote bridge list to be rejected")
	}
	if got := cacheEntryCount(8001); got != 1 {
		t.Fatalf("cache entries for 8001 = %d, want 1 (local data must survive)", got)
	}
}

// 正常同步用远端集合整体替换本地：本地多出来的记录要被移除
func TestSyncBridgeReplacesLocalSet(t *testing.T) {
	setupServerTest(t)
	if err := cf.Add(8001, "1.2.3.4:80"); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":200,"msg":"ok","data":[{"port":8002,"proxyAddr":"5.6.7.8:90"}]}`)
	}))
	t.Cleanup(ts.Close)
	prev := config.Cfg.SyncDomain
	config.Cfg.SyncDomain = ts.URL
	t.Cleanup(func() { config.Cfg.SyncDomain = prev })

	if err := syncBridge(); err != nil {
		t.Fatal(err)
	}
	if got := cacheEntryCount(8001); got != 0 {
		t.Fatalf("cache entries for 8001 = %d, want 0 after replace", got)
	}
	entry := cf.Get(8002)
	if entry == nil || entry.ProxyAddr != "5.6.7.8:90" {
		t.Fatalf("port 8002 = %+v, want proxyAddr 5.6.7.8:90", entry)
	}
}

// 管理接口的锁必须是端口粒度：一次慢操作（DelBridgeHandler 最坏等 stopTimeout=10s，
// 或一次卡住的 fsync）只能堵住同端口的请求，不能把其它端口的请求排在后面
func TestManagementLockIsPerPort(t *testing.T) {
	setupServerTest(t)

	target := startTCPServer(t, "first")
	busyPort := freeTCPPort(t)
	otherPort := freeTCPPort(t)

	// 直接占住 busyPort 的管理锁，等价于该端口上正在跑一次慢操作
	release := lockPort(busyPort)
	releaseOnce := sync.OnceFunc(release)
	defer releaseOnce()

	other := make(chan dto.Res, 1)
	go func() {
		res, _ := doBridgeRequest("/bridge/add", dto.UseBridge{
			BridgePort: otherPort, Ip: target.host, Port: target.port,
		}, AddBridge)
		other <- res
	}()
	select {
	case res := <-other:
		if res.Code != 200 {
			t.Fatalf("add on a free port failed: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("add on another port was blocked by a busy port: the management lock is global")
	}

	// 同一端口仍然必须互斥，否则就回到了状态分叉
	same := make(chan struct{})
	go func() {
		defer close(same)
		_, _ = doBridgeRequest("/bridge/add", dto.UseBridge{
			BridgePort: busyPort, Ip: target.host, Port: target.port,
		}, AddBridge)
	}()
	select {
	case <-same:
		t.Fatal("same-port add was not serialized against the held lock")
	case <-time.After(200 * time.Millisecond):
	}

	releaseOnce()
	select {
	case <-same:
	case <-time.After(5 * time.Second):
		t.Fatal("same-port add did not proceed after the lock was released")
	}

	assertConsistent(t, busyPort)
	assertConsistent(t, otherPort)
}

// 目标未变且监听在跑时，重复 add 不能重建监听，否则会踢掉该端口上所有在途连接
func TestAddBridgeUnchangedDoesNotRebuildListener(t *testing.T) {
	setupServerTest(t)

	target := startTCPServer(t, "first")
	bridgePort := freeTCPPort(t)
	bridge := dto.UseBridge{BridgePort: bridgePort, Ip: target.host, Port: target.port}

	if resp := performAddBridge(t, bridge); resp.Code != 200 {
		t.Fatalf("first add failed: %+v", resp)
	}
	before := bridgeListenerOf(bridgePort)
	if before == nil {
		t.Fatal("no listener after first add")
	}

	if resp := performAddBridge(t, bridge); resp.Code != 200 {
		t.Fatalf("repeat add failed: %+v", resp)
	}

	if after := bridgeListenerOf(bridgePort); after != before {
		t.Fatal("repeat add with unchanged target rebuilt the listener")
	}
	select {
	case <-before.done:
		t.Fatal("no-op add stopped the accept loop")
	default:
	}

	assertConsistent(t, bridgePort)
	assertProxyResponse(t, bridgePort, "first")
}

// 改目标不应重建监听：重建会有「端口未监听」窗口，期间新连接被 RST，
// 而且重新 bind 还可能失败。目标用原子替换即可。
func TestAddBridgeChangedTargetKeepsListener(t *testing.T) {
	setupServerTest(t)

	first := startTCPServer(t, "first")
	second := startTCPServer(t, "second")
	bridgePort := freeTCPPort(t)

	if resp := performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort, Ip: first.host, Port: first.port,
	}); resp.Code != 200 {
		t.Fatalf("first add failed: %+v", resp)
	}
	before := bridgeListenerOf(bridgePort)
	if before == nil {
		t.Fatal("no listener after first add")
	}
	assertProxyResponse(t, bridgePort, "first")

	if resp := performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort, Ip: second.host, Port: second.port,
	}); resp.Code != 200 {
		t.Fatalf("change target failed: %+v", resp)
	}

	if after := bridgeListenerOf(bridgePort); after != before {
		t.Fatal("retarget should not have rebuilt the listener")
	}
	select {
	case <-before.done:
		t.Fatal("retarget stopped the accept loop")
	default:
	}

	assertConsistent(t, bridgePort)
	assertProxyResponse(t, bridgePort, "second")
}

// Accept 遇到可恢复错误（线上最典型的是 fd 耗尽）必须退避重试，
// 只有 listener 真正失效才退出。旧实现一次错误就永久停止服务。
func TestAcceptLoopRetriesTransientErrors(t *testing.T) {
	transient := errors.New("accept: too many open files")
	fl := &fakeListener{results: []fakeAcceptResult{
		{err: transient},
		{err: transient},
		{err: net.ErrClosed},
	}}
	l := newBridgeListener("127.0.0.1:1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := l.acceptLoop(0, fl, func(net.Conn, string) {}); err != nil {
			t.Errorf("acceptLoop returned %v, want nil on ErrClosed", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("acceptLoop did not exit")
	}
	if got := fl.callCount(); got != 3 {
		t.Fatalf("Accept called %d times, want 3 (two retries then ErrClosed)", got)
	}
}

// 删除桥必须断开在途连接：只关 listener 会留下「桥已删除、流量仍在转发」
func TestDelBridgeClosesActiveConns(t *testing.T) {
	setupServerTest(t)

	target := startHoldingTCPServer(t)
	bridgePort := freeTCPPort(t)
	if resp := performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort, Ip: target.host, Port: target.port,
	}); resp.Code != 200 {
		t.Fatalf("add failed: %+v", resp)
	}

	client, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", formatPort(bridgePort)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	waitFor(t, 2*time.Second, func() bool { return bridgeConnCount(bridgePort) == 1 })

	resp, err := doBridgeRequest("/bridge/del", dto.UseBridge{BridgePort: bridgePort}, DelBridge)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Code != 200 {
		t.Fatalf("del failed: %+v", resp)
	}

	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err = client.Read(buf)
	if err == nil {
		t.Fatal("connection still alive after DelBridge")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("DelBridge did not close the in-flight connection (read timed out)")
	}
}

// 删除时持久化失败必须恢复原监听：接口报错后，旧桥仍应可用。
func TestDelBridgeRestoresListenerWhenPersistFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetListeners(t)
	t.Cleanup(func() { resetListeners(t) })
	config.Cfg.Key = testKey

	dir := t.TempDir()
	filename := filepath.Join(dir, "bridge.db")
	var err error
	cf, err = cachef.New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)

	target := startTCPServer(t, "first")
	bridgePort := freeTCPPort(t)
	if resp := performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort, Ip: target.host, Port: target.port,
	}); resp.Code != 200 {
		t.Fatalf("add failed: %+v", resp)
	}

	// Rename the cache directory so cf.Del's next atomic dump cannot create
	// its temporary file. This is deterministic and works without permission
	// assumptions on either Windows or Unix.
	movedDir := dir + ".moved"
	if err := os.Rename(dir, movedDir); err != nil {
		t.Fatal(err)
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			_ = os.Rename(movedDir, dir)
		}
	})

	resp, err := doBridgeRequest("/bridge/del", dto.UseBridge{BridgePort: bridgePort}, DelBridge)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Code == 200 {
		t.Fatalf("delete should fail when cache dump fails: %+v", resp)
	}
	if !strings.Contains(resp.Msg, "no such file") && !strings.Contains(resp.Msg, "cannot find") {
		t.Logf("delete failure message: %q", resp.Msg)
	}

	if err := os.Rename(movedDir, dir); err != nil {
		t.Fatal(err)
	}
	restored = true
	waitFor(t, 2*time.Second, func() bool { return hasBridgeHandler(bridgePort) })
	assertConsistent(t, bridgePort)
	assertProxyResponse(t, bridgePort, "first")
}

// add 切换目标时持久化失败必须恢复旧目标，而不是留下新目标的监听。
func TestAddBridgeRestoresPreviousTargetWhenPersistFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetListeners(t)
	t.Cleanup(func() { resetListeners(t) })
	config.Cfg.Key = testKey
	dir := t.TempDir()
	var err error
	cf, err = cachef.New(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)

	first := startTCPServer(t, "first")
	second := startTCPServer(t, "second")
	bridgePort := freeTCPPort(t)
	if resp := performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort, Ip: first.host, Port: first.port,
	}); resp.Code != 200 {
		t.Fatalf("initial add failed: %+v", resp)
	}

	// Hide the cache directory so the next cf.Add dump fails after the listener
	// has been retargeted. The rollback must put both layers back to first.
	movedDir := dir + ".moved"
	if err := os.Rename(dir, movedDir); err != nil {
		t.Fatal(err)
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			_ = os.Rename(movedDir, dir)
		}
	})

	resp := performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort, Ip: second.host, Port: second.port,
	})
	if resp.Code == 200 {
		t.Fatalf("switch should fail when cache dump fails: %+v", resp)
	}
	if err := os.Rename(movedDir, dir); err != nil {
		t.Fatal(err)
	}
	restored = true

	entry := cf.Get(bridgePort)
	if entry == nil || entry.ProxyAddr != net.JoinHostPort(first.host, formatPort(first.port)) {
		t.Fatalf("cache target after failed switch = %+v, want first target", entry)
	}
	assertConsistent(t, bridgePort)
	assertProxyResponse(t, bridgePort, "first")
}

// Set 后立即 Del 可能发生在 supervisor 完成 bind 之前；反复执行后端口必须仍可复用。
func TestImmediateSetDeleteDoesNotLeakListener(t *testing.T) {
	setupServerTest(t)
	bridgePort := freeTCPPort(t)

	for i := 0; i < 200; i++ {
		if err := SetBridgeHandler(bridgePort, "127.0.0.1:1"); err != nil {
			t.Fatalf("set #%d: %v", i, err)
		}
		if err := DelBridgeHandler(bridgePort); err != nil {
			t.Fatalf("del #%d: %v", i, err)
		}
	}

	if got := bridgeListenerCount(bridgePort); got != 0 {
		t.Fatalf("listeners after immediate set/delete = %d, want 0", got)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", bridgePort))
	if err != nil {
		t.Fatalf("port %d cannot be rebound after immediate set/delete: %v", bridgePort, err)
	}
	ln.Close()
}

// 超过单端口连接上限的连接应被立刻拒绝，给 fd 用量兜底
func TestMaxConnsPerPortRejectsExcess(t *testing.T) {
	setupServerTest(t)

	prev := config.Cfg.MaxConnsPerPort
	config.Cfg.MaxConnsPerPort = 1
	t.Cleanup(func() { config.Cfg.MaxConnsPerPort = prev })

	target := startHoldingTCPServer(t)
	bridgePort := freeTCPPort(t)
	if resp := performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort, Ip: target.host, Port: target.port,
	}); resp.Code != 200 {
		t.Fatalf("add failed: %+v", resp)
	}
	addr := net.JoinHostPort("127.0.0.1", formatPort(bridgePort))

	first, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	waitFor(t, 2*time.Second, func() bool { return bridgeConnCount(bridgePort) == 1 })

	second, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	second.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err = second.Read(buf)
	if err == nil {
		t.Fatal("connection over the limit was not rejected")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("connection over the limit was not rejected (read timed out)")
	}

	if got := bridgeConnCount(bridgePort); got != 1 {
		t.Fatalf("active conns = %d, want 1", got)
	}
}

// 进程级并发上限是 fd 耗尽的真正兜底：单端口上限拦不住桥的总数
func TestGlobalMaxConnsRejectsExcess(t *testing.T) {
	setupServerTest(t)

	prev := config.Cfg.MaxConns
	config.Cfg.MaxConns = 1
	InitConnLimits()
	t.Cleanup(func() {
		config.Cfg.MaxConns = prev
		globalConnSem.Store(nil)
	})

	target := startHoldingTCPServer(t)
	bridgePort := freeTCPPort(t)
	if resp := performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort, Ip: target.host, Port: target.port,
	}); resp.Code != 200 {
		t.Fatalf("add failed: %+v", resp)
	}
	addr := net.JoinHostPort("127.0.0.1", formatPort(bridgePort))

	first, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	waitFor(t, 2*time.Second, func() bool { return bridgeConnCount(bridgePort) == 1 })

	second, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	second.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err = second.Read(buf)
	if err == nil {
		t.Fatal("connection over the global limit was not rejected")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("connection over the global limit was not rejected (read timed out)")
	}

	// 配额必须在连接结束时归还，否则一段时间后所有桥都会拒连
	first.Close()
	waitFor(t, 2*time.Second, func() bool { return globalConnInUse() == 0 })
}

type tcpTarget struct {
	host string
	port uint16
}
type fakeAcceptResult struct {
	conn net.Conn
	err  error
}

// fakeListener 按预设序列返回 Accept 结果，用于测试 Accept 循环的错误处理
type fakeListener struct {
	mu      sync.Mutex
	results []fakeAcceptResult
	calls   int
}

func (f *fakeListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.results) == 0 {
		return nil, net.ErrClosed
	}
	r := f.results[0]
	f.results = f.results[1:]
	return r.conn, r.err
}

func (f *fakeListener) Close() error   { return nil }
func (f *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

func (f *fakeListener) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// 缓存有记录但监听丢了（启动时 bind 失败的场景）：重复 add 必须补建监听，

// 短路逻辑绝不能只看缓存
func TestAddBridgeRepairsMissingListener(t *testing.T) {
	setupServerTest(t)

	target := startTCPServer(t, "first")
	bridgePort := freeTCPPort(t)
	bridge := dto.UseBridge{BridgePort: bridgePort, Ip: target.host, Port: target.port}

	if resp := performAddBridge(t, bridge); resp.Code != 200 {
		t.Fatalf("first add failed: %+v", resp)
	}

	// 只停监听、保留缓存记录，人为制造状态分叉
	if err := DelBridgeHandler(bridgePort); err != nil {
		t.Fatal(err)
	}
	if got := cacheEntryCount(bridgePort); got != 1 {
		t.Fatalf("cache entries = %d, want 1", got)
	}
	if got := bridgeListenerCount(bridgePort); got != 0 {
		t.Fatalf("listeners = %d, want 0", got)
	}

	if resp := performAddBridge(t, bridge); resp.Code != 200 {
		t.Fatalf("add should repair the missing listener, got %+v", resp)
	}
	assertConsistent(t, bridgePort)
	assertProxyResponse(t, bridgePort, "first")
}

func setupServerTest(t *testing.T) {

	t.Helper()
	gin.SetMode(gin.TestMode)
	resetListeners(t)
	t.Cleanup(func() {
		resetListeners(t)
	})
	config.Cfg.Key = testKey

	var err error
	cf, err = cachef.New(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)
}

// doBridgeRequest 不使用 t.Fatal，可以在测试的子 goroutine 里安全调用
func doBridgeRequest(path string, bridge dto.UseBridge, handler gin.HandlerFunc) (dto.Res, error) {
	body, err := json.Marshal(bridge)
	if err != nil {
		return dto.Res{}, err
	}
	encrypted, err := utils.AesEncryptCBCStr(string(body), testKey)
	if err != nil {
		return dto.Res{}, err
	}
	reqBody, err := json.Marshal(dto.Req{
		Ver:       "1",
		Timestamp: time.Now().Unix(),
		Data:      encrypted,
	})
	if err != nil {
		return dto.Res{}, err
	}

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	handler(c)

	var resp dto.Res
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return dto.Res{}, fmt.Errorf("response %q: %w", rec.Body.String(), err)
	}
	return resp, nil
}

func performAddBridge(t *testing.T, bridge dto.UseBridge) dto.Res {
	t.Helper()
	resp, err := doBridgeRequest("/bridge/add", bridge, AddBridge)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func performStartBridgeRequest(t *testing.T, remoteAddr string, bridgePort uint16) (dto.Res, dto.BridgeStartResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/bridge/start?bridgePort="+formatPort(bridgePort), nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	StartBridge(c)

	var resp dto.Res
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response %q: %v", rec.Body.String(), err)
	}
	var result dto.BridgeStartResult
	if resp.Data != nil {
		data, err := json.Marshal(resp.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
	}
	return resp, result
}

func bridgeListenerCount(port uint16) int {
	runMu.RLock()
	defer runMu.RUnlock()
	if _, ok := runListens[port]; ok {
		return 1
	}
	return 0
}

func bridgeListenerOf(port uint16) *bridgeListener {
	runMu.RLock()
	defer runMu.RUnlock()
	return runListens[port]
}

func bridgeConnCount(port uint16) int {
	l := bridgeListenerOf(port)
	if l == nil {
		return 0
	}
	return l.connCount()
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

func cacheEntryCount(port uint16) int {
	n := 0
	for _, b := range cf.All() {
		if b.Port == port {
			n++
		}
	}
	return n
}

// respListening 读取 AddBridge 响应里的 listening 标记。
// JSON 解码后 Data 是 map[string]any，布尔值原样保留。
func respListening(t *testing.T, resp dto.Res) bool {
	t.Helper()
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("response data = %#v, want map", resp.Data)
	}
	v, ok := data["listening"].(bool)
	if !ok {
		t.Fatalf("response data missing listening: %#v", data)
	}
	return v
}

// assertConsistent 断言持久化记录与实际监听没有分叉，这是 add/del 失败的根因
func assertConsistent(t *testing.T, port uint16) {
	t.Helper()
	cached := cacheEntryCount(port)
	listening := bridgeListenerCount(port)
	if cached > 1 {
		t.Fatalf("port %d has %d cache entries, want at most 1", port, cached)
	}
	if cached != listening {
		t.Fatalf("port %d state diverged: cache entries=%d, listeners=%d", port, cached, listening)
	}
}

func runWithTimeout(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("timed out after %s, possible deadlock", d)
	}
}

func startTCPServer(t *testing.T, response string) tcpTarget {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.Write([]byte(response))
			}()
		}
	}()
	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return tcpTarget{host: host, port: portOf(t, ln.Addr())}
}

// startHoldingTCPServer 的目标会一直保持连接不关闭，用于观察在途连接
func startHoldingTCPServer(t *testing.T) tcpTarget {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				<-stop
			}()
		}
	}()
	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return tcpTarget{host: host, port: portOf(t, ln.Addr())}
}

func portOf(t *testing.T, addr net.Addr) uint16 {

	t.Helper()
	_, portString, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := net.LookupPort("tcp", portString)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(port)
}

func freeTCPPort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return portOf(t, ln.Addr())
}

func assertProxyResponse(t *testing.T, port uint16, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", formatPort(port)), 100*time.Millisecond)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		buf := make([]byte, len(want))
		conn.SetReadDeadline(time.Now().Add(time.Second))
		_, err = conn.Read(buf)
		conn.Close()
		if err == nil && string(buf) == want {
			return
		}
		t.Fatalf("got %q, want %q, err=%v", string(buf), want, err)
	}
	t.Fatalf("proxy on port %d did not respond with %q", port, want)
}

func formatPort(port uint16) string {
	return strconv.FormatUint(uint64(port), 10)
}

func resetListeners(t *testing.T) {
	t.Helper()
	runMu.Lock()
	listeners := make([]*bridgeListener, 0, len(runListens))
	for port, listener := range runListens {
		delete(runListens, port)
		listeners = append(listeners, listener)
	}
	runMu.Unlock()
	for _, listener := range listeners {
		listener.stop()
		<-listener.done
	}
}
