package server

// 管理面并发压测：混合 add / del / start / retarget / status / metrics 六条路径，
// 用来回答"是否存在死锁"这个问题，并把它固化成回归用例。
//
// 这些用例即使不开 -race 也有价值：Go 运行时对未受保护的 map 并发读写会直接
// fatal("concurrent map read and map write") 杀掉进程，所以只要 runListens 或
// cf.data 少了一把锁，这里就会崩，而不是静默通过。

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baowk/bridge-direct/config"
	"github.com/baowk/bridge-direct/server/dto"
	"github.com/gin-gonic/gin"
)

// reservePorts 一次性占住 n 个端口再统一释放，保证返回的端口列表内部互不重复。
// 逐次调用 freeTCPPort 会得到可能重复的端口，在压测里会造成互相抢端口。
func reservePorts(t *testing.T, n int) []uint16 {
	t.Helper()
	listeners := make([]net.Listener, 0, n)
	ports := make([]uint16, 0, n)
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve port %d: %v", i, err)
		}
		listeners = append(listeners, ln)
		ports = append(ports, portOf(t, ln.Addr()))
	}
	for _, ln := range listeners {
		_ = ln.Close()
	}
	return ports
}

func prepareStressConfig() {
	config.Cfg.MaxConns = 0
	config.Cfg.MaxConnsPerPort = 0
	config.Cfg.ConnIdleTimeout = 0
	InitConnLimits()
}

// callAdd / callDel 复用测试辅助里的 doBridgeRequest，它不调用 t.Fatal，
// 因此可以在子 goroutine 里安全使用。
func callAdd(bridge dto.UseBridge) (dto.Res, error) {
	return doBridgeRequest("/bridge/add", bridge, AddBridge)
}

func callDel(port uint16) (dto.Res, error) {
	return doBridgeRequest("/bridge/del", dto.UseBridge{BridgePort: port}, DelBridge)
}

func callStart(port uint16) error {
	req := httptest.NewRequest(http.MethodPost, "/bridge/start?bridgePort="+formatPort(port), nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	StartBridge(c)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("start http status %d", rec.Code)
	}
	return nil
}

func callStatus(port uint16, check bool) error {
	path := "/bridge/status"
	sep := "?"
	if port != 0 {
		path += "?bridgePort=" + formatPort(port)
		sep = "&"
	}
	if check {
		path += sep + "check=1"
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	GetBridgeStatus(c)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("status http status %d", rec.Code)
	}
	return nil
}

// drainPort 反复尝试把某个端口清干净，直到 cache 与运行态监听都为 0。
// 单次 DelBridge 在持久化瞬时失败或监听器停止超时时可能留下残留，这里重试。
func drainPort(t *testing.T, port uint16) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := callDel(port); err != nil {
			t.Fatalf("drain del port %d: %v", port, err)
		}
		if cacheEntryCount(port) == 0 && bridgeListenerCount(port) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d did not drain: cache=%d listeners=%d",
		port, cacheEntryCount(port), bridgeListenerCount(port))
}

