//go:build e2e

// Package e2e 通过真实进程 + 真实 SOCKS5 代理 + 真实外网请求验证桥的行为。
//
// 默认不参与 go test ./...，需要显式打开 tag：
//
//	go test -tags e2e ./e2e/ -v                       # 测当前代码
//	go test -tags e2e ./e2e/ -v -run E2EStress        # 只跑压力用例
//	$env:BRIDGE_BIN="<old.exe>"; go test -tags e2e ./e2e/ -v   # 测另一个构建
//
// 拓扑：client --socks5--> bridgePort --tcp--> 本地 socks5 服务 --tcp--> myip.ipipv.com
// 桥只是字节管道，所以 SOCKS5 握手是端到端的，桥中途断链/丢监听都会直接表现为请求失败。
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baowk/bridge-direct/server/dto"
	"github.com/baowk/bridge-direct/utils"
)

const (
	testKey   = "abcd1234poiu5678bvbvnbnb" // 24 字节，AES-192
	targetURL = "http://myip.ipipv.com/"
	targetHos = "myip.ipipv.com"
)

func TestE2ESocks5ViaBridge(t *testing.T) {
	br := startBridge(t)
	socks := startSocks5(t)

	bridgePort := freePort(t)
	if res := br.add(t, bridgePort, socks.host, socks.port); res.Code != 200 {
		t.Fatalf("add bridge failed: %+v", res)
	}

	body, err := fetchViaBridge(bridgePort, 20*time.Second)
	if err != nil {
		t.Fatalf("request through bridge failed: %v", err)
	}
	if !bytes.Contains(body, []byte(`"Ip"`)) {
		t.Fatalf("unexpected body: %s", truncate(body, 300))
	}
	t.Logf("bridge -> socks5 -> %s ok: %s", targetHos, truncate(body, 200))

	if got := socks.connections.Load(); got != 1 {
		t.Fatalf("socks5 saw %d connections, want 1", got)
	}
}

// TestE2ESameBridgePortSwitchesProxy verifies that one bridge port can be
// retargeted between two local proxy endpoints without rebuilding its listener.
// The delete request intentionally carries the second proxy address to ensure
// deletion is keyed by bridgePort rather than by the target address.
func TestE2ESameBridgePortSwitchesProxy(t *testing.T) {
	br := startBridge(t)
	first := startSocks5(t)
	second := startSocks5(t)
	bridgePort := freePort(t)

	if first.port == second.port {
		t.Fatalf("test proxies unexpectedly share port %d", first.port)
	}

	if res := br.add(t, bridgePort, first.host, first.port); res.Code != 200 {
		t.Fatalf("add first proxy failed: %+v", res)
	}
	body, err := fetchViaBridge(bridgePort, 20*time.Second)
	if err != nil {
		t.Fatalf("request through first proxy failed: %v", err)
	}
	if !bytes.Contains(body, []byte(`"Ip"`)) {
		t.Fatalf("unexpected first proxy response: %s", truncate(body, 300))
	}
	if got := first.connections.Load(); got != 1 {
		t.Fatalf("first proxy saw %d connections after initial add, want 1", got)
	}
	if got := second.connections.Load(); got != 0 {
		t.Fatalf("second proxy saw %d connections before switch, want 0", got)
	}

	// Same bridgePort, different local proxy address: this must retarget the
	// existing listener so the port remains continuously available.
	if res := br.add(t, bridgePort, second.host, second.port); res.Code != 200 {
		t.Fatalf("switch to second proxy failed: %+v", res)
	}
	body, err = fetchViaBridge(bridgePort, 20*time.Second)
	if err != nil {
		t.Fatalf("request through switched proxy failed: %v", err)
	}
	if !bytes.Contains(body, []byte(`"Ip"`)) {
		t.Fatalf("unexpected switched proxy response: %s", truncate(body, 300))
	}
	if got := first.connections.Load(); got != 1 {
		t.Fatalf("first proxy saw %d connections after switch, want 1", got)
	}
	if got := second.connections.Load(); got != 1 {
		t.Fatalf("second proxy saw %d connections after switch, want 1", got)
	}

	// Del only needs bridgePort. Supply the second target deliberately to
	// prove a different proxy address does not prevent deletion.
	res, err := br.call("/bridge/del", dto.UseBridge{
		BridgePort: uint16(bridgePort),
		Ip:         second.host,
		Port:       uint16(second.port),
	})
	if err != nil {
		t.Fatalf("delete switched bridge failed: %v", err)
	}
	if res.Code != 200 {
		t.Fatalf("delete switched bridge returned: %+v", res)
	}
	if _, err := fetchViaBridge(bridgePort, 5*time.Second); err == nil {
		t.Fatal("request succeeded after bridge deletion")
	}
}

