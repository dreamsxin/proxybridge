package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof" // 仅在配置了 pprofAddr 时才对外提供
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/baowk/bridge-direct/cachef"
	"github.com/baowk/bridge-direct/config"
	"github.com/baowk/bridge-direct/server/dto"
	"github.com/baowk/bridge-direct/utils"
	"github.com/gin-gonic/gin"
)


var cf *cachef.CacheF

// listenReadyTimeout 是 AddBridge 等待 bind 结果的上限。
// 正常情况下 bind 是微秒级的；这里只为把「端口被占用、正在退避重试」这种情况
// 在响应里体现出来，不能设太长，否则会拖住管理接口。
const listenReadyTimeout = 500 * time.Millisecond

// portLocks 按端口串行化管理接口。cf（持久化映射表）和 runListens（实际监听）是
// 两份独立状态，同一端口上并发的 add/del 会在「查缓存」和「改监听」之间产生分叉，
// 表现为 "port N already listening" 或删除后监听仍在。
//
// 用端口粒度而不是全局一把锁：这两份状态的一致性约束只存在于同一端口内部，
// 跨端口没有共享不变量（数据文件由 cachef 自己的锁保护）。全局锁会让一次慢操作
// —— 比如 DelBridgeHandler 最坏等 stopTimeout=10s，或者一次 fsync 卡住 ——
// 把其它端口的管理请求全部堵在后面（队头阻塞）。
var portLocks sync.Map // uint16 -> *sync.Mutex

// lockPort 返回解锁函数。条目不回收：端口数上限 65535，常驻内存可忽略，
// 而回收会引入「删除瞬间另一个请求正持有该锁」的竞态。
func lockPort(port uint16) func() {
	value, _ := portLocks.LoadOrStore(port, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func Start() {
	var err error
	if cf, err = cachef.New(config.Cfg.DataFilename); err != nil {
		panic(err)
	}

	startPprof(config.Cfg.PprofAddr)
	startStatsLogger(config.Cfg.StatsInterval)
	// 必须在起任何桥之前设置全局配额
	InitConnLimits()


	if config.Cfg.Mode != config.MODE_LOCAL {
		//同步桥
		if err := syncBridge(); err != nil {
			slog.Error("syncBridge", "syncDomain", config.Cfg.SyncDomain,
				"bridgeId", config.Cfg.BridgeId, "err", err)
		}
	} else {
		slog.Info("local mode: skip syncBridge and management api", "mode", config.Cfg.Mode)
	}

	InitBridgeHandler()

	if config.Cfg.Mode != config.MODE_LOCAL {
		gin.SetMode(gin.ReleaseMode)
		r := gin.Default()
		r.POST("/bridge/add", AddBridge)
		r.POST("/bridge/del", DelBridge)
		if config.Cfg.Addr == "" {
			config.Cfg.Addr = ":8080"
		}
		slog.Info("management server listening", "addr", config.Cfg.Addr)
		if err := r.Run(config.Cfg.Addr); err != nil {
			slog.Error("management server exited", "addr", config.Cfg.Addr, "err", err)
		}
	}
}

// startPprof 按需开启 pprof。空地址=关闭。
// pprof 没有任何鉴权，暴露出去等于把进程内存和调用栈公开，只能绑回环或内网。
func startPprof(addr string) {
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		slog.Error("pprof addr invalid", "addr", addr, "err", err)
		return
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		slog.Warn("pprof is NOT authenticated, binding to a non-loopback address exposes heap and stacks",
			"addr", addr)
	}

	go func() {
		slog.Info("pprof listening", "addr", addr,
			"hint", "goroutine: /debug/pprof/goroutine?debug=2, heap: /debug/pprof/heap")
		srv := &http.Server{
			Addr:              addr,
			Handler:           http.DefaultServeMux, // net/http/pprof 注册在 DefaultServeMux
			ReadHeaderTimeout: 10 * time.Second,
		}
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("pprof server exited", "addr", addr, "err", err)
		}
	}()
}

// startStatsLogger 定期输出运行水位，用于判断 goroutine/连接/内存是否在单调增长
func startStatsLogger(intervalSec int) {
	if intervalSec <= 0 {
		return
	}
	interval := time.Duration(intervalSec) * time.Second
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			bs := CollectBridgeStats()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			slog.Info("stats",
				"goroutines", runtime.NumGoroutine(),
				"bridges", bs.Bridges,
				// listening < bridges 说明有桥卡在 bind 重试上
				"listening", bs.Listening,
				"conns", bs.Conns,
				"accepted", bs.Accepted,
				// rejected 增长说明撞到了并发上限
				"rejected", bs.Rejected,
				"dialOK", bs.DialOK,
				"dialFail", bs.DialFail,
				"heapAllocMB", ms.HeapAlloc>>20,
				"sysMB", ms.Sys>>20,
				"numGC", ms.NumGC)
		}
	}()
}