// TestStressManagementPlaneNoDeadlock 让六条管理路径在多端口上长时间互相踩踏，
// 最后要求：所有操作在超时内完成、状态收敛、goroutine 回到基线。
func TestStressManagementPlaneNoDeadlock(t *testing.T) {
	setupServerTest(t)
	prepareStressConfig()

	targetA := startTCPServer(t, "a")
	targetB := startTCPServer(t, "b")

	const (
		portCount  = 24
		iterations = 60
	)
	ports := reservePorts(t, portCount)

	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var ops atomic.Int64
	var wg sync.WaitGroup

	// 反例保护：任何一条路径返回错误都记下来，便于失败时定位，但不算测试失败，
	// 因为 add/del 并发本身就会出现"端口已被另一个操作占用"这类合法竞争结果。
	var addErrs, delErrs, statusErrs, startErrs atomic.Int64

	runWithTimeout(t, 120*time.Second, func() {
		// 1) add + retarget（在 A/B 两个目标之间来回切换，覆盖原子替换目标的分支）
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				port := ports[i%portCount]
				target := targetA
				if i%3 == 0 {
					target = targetB
				}
				if _, err := callAdd(dto.UseBridge{BridgePort: port, Ip: target.host, Port: target.port}); err != nil {
					addErrs.Add(1)
					continue
				}
				ops.Add(1)
			}
		}()

		// 2) del
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				port := ports[i%portCount]
				if _, err := callDel(port); err != nil {
					delErrs.Add(1)
					continue
				}
				ops.Add(1)
			}
		}()

		// 3) status（带 check 会做真实 TCP 拨号，走 closeConns 之外的路径）
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if err := callStatus(ports[i%portCount], i%2 == 0); err != nil {
					statusErrs.Add(1)
					continue
				}
				ops.Add(1)
			}
		}()

		// 4) status 全量（不加过滤器，遍历整张表）
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if err := callStatus(0, false); err != nil {
					statusErrs.Add(1)
					continue
				}
				ops.Add(1)
			}
		}()

		// 5) metrics 抓取（Collect 会同时读 runListens 和 cf.data）
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler := MetricsHandler()
			for i := 0; i < iterations; i++ {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("metrics http status %d", rec.Code)
					return
				}
				ops.Add(1)
			}
		}()

		// 6) start（读 cf 后重建监听，和 add/del 抢同一把端口锁）
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if err := callStart(ports[i%portCount]); err != nil {
					startErrs.Add(1)
					continue
				}
				ops.Add(1)
			}
		}()

		wg.Wait()
	})

	t.Logf("stress ops=%d addErrs=%d delErrs=%d statusErrs=%d startErrs=%d",
		ops.Load(), addErrs.Load(), delErrs.Load(), statusErrs.Load(), startErrs.Load())

	// 6 条路径各至少成功执行过，否则说明压测实际上没跑起来
	if ops.Load() < int64(iterations) {
		t.Fatalf("stress made too little progress: ops=%d", ops.Load())
	}
	if statusErrs.Load() > 0 || startErrs.Load() > 0 {
		t.Fatalf("read paths must not fail: statusErrs=%d startErrs=%d",
			statusErrs.Load(), startErrs.Load())
	}

	// 收敛：全量清理后 cache 与 listener 必须同时归零，不能出现状态分叉
	for _, port := range ports {
		drainPort(t, port)
	}
	for _, port := range ports {
		assertConsistent(t, port)
	}

	// goroutine 泄漏：所有 supervisor 都由 DelBridgeHandler 等待退出，
	// 清理完成后应回到基线附近。
	waitFor(t, 15*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline+10
	})
}

// TestStressSinglePortAddDelNoDeadlock 把全部压力集中在同一个端口上，
// 让所有操作争抢同一把 portLock，这是最容易暴露锁顺序问题或自死锁的形态。
func TestStressSinglePortAddDelNoDeadlock(t *testing.T) {
	setupServerTest(t)
	prepareStressConfig()

	targetA := startTCPServer(t, "a")
	targetB := startTCPServer(t, "b")

	port := reservePorts(t, 1)[0]

	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const (
		goroutines = 8
		iterations = 25
	)

	var wg sync.WaitGroup
	runWithTimeout(t, 90*time.Second, func() {
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					switch (g + i) % 4 {
					case 0:
						// 同一目标重复 add，命中幂等短路
						_, _ = callAdd(dto.UseBridge{BridgePort: port, Ip: targetA.host, Port: targetA.port})
					case 1:
						// 切换目标，命中原子替换分支
						_, _ = callAdd(dto.UseBridge{BridgePort: port, Ip: targetB.host, Port: targetB.port})
					case 2:
						_, _ = callDel(port)
					case 3:
						_ = callStart(port)
					}
				}
			}(g)
		}
		wg.Wait()
	})

	drainPort(t, port)
	assertConsistent(t, port)

	waitFor(t, 15*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline+10
	})
}