// TestE2EUnreachableRetargetKeepsBridgeListening repeatedly retargets one
// bridge port to local endpoints with no listener. Requests must fail because
// the backend proxy is unavailable, but the bridge port itself must continue
// accepting TCP connections. The final switch to a working proxy verifies that
// a failed backend does not poison the listener or prevent recovery.
func TestE2EUnreachableRetargetKeepsBridgeListening(t *testing.T) {
	retargets := envInt("E2E_BAD_RETARGETS", 30)
	br := startBridge(t)
	good := startSocks5(t)
	bridgePort := freePort(t)
	bridgeAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(bridgePort))

	if res := br.add(t, bridgePort, good.host, good.port); res.Code != 200 {
		t.Fatalf("add initial proxy failed: %+v", res)
	}

	badPorts := make([]int, retargets)
	for i := range badPorts {
		port := freePort(t)
		for port == bridgePort {
			port = freePort(t)
		}
		badPorts[i] = port
	}

	for i, badPort := range badPorts {
		res, err := br.callAdd(bridgePort, "127.0.0.1", badPort)
		if err != nil {
			t.Fatalf("retarget #%d/%d API call failed: %v", i+1, retargets, err)
		}
		if res.Code != 200 {
			t.Fatalf("retarget #%d/%d returned: %+v", i+1, retargets, res)
		}

		// A backend failure is expected, but a listener failure is not.
		conn, err := net.DialTimeout("tcp", bridgeAddr, time.Second)
		if err != nil {
			t.Fatalf("bridge port refused after retarget #%d/%d to bad proxy 127.0.0.1:%d: %v", i+1, retargets, badPort, err)
		}
		conn.Close()

		if _, err := fetchViaBridge(bridgePort, 3*time.Second); err == nil {
			t.Fatalf("request unexpectedly succeeded through unreachable proxy at retarget #%d/%d", i+1, retargets)
		} else if isRefused(err) {
			t.Fatalf("bridge listener was refused after retarget #%d/%d: %v", i+1, retargets, err)
		}
	}

	// Recovery check: changing back to a working backend must restore traffic
	// on the same listener after all failed backend attempts.
	if res := br.add(t, bridgePort, good.host, good.port); res.Code != 200 {
		t.Fatalf("restore working proxy failed: %+v", res)
	}
	body, err := fetchViaBridge(bridgePort, 20*time.Second)
	if err != nil {
		t.Fatalf("request after restoring working proxy failed: %v", err)
	}
	if !bytes.Contains(body, []byte(`"Ip"`)) {
		t.Fatalf("unexpected response after restoring working proxy: %s", truncate(body, 300))
	}

	if res, err := br.call("/bridge/del", dto.UseBridge{BridgePort: uint16(bridgePort)}); err != nil {
		t.Fatalf("delete after unreachable retargets failed: %v", err)
	} else if res.Code != 200 {
		t.Fatalf("delete after unreachable retargets returned: %+v", res)
	}
	if portStillAccepting(bridgeAddr, 3*time.Second) {
		t.Fatalf("bridge port %d still accepts connections after final delete", bridgePort)
	}
}