func AddBridge(c *gin.Context) {
	var bridge dto.UseBridge
	if err := decryptReq(c, &bridge); err != nil {
		slog.Error("AddBridge decrypt", "clientIP", c.ClientIP(), "err", err)
		respFail(c, err)
		return
	}
	if err := validateBridge(bridge); err != nil {
		slog.Error("AddBridge validate", "clientIP", c.ClientIP(),
			"port", bridge.BridgePort, "ip", bridge.Ip, "targetPort", bridge.Port, "err", err)
		respFail(c, err)
		return
	}

	unlock := lockPort(bridge.BridgePort)
	defer unlock()

	toAddr := net.JoinHostPort(bridge.Ip, strconv.FormatUint(uint64(bridge.Port), 10))
	slog.Info("AddBridge", "clientIP", c.ClientIP(), "port", bridge.BridgePort, "toAddr", toAddr)

	// 记录回滚目标：持久化失败时要恢复成数据文件里描述的状态
	var prevAddr string
	if prev := cf.Get(bridge.BridgePort); prev != nil {
		prevAddr = prev.ProxyAddr
	}

	// 目标未变且监听在跑：什么都不做，省掉一次全量重写数据文件+fsync。
	// 注意这里必须同时检查监听是否真的存在：只看缓存会在
	// 「启动时 bind 失败、记录还在但监听没起来」的情况下永远修不回来。
	if prevAddr == toAddr && hasBridgeHandler(bridge.BridgePort) {
		slog.Info("AddBridge unchanged", "port", bridge.BridgePort, "toAddr", toAddr)
		respOKData(c, map[string]any{"listening": true})
		return
	}

	// SetBridgeHandler 幂等：已有监听时只原子替换目标，不重建监听
	if err := SetBridgeHandler(bridge.BridgePort, toAddr); err != nil {
		slog.Error("AddBridge listen", "port", bridge.BridgePort, "toAddr", toAddr, "err", err)
		respFail(c, err)
		return
	}


	if err := cf.Add(bridge.BridgePort, toAddr); err != nil {
		slog.Error("AddBridge persist", "port", bridge.BridgePort, "toAddr", toAddr, "err", err)
		rollbackHandler(bridge.BridgePort, prevAddr)
		respFail(c, err)
		return
	}

	// bind 由 supervisor 异步执行且失败会一直退避重试。这里等一小会儿确认结果：
	// 没监听上就直接给管理端报错（记录保留、supervisor 继续重试，端口一释放自愈），
	// 让中心侧立刻知道这条桥当前不通，而不是收到 200 却没有流量。
	listening, bindErr := WaitBridgeListening(bridge.BridgePort, listenReadyTimeout)
	if !listening {
		if bindErr == "" {
			bindErr = "bind not completed yet"
		}
		err := fmt.Errorf("port %d is not listening: %s", bridge.BridgePort, bindErr)
		slog.Error("AddBridge not listening", "port", bridge.BridgePort,
			"toAddr", toAddr, "err", bindErr)
		respFailData(c, err, map[string]any{"listening": false, "retrying": true})
		return
	}

	slog.Info("AddBridge ok", "port", bridge.BridgePort, "toAddr", toAddr)
	respOKData(c, map[string]any{"listening": true})
}

func DelBridge(c *gin.Context) {
	var bridge dto.UseBridge
	if err := decryptReq(c, &bridge); err != nil {
		slog.Error("DelBridge decrypt", "clientIP", c.ClientIP(), "err", err)
		respFail(c, err)
		return
	}
	if bridge.BridgePort == 0 {
		err := errors.New("bridgePort is required")
		slog.Error("DelBridge validate", "clientIP", c.ClientIP(), "err", err)
		respFail(c, err)
		return
	}

	unlock := lockPort(bridge.BridgePort)
	defer unlock()

	slog.Info("DelBridge", "clientIP", c.ClientIP(), "port", bridge.BridgePort)

	// 先停监听再删记录。反序会在 cf.Del 失败时留下「记录已删、监听仍在转发」的状态，
	// 那比「记录还在、监听已停」（重启后自愈，重试即可）危险得多。
	if err := DelBridgeHandler(bridge.BridgePort); err != nil {
		slog.Error("DelBridge stop listener", "port", bridge.BridgePort, "err", err)
		respFail(c, err)
		return
	}
	if err := cf.Del(bridge.BridgePort); err != nil {
		slog.Error("DelBridge persist", "port", bridge.BridgePort, "err", err)
		respFail(c, err)
		return
	}

	slog.Info("DelBridge ok", "port", bridge.BridgePort)
	respOK(c)
}