// TestE2EInfoLogsWrittenToFileWithoutConsole verifies that INFO lifecycle and
// sync logs are persisted when console output is disabled.
func TestE2EInfoLogsWrittenToFileWithoutConsole(t *testing.T) {
	syncPort := freePort(t)
	targetPort := freePort(t)
	targetAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort))
	syncBody := fmt.Sprintf(`{"code":200,"msg":"ok","data":[{"port":%d,"proxyAddr":"%s"}]}`,
		syncPort, targetAddr)
	logFile := filepath.Join(t.TempDir(), "bridge.log")
	br := startBridgeWithOptions(t, logFile, false, syncBody)

	// The synchronized bridge is already present. Re-adding it exercises the
	// management INFO logs without changing the synchronized target.
	if res := br.add(t, syncPort, "127.0.0.1", targetPort); res.Code != 200 {
		t.Fatalf("add synchronized bridge failed: %+v", res)
	}
	if res, err := br.call("/bridge/del", dto.UseBridge{BridgePort: uint16(syncPort)}); err != nil {
		t.Fatalf("delete synchronized bridge failed: %v", err)
	} else if res.Code != 200 {
		t.Fatalf("delete synchronized bridge returned: %+v", res)
	}

	deadline := time.Now().Add(2 * time.Second)
	var content []byte
	for time.Now().Before(deadline) {
		content, _ = os.ReadFile(logFile)
		text := string(content)
		if strings.Contains(text, "syncBridge ok") &&
			(strings.Contains(text, "AddBridge ok") || strings.Contains(text, "AddBridge unchanged")) &&
			strings.Contains(text, "DelBridge ok") &&
			strings.Contains(text, "management request") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	text := string(content)
	for _, event := range []string{"syncBridge ok", "DelBridge ok", "management request"} {
		if !strings.Contains(text, event) {
			t.Errorf("log file %s is missing INFO event %q; content:\n%s", logFile, event, text)
		}
	}
	if !strings.Contains(text, "AddBridge ok") && !strings.Contains(text, "AddBridge unchanged") {
		t.Errorf("log file %s is missing an AddBridge INFO event; content:\n%s", logFile, text)
	}
	if stdout := br.logs.String(); strings.Contains(stdout, "AddBridge") || strings.Contains(stdout, "DelBridge") || strings.Contains(stdout, "syncBridge") {
		t.Fatalf("INFO lifecycle logs leaked to console despite logConsole=false:\n%s", stdout)
	}
}

// TestE2EStressSocks5ViaBridge 在持续流量下反复重推同一个桥。
//
// 这是生产事故的最小复现：中心侧会周期性重推桥配置，如果重推会重建监听，
// 就存在「端口未监听」窗口和重新 bind 失败的风险，表现为客户端 connection refused
// 或者桥直接消失。
func TestE2EStressSocks5ViaBridge(t *testing.T) {
	requests := envInt("E2E_REQUESTS", 40)
	concurrency := envInt("E2E_CONCURRENCY", 8)
	churnRounds := envInt("E2E_CHURN", 8)

	br := startBridge(t)
	socks := startSocks5(t)
	bridgePort := freePort(t)
	if res := br.add(t, bridgePort, socks.host, socks.port); res.Code != 200 {
		t.Fatalf("add bridge failed: %+v", res)
	}
	// 先确认链路是通的，避免把环境问题算成桥的问题
	if _, err := fetchViaBridge(bridgePort, 20*time.Second); err != nil {
		t.Fatalf("warmup request failed: %v", err)
	}

	var (
		reqOK      atomic.Int64
		reqRefused atomic.Int64 // 拨不通桥端口 = 监听丢了或处于未监听窗口
		reqBroken  atomic.Int64 // 连上了但中途断/超时
		apiFail    atomic.Int64
	)
	var mu sync.Mutex
	var samples []string
	record := func(kind string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if len(samples) < 10 {
			samples = append(samples, kind+": "+err.Error())
		}
	}

	stop := make(chan struct{})
	var churnWG sync.WaitGroup
	churnWG.Add(1)
	go func() {
		defer churnWG.Done()
		for i := 0; i < churnRounds; i++ {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
			}
			// 重推同一个桥：目标不变，只是中心侧的周期性下发
			res, err := br.callAdd(bridgePort, socks.host, socks.port)
			if err != nil {
				apiFail.Add(1)
				record("api", err)
				continue
			}
			if res.Code != 200 {
				apiFail.Add(1)
				record("api", fmt.Errorf("code=%d msg=%s data=%v", res.Code, res.Msg, res.Data))
			}
		}
	}()

	var pending atomic.Int64
	pending.Store(int64(requests))
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pending.Add(-1) >= 0 {
				_, err := fetchViaBridge(bridgePort, 25*time.Second)
				switch {
				case err == nil:
					reqOK.Add(1)
				case isRefused(err):
					reqRefused.Add(1)
					record("refused", err)
				default:
					reqBroken.Add(1)
					record("broken", err)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	churnWG.Wait()

	// 压测后桥必须还在服务：这是「监听成功后不再接受新连接」的检测点
	finalErr := error(nil)
	if _, err := fetchViaBridge(bridgePort, 20*time.Second); err != nil {
		finalErr = err
	}

	t.Logf("requests=%d concurrency=%d churn=%d => ok=%d refused=%d broken=%d apiFail=%d socksConns=%d",
		requests, concurrency, churnRounds,
		reqOK.Load(), reqRefused.Load(), reqBroken.Load(), apiFail.Load(), socks.connections.Load())
	for _, s := range samples {
		t.Logf("  sample %s", s)
	}

	if n := reqRefused.Load(); n > 0 {
		t.Errorf("%d requests could not reach the bridge port: listener was missing during churn", n)
	}
	if n := reqBroken.Load(); n > 0 {
		t.Errorf("%d in-flight requests were broken during churn", n)
	}
	if n := apiFail.Load(); n > 0 {
		t.Errorf("%d /bridge/add calls failed while re-pushing an unchanged bridge", n)
	}
	if finalErr != nil {
		t.Errorf("bridge stopped serving after the stress phase: %v", finalErr)
	}
}

// TestE2EChurnKeepsPortListening 用并发重推 + 紧密拨号命中「端口未监听」窗口。
//
// 中心侧重推是并发的，而重建监听的实现里「关掉旧 listener」和「bind 新 listener」
// 之间存在窗口，窗口内到达的连接会被 RST；并发重推还会互相撞上
// 「port N already listening」。这两个都是亚毫秒级事件，必须高频拨号才能测出来，
// 所以这里不走外网，只做 TCP 连接建立。
func TestE2EChurnKeepsPortListening(t *testing.T) {
	dialers := envInt("E2E_DIALERS", 4)
	churners := envInt("E2E_CHURNERS", 4)
	duration := time.Duration(envInt("E2E_CHURN_SECONDS", 6)) * time.Second

	br := startBridge(t)
	socks := startSocks5(t)
	bridgePort := freePort(t)
	if res := br.add(t, bridgePort, socks.host, socks.port); res.Code != 200 {
		t.Fatalf("add bridge failed: %+v", res)
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(bridgePort))
	var (
		dialOK      atomic.Int64
		dialRefused atomic.Int64
		dialOther   atomic.Int64
		addOK       atomic.Int64
		addFail     atomic.Int64
	)
	var mu sync.Mutex
	var samples []string
	record := func(kind, msg string) {
		mu.Lock()
		defer mu.Unlock()
		if len(samples) < 12 {
			samples = append(samples, kind+": "+msg)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < dialers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
				if err != nil {
					if isRefused(err) {
						dialRefused.Add(1)
						record("refused", err.Error())
					} else {
						dialOther.Add(1)
						record("dial", err.Error())
					}
					continue
				}
				// 完整走一次 SOCKS5 握手到本地 socks5，确认桥真的在转发
				if err := socks5Handshake(conn, net.JoinHostPort("127.0.0.1", strconv.Itoa(socks.port))); err != nil {
					dialOther.Add(1)
					record("handshake", err.Error())
				} else {
					dialOK.Add(1)
				}
				conn.Close()
			}
		}()
	}

	for i := 0; i < churners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, err := br.callAdd(bridgePort, socks.host, socks.port)
				if err != nil {
					addFail.Add(1)
					record("api", err.Error())
					continue
				}
				if res.Code != 200 {
					addFail.Add(1)
					record("api", fmt.Sprintf("code=%d msg=%s", res.Code, res.Msg))
					continue
				}
				addOK.Add(1)
			}
		}()
	}

	time.Sleep(duration)
	close(stop)
	wg.Wait()

	// 收敛检查：churn 结束后端口必须还在监听，且能正常转发
	var finalErr error
	if _, err := fetchViaBridge(bridgePort, 20*time.Second); err != nil {
		finalErr = err
	}

	t.Logf("dialers=%d churners=%d duration=%s => dialOK=%d refused=%d otherDialErr=%d addOK=%d addFail=%d",
		dialers, churners, duration, dialOK.Load(), dialRefused.Load(),
		dialOther.Load(), addOK.Load(), addFail.Load())
	for _, s := range samples {
		t.Logf("  sample %s", s)
	}

	// churn 之后删桥：端口必须真的不再接受连接。
	// 并发重建监听时，「删掉 map 记录」和「关掉 listener」可能作用在不同对象上，
	// 留下一个没人引用却还在 accept 的孤儿监听——记录已删、流量仍在转发，
	// 同时泄漏一个 socket 和一个 accept 协程。
	if res, err := br.call("/bridge/del", dto.UseBridge{BridgePort: uint16(bridgePort)}); err != nil {
		t.Errorf("del after churn: %v", err)
	} else if res.Code != 200 {
		t.Errorf("del after churn failed: %+v", res)
	}
	if orphan := portStillAccepting(addr, 3*time.Second); orphan {
		t.Errorf("port %d still accepts connections after del: orphaned listener leaked", bridgePort)
	}
	t.Logf("post-churn process: %s", br.snapshot(t))

	if n := dialRefused.Load(); n > 0 {
		t.Errorf("%d connections were refused: re-pushing a bridge left the port unlistened", n)
	}
	if n := dialOther.Load(); n > 0 {
		t.Errorf("%d connections failed mid-handshake during churn", n)
	}
	if n := addFail.Load(); n > 0 {
		t.Errorf("%d concurrent /bridge/add calls failed on an unchanged bridge", n)
	}
	if finalErr != nil {
		t.Errorf("bridge stopped serving after churn: %v", finalErr)
	}
}