// rollbackHandler 把 port 上的监听恢复成 prevAddr 描述的状态，
// prevAddr 为空表示这个端口本来就不该有监听。
func rollbackHandler(port uint16, prevAddr string) {
	if prevAddr == "" {
		if err := DelBridgeHandler(port); err != nil {
			slog.Error("rollback del", "port", port, "err", err)
		}
		return
	}
	if err := SetBridgeHandler(port, prevAddr); err != nil {
		slog.Error("rollback restore", "port", port, "toAddr", prevAddr, "err", err)
	}
}


func respFail(c *gin.Context, err error) {
	c.JSON(http.StatusOK, dto.Res{
		Code: 500,
		Msg:  err.Error(),
	})
}

func respFailData(c *gin.Context, err error, data any) {
	c.JSON(http.StatusOK, dto.Res{
		Code: 500,
		Msg:  err.Error(),
		Data: data,
	})
}

func respOK(c *gin.Context) {
	c.JSON(http.StatusOK, dto.Res{
		Code: 200,
		Msg:  "ok",
	})
}

func respOKData(c *gin.Context, data any) {
	c.JSON(http.StatusOK, dto.Res{
		Code: 200,
		Msg:  "ok",
		Data: data,
	})
}

func decryptReq(c *gin.Context, bridge *dto.UseBridge) error {
	var req dto.Req
	if err := c.ShouldBind(&req); err != nil {
		return err
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		return err
	}
	de, err := utils.AesDecryptCBC(data, []byte(config.Cfg.Key))
	if err != nil {
		return err
	}
	return json.Unmarshal(de, &bridge)
}

func validateBridge(bridge dto.UseBridge) error {
	if bridge.BridgePort == 0 {
		return errors.New("bridgePort is required")
	}
	if bridge.Port == 0 {
		return errors.New("port is required")
	}
	if bridge.Ip == "" {
		return errors.New("ip is required")
	}
	ip := net.ParseIP(bridge.Ip)
	if ip == nil || ip.IsUnspecified() {
		return errors.New("invalid ip")
	}
	if who, ok := selfPorts()[bridge.BridgePort]; ok {
		return fmt.Errorf("bridgePort %d conflicts with %s", bridge.BridgePort, who)
	}
	return nil
}

// selfPorts 返回本进程自己占着的端口。撞上这些端口必须在入口拒掉：
// 它们不在 runListens 里，走不到「己方桥占用 → 只换目标」的路径，
// 而 bind 又永远不可能成功（同进程同端口不能二次 bind），
// 放进去的结果只是 supervisor 无限退避重试 + 刷 error 日志。
func selfPorts() map[uint16]string {
	ports := make(map[uint16]string, 2)
	if p, ok := portFromAddr(config.Cfg.Addr); ok {
		ports[p] = "management addr"
	}
	if p, ok := portFromAddr(config.Cfg.PprofAddr); ok {
		ports[p] = "pprof addr"
	}
	return ports
}

func portFromAddr(addr string) (uint16, bool) {
	if addr == "" {
		return 0, false
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	p, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || p == 0 {
		return 0, false
	}
	return uint16(p), true
}

func syncBridge() error {
	b := dto.GetBridgesReq{
		BridgeId: config.Cfg.BridgeId,
	}

	reqD, err := json.Marshal(b)
	if err != nil {
		return err
	}

	body, err := utils.NewUrl(config.Cfg.SyncDomain).AddHeader("Content-Type", "application/json").Post("/api/notify/bridges", reqD)
	if err != nil {
		return err
	}
	slog.Debug("syncBridge", "body", string(body))
	var res dto.GetBridgesResp
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	if res.Code != 200 {
		return fmt.Errorf("sync bridges rejected: code=%d msg=%s", res.Code, res.Msg)
	}
	cf.Clear()
	if err := cf.BatchAdd(res.Data); err != nil {
		return err
	}
	slog.Info("syncBridge ok", "count", len(res.Data))
	return nil
}