// TestE2EResourceLeakUnderChurn 反复「建桥 → 打流量 → 删桥」，然后比对资源水位。
//
// 看两类泄漏：
//   - goroutine：每个桥有 accept 协程、每条连接有转发协程，任何一处不退出都会
//     随时间单调增长，最终表现为「监听还在但不再接受新连接」。经 pprof 读取。
//   - 内核对象：Windows 上是 handle/线程数，对应 Linux 的 fd。fd 耗尽会让
//     accept 持续返回 EMFILE，这是生产上最可能的死法。经进程指标读取，
//     旧构建没有 pprof 也能测。
func TestE2EResourceLeakUnderChurn(t *testing.T) {
	cycles := envInt("E2E_LEAK_CYCLES", 25)
	connsPerCycle := envInt("E2E_LEAK_CONNS", 20)

	br := startBridge(t)
	socks := startSocks5(t)

	// 泄漏检测不打外网：需要足够多的短连接，且不能被外部服务的限流干扰
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(target.Close)
	targetAddr := strings.TrimPrefix(target.URL, "http://")

	bridgePort := freePort(t)
	// 预热一轮，把一次性初始化（连接池、日志、gin）排除在基线之外
	runLeakCycle(t, br, socks, bridgePort, targetAddr, 5)

	base := br.snapshot(t)
	t.Logf("baseline: %s", base)

	for i := 0; i < cycles; i++ {
		runLeakCycle(t, br, socks, bridgePort, targetAddr, connsPerCycle)
	}

	// 等在途连接收敛：TCP 关闭是异步的，立刻采样会把正常的收尾算成泄漏
	time.Sleep(3 * time.Second)
	after := br.snapshot(t)
	t.Logf("after %d cycles x %d conns: %s", cycles, connsPerCycle, after)

	if base.goroutines > 0 && after.goroutines > 0 {
		delta := after.goroutines - base.goroutines
		t.Logf("goroutine delta = %+d", delta)
		if delta > 20 {
			t.Errorf("goroutine leak: %d -> %d (+%d) after %d bridge create/destroy cycles",
				base.goroutines, after.goroutines, delta, cycles)
			if dump := br.pprofText("/debug/pprof/goroutine?debug=1"); dump != "" {
				t.Logf("---- goroutine profile ----\n%s", truncate([]byte(dump), 4000))
			}
		}
	} else {
		t.Logf("pprof unavailable on this build, goroutine leak check skipped")
	}

	if base.handles > 0 && after.handles > 0 {
		delta := after.handles - base.handles
		t.Logf("kernel handle delta = %+d (threads %+d)", delta, after.threads-base.threads)
		// 阈值放宽：连接关闭后句柄回收有延迟，只抓住单调增长的量级
		if delta > 200 {
			t.Errorf("kernel handle leak: %d -> %d (+%d)", base.handles, after.handles, delta)
		}
		if d := after.threads - base.threads; d > 32 {
			t.Errorf("thread count grew by %d, runtime threads are not being reused", d)
		}
	}

	if base.heapAlloc > 0 && after.heapAlloc > 0 {
		grow := after.heapAlloc - base.heapAlloc
		t.Logf("heapAlloc delta = %+d bytes (objects %+d)", grow, after.heapObjects-base.heapObjects)
		if grow > 32<<20 {
			t.Errorf("heap grew by %d bytes after churn, suspect retained buffers/conns", grow)
		}
	}

	// 泄漏跑完后桥必须还能正常服务
	if res := br.add(t, bridgePort, socks.host, socks.port); res.Code != 200 {
		t.Fatalf("re-add after leak test failed: %+v", res)
	}
	if _, err := fetchThroughBridge(bridgePort, targetAddr, "http://"+targetAddr+"/", 10*time.Second); err != nil {
		t.Errorf("bridge stopped serving after leak test: %v", err)
	}
}

func runLeakCycle(t *testing.T, br *bridgeProc, socks *socks5Server, bridgePort int, targetAddr string, conns int) {
	t.Helper()
	if res := br.add(t, bridgePort, socks.host, socks.port); res.Code != 200 {
		t.Fatalf("add bridge failed: %+v", res)
	}
	var wg sync.WaitGroup
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 失败不判错：这里只关心资源水位，正确性由其它用例覆盖
			_, _ = fetchThroughBridge(bridgePort, targetAddr, "http://"+targetAddr+"/", 10*time.Second)
		}()
	}
	wg.Wait()
	if _, err := br.call("/bridge/del", dto.UseBridge{BridgePort: uint16(bridgePort)}); err != nil {
		t.Fatalf("del bridge failed: %v", err)
	}
}

// ---------- 资源水位采样 ----------

type resSnapshot struct {
	goroutines  int
	heapAlloc   int64
	heapObjects int64
	handles     int
	threads     int
}

func (s resSnapshot) String() string {
	return fmt.Sprintf("goroutines=%d heapAlloc=%d heapObjects=%d handles=%d threads=%d",
		s.goroutines, s.heapAlloc, s.heapObjects, s.handles, s.threads)
}

func (b *bridgeProc) snapshot(t *testing.T) resSnapshot {
	t.Helper()
	var s resSnapshot
	if txt := b.pprofText("/debug/pprof/goroutine?debug=1"); txt != "" {
		s.goroutines = parseGoroutineTotal(txt)
	}
	// gc=1 让 runtime 先跑一次 GC，否则未回收的垃圾会被当成增长
	if txt := b.pprofText("/debug/pprof/heap?debug=1&gc=1"); txt != "" {
		s.heapAlloc = parseMemStat(txt, "HeapAlloc")
		s.heapObjects = parseMemStat(txt, "HeapObjects")
	}
	s.handles, s.threads = procMetrics(b.pid)
	return s
}

func (b *bridgeProc) pprofText(path string) string {
	if b.pprofAddr == "" {
		return ""
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get("http://" + b.pprofAddr + path)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return string(raw)
}

func parseGoroutineTotal(txt string) int {
	// 首行形如：goroutine profile: total 42
	line := txt
	if i := strings.IndexByte(txt, '\n'); i > 0 {
		line = txt[:i]
	}
	i := strings.LastIndex(line, "total ")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[i+len("total "):]))
	if err != nil {
		return 0
	}
	return n
}

func parseMemStat(txt, key string) int64 {
	// heap?debug=1 尾部是 MemStats 转储：# HeapAlloc = 1234
	marker := "# " + key + " = "
	i := strings.Index(txt, marker)
	if i < 0 {
		return 0
	}
	rest := txt[i+len(marker):]
	if j := strings.IndexAny(rest, "\r\n"); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// procMetrics 读进程的内核对象水位。Windows 用 handle/线程数，
// 对应 Linux 上的 /proc/<pid>/fd 与 task 数；拿不到就返回 0 让调用方跳过。
func procMetrics(pid int) (handles, threads int) {
	if pid <= 0 {
		return 0, 0
	}
	script := fmt.Sprintf(
		"$p = Get-Process -Id %d -ErrorAction SilentlyContinue; if ($p) { \"$($p.HandleCount) $($p.Threads.Count)\" }", pid)
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0
	}
	handles, _ = strconv.Atoi(fields[0])
	threads, _ = strconv.Atoi(fields[1])
	return handles, threads
}

// ---------- 被测进程 ----------

type bridgeProc struct {
	adminAddr string
	pprofAddr string
	pid       int
	logs      *syncBuffer
}

func startBridge(t *testing.T) *bridgeProc {
	return startBridgeWithOptions(t, "", false, `{"code":200,"msg":"ok","data":[]}`)
}

func startBridgeWithOptions(t *testing.T, logFile string, logConsole bool, syncBody string) *bridgeProc {
	t.Helper()

	bin := os.Getenv("BRIDGE_BIN")
	if bin == "" {
		bin = buildCurrent(t)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("BRIDGE_BIN %q: %v", bin, err)
	}

	// 桩掉中心侧：mode 必须非 local 才会起管理接口，而非 local 会先同步桥列表
	sync := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, syncBody)
	}))
	t.Cleanup(sync.Close)

	dir := t.TempDir()
	adminPort := freePort(t)
	pprofPort := freePort(t)
	cfg := map[string]any{
		"mode":         "remote",
		"addr":         fmt.Sprintf("127.0.0.1:%d", adminPort),
		"logFile":      logFile,
		"logConsole":   logConsole,
		"logLevel":     "info",
		"logFormat":    "text",
		"logSource":    false,
		"syncDomain":   sync.URL,
		"key":          testKey,
		"dataFilename": "bridge.db",
		"bridgeId":     1,
		// 旧构建没有这个配置项，viper 会忽略，此时 pprof 相关断言自动跳过
		"pprofAddr": fmt.Sprintf("127.0.0.1:%d", pprofPort),
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuffer{}
	cmd := exec.Command(bin, "-c", "config.json")
	cmd.Dir = dir
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}

	b := &bridgeProc{
		adminAddr: fmt.Sprintf("127.0.0.1:%d", adminPort),
		pprofAddr: fmt.Sprintf("127.0.0.1:%d", pprofPort),
		pid:       cmd.Process.Pid,
		logs:      logs,
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if t.Failed() {
			t.Logf("---- bridge log (%s) ----\n%s", bin, logs.String())
		}
	})

	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", b.adminAddr, time.Second)
		if err == nil {
			conn.Close()
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("management api %s did not come up: %v\n%s", b.adminAddr, err, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func buildCurrent(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "bridge-direct.exe")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = ".."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

func (b *bridgeProc) add(t *testing.T, bridgePort int, ip string, port int) dto.Res {
	t.Helper()
	res, err := b.callAdd(bridgePort, ip, port)
	if err != nil {
		t.Fatalf("POST /bridge/add: %v", err)
	}
	return res
}

func (b *bridgeProc) callAdd(bridgePort int, ip string, port int) (dto.Res, error) {
	return b.call("/bridge/add", dto.UseBridge{
		BridgePort: uint16(bridgePort), Ip: ip, Port: uint16(port),
	})
}

func (b *bridgeProc) call(path string, payload dto.UseBridge) (dto.Res, error) {
	var res dto.Res
	plain, err := json.Marshal(payload)
	if err != nil {
		return res, err
	}
	enc, err := utils.AesEncryptCBC(plain, []byte(testKey))
	if err != nil {
		return res, err
	}
	body, err := json.Marshal(dto.Req{
		Ver:       "1",
		Timestamp: time.Now().Unix(),
		Data:      base64.StdEncoding.EncodeToString(enc),
	})
	if err != nil {
		return res, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+b.adminAddr+path, bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return res, err
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return res, fmt.Errorf("decode %q: %w", truncate(raw, 200), err)
	}
	return res, nil
}

// ---------- 最小 SOCKS5 服务 ----------

type socks5Server struct {
	host        string
	port        int
	connections atomic.Int64
}

// startSocks5 起一个只支持无认证 CONNECT 的 SOCKS5 服务，作为桥的转发目标。
// 用它而不是普通 HTTP 服务，是因为生产流量就是浏览器走 SOCKS5：
// 握手在客户端和代理之间端到端完成，桥中途做任何断链都会被立刻暴露。
func startSocks5(t *testing.T) *socks5Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	s := &socks5Server{host: host, port: port}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.connections.Add(1)
			go serveSocks5(conn)
		}
	}()
	return s
}

func serveSocks5(client net.Conn) {
	defer client.Close()
	client.SetDeadline(time.Now().Add(60 * time.Second))

	br := bufio.NewReader(client)
	// greeting: ver | nmethods | methods...
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil || head[0] != 0x05 {
		return
	}
	if _, err := io.CopyN(io.Discard, br, int64(head[1])); err != nil {
		return
	}
	if _, err := client.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// request: ver | cmd | rsv | atyp | addr | port
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil || req[1] != 0x01 {
		client.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	case 0x03:
		n := make([]byte, 1)
		if _, err := io.ReadFull(br, n); err != nil {
			return
		}
		buf := make([]byte, int(n[0]))
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		host = string(buf)
	default:
		client.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(br, pb); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))))

	upstream, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		client.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	client.SetDeadline(time.Time{})
	upstream.SetDeadline(time.Time{})

	go func() {
		io.Copy(upstream, br)
		upstream.Close()
	}()
	io.Copy(client, upstream)
}

// ---------- 客户端 ----------

// fetchViaBridge 把桥端口当成 SOCKS5 代理入口发一次外网 HTTP 请求。
func fetchViaBridge(bridgePort int, timeout time.Duration) ([]byte, error) {
	return fetchThroughBridge(bridgePort, targetHos+":80", targetURL, timeout)
}

// fetchThroughBridge 与 fetchViaBridge 相同，但目标可指定，便于用本地服务做高频压测。
func fetchThroughBridge(bridgePort int, socksTarget, url string, timeout time.Duration) ([]byte, error) {
	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(bridgePort)))
			if err != nil {
				return nil, err
			}
			if err := socks5Handshake(conn, socksTarget); err != nil {
				conn.Close()
				return nil, err
			}
			return conn, nil
		},
	}
	client := &http.Client{Transport: tr, Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("status %d", resp.StatusCode)
	}
	return body, nil
}

func socks5Handshake(conn net.Conn, addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5 greeting: %w", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("socks5 greeting rejected: %v", resp)
	}

	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("socks5 connect: %w", err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5 connect rejected: rep=%d", head[1])
	}
	switch head[3] {
	case 0x01:
		if _, err := io.CopyN(io.Discard, conn, 4+2); err != nil {
			return err
		}
	case 0x03:
		n := make([]byte, 1)
		if _, err := io.ReadFull(conn, n); err != nil {
			return err
		}
		if _, err := io.CopyN(io.Discard, conn, int64(n[0])+2); err != nil {
			return err
		}
	case 0x04:
		if _, err := io.CopyN(io.Discard, conn, 16+2); err != nil {
			return err
		}
	}
	return conn.SetDeadline(time.Time{})
}

// ---------- 杂项 ----------

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// portStillAccepting 判断端口是否仍能建立连接。删桥后应当在很短时间内变为不可连。
func portStillAccepting(addr string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

func isRefused(err error) bool {
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "actively refused") ||
		strings.Contains(s, "No connection could be made")
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
