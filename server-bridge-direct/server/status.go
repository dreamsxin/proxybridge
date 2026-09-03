package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/baowk/bridge-direct/server/dto"
	"github.com/gin-gonic/gin"
)

// GetBridgeStatus 返回当前进程中的桥监听状态。
//
// 这个接口只接受本机回环请求，避免把桥端口和目标代理地址暴露给外部。
// 不带 check 时只返回运行态监听信息；check=1/true 时额外做桥端口和目标
// 代理的 TCP 建连探测。
func GetBridgeStatus(c *gin.Context) {
	if !isLoopbackRequest(c.Request.RemoteAddr) {
		c.JSON(http.StatusOK, dto.Res{Code: http.StatusForbidden, Msg: "bridge status is only available from localhost"})
		return
	}

	bridgePort, err := queryBridgePort(c)
	if err != nil {
		respFail(c, err)
		return
	}
	check := c.Query("check") == "1" || strings.EqualFold(c.Query("check"), "true")
	data := RuntimeBridgeStatus(bridgePort, c.Query("proxyAddr"), check)
	c.JSON(http.StatusOK, bridgeStatusResponse{
		Code:  http.StatusOK,
		Msg:   "ok",
		Data:  data,
		Stats: CollectRuntimeStats(),
	})
}

// bridgeStatusResponse 保持原有 data 数组格式，在顶层补充 startStatsLogger
// 使用的进程/桥统计。已有客户端只读取 data 时不受影响。
type bridgeStatusResponse struct {
	Code  int                `json:"code"`
	Msg   string             `json:"msg"`
	Data  []dto.BridgeStatus `json:"data"`
	Stats RuntimeStats       `json:"stats"`
}

// RuntimeStats 是 startStatsLogger 和 /bridge/status 共享的统计快照。
type RuntimeStats struct {
	Goroutines  int    `json:"goroutines"`
	Bridges     int    `json:"bridges"`
	Listening   int    `json:"listening"`
	Conns       int    `json:"conns"`
	Accepted    int64  `json:"accepted"`
	Rejected    int64  `json:"rejected"`
	DialOK      int64  `json:"dialOK"`
	DialFail    int64  `json:"dialFail"`
	HeapAllocMB uint64 `json:"heapAllocMB"`
	SysMB       uint64 `json:"sysMB"`
	NumGC       uint32 `json:"numGC"`
}

// CollectRuntimeStats 采集与 startStatsLogger 相同的运行水位。
func CollectRuntimeStats() RuntimeStats {
	bridgeStats := CollectBridgeStats()
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	return RuntimeStats{
		Goroutines:  runtime.NumGoroutine(),
		Bridges:     bridgeStats.Bridges,
		Listening:   bridgeStats.Listening,
		Conns:       bridgeStats.Conns,
		Accepted:    bridgeStats.Accepted,
		Rejected:    bridgeStats.Rejected,
		DialOK:      bridgeStats.DialOK,
		DialFail:    bridgeStats.DialFail,
		HeapAllocMB: memStats.HeapAlloc >> 20,
		SysMB:       memStats.Sys >> 20,
		NumGC:       memStats.NumGC,
	}
}

// StartBridge 从 bridge.db 中恢复一个已有桥的运行态监听。
// 这个接口不修改代理配置；代理目标变更必须使用 /bridge/add。
func StartBridge(c *gin.Context) {
	if !isLoopbackRequest(c.Request.RemoteAddr) {
		c.JSON(http.StatusOK, dto.Res{Code: http.StatusForbidden, Msg: "bridge start is only available from localhost"})
		return
	}
	if cf == nil {
		respFail(c, errors.New("bridge cache is not initialized"))
		return
	}
	bridgePort, err := queryBridgePort(c)
	if err != nil {
		respFail(c, err)
		return
	}
	if bridgePort == 0 {
		respFail(c, errors.New("bridgePort is required"))
		return
	}

	// 和 add/del 共用端口锁，覆盖读取持久化配置到启动监听的完整流程，
	// 防止 start 读取旧目标后与并发 add/del 交叉执行。
	unlock := lockPort(bridgePort)
	defer unlock()

	configured := cf.Get(bridgePort)
	if configured == nil {
		c.JSON(http.StatusOK, dto.Res{Code: http.StatusNotFound, Msg: "bridge config not found"})
		return
	}

	slog.Info("StartBridge", "clientIP", c.ClientIP(), "port", bridgePort, "toAddr", configured.ProxyAddr)

	runMu.RLock()
	listener, running := runListens[bridgePort]
	currentTarget := ""
	listening := false
	closed := false
	if running {
		currentTarget = listener.currentTarget()
		listening = listener.listening.Load()
		closed = listener.closed.Load()
	}
	runMu.RUnlock()

	if running && !closed && currentTarget != configured.ProxyAddr {
		result := dto.BridgeStartResult{
			BridgePort: bridgePort,
			ProxyAddr:  configured.ProxyAddr,
			Status:     "conflict",
			Err:        fmt.Sprintf("bridgePort already listening to %s", currentTarget),
		}
		c.JSON(http.StatusOK, dto.Res{Code: http.StatusInternalServerError, Msg: "bridge listener has a different target", Data: result})
		return
	}

	status := "started"
	if running && !closed && listening {
		status = "alreadyListening"
	} else {
		if err := SetBridgeHandler(bridgePort, configured.ProxyAddr); err != nil {
			result := dto.BridgeStartResult{BridgePort: bridgePort, ProxyAddr: configured.ProxyAddr, Status: "failed", Err: err.Error()}
			c.JSON(http.StatusOK, dto.Res{Code: http.StatusInternalServerError, Msg: "bridge listener failed to start", Data: result})
			return
		}
		ready, bindErr := WaitBridgeListening(bridgePort, listenReadyTimeout)
		if !ready {
			if bindErr == "" {
				bindErr = "listener is not ready"
			}
			result := dto.BridgeStartResult{
				BridgePort: bridgePort,
				ProxyAddr:  configured.ProxyAddr,
				Status:     "failed",
				Retrying:   true,
				Err:        bindErr,
			}
			slog.Error("StartBridge not ready", "port", bridgePort, "toAddr", configured.ProxyAddr, "err", bindErr)
			c.JSON(http.StatusOK, dto.Res{Code: http.StatusInternalServerError, Msg: "bridge listener is not ready", Data: result})
			return
		}
	}

	result := dto.BridgeStartResult{BridgePort: bridgePort, ProxyAddr: configured.ProxyAddr, Status: status}
	slog.Info("StartBridge ok", "port", bridgePort, "toAddr", configured.ProxyAddr, "status", status)
	c.JSON(http.StatusOK, dto.Res{Code: http.StatusOK, Msg: "ok", Data: result})
}

func queryBridgePort(c *gin.Context) (uint16, error) {
	value := c.Query("bridgePort")
	if value == "" {
		return 0, nil
	}
	p, err := strconv.ParseUint(value, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid bridgePort: %w", err)
	}
	return uint16(p), nil
}

// RuntimeBridgeStatus 汇总运行态监听器，并补充 bridge.db 中存在但运行态
// 没有 listener 的配置。网络探测在释放全局锁后执行，避免阻塞 add/del。
func RuntimeBridgeStatus(bridgePort uint16, proxyAddr string, check bool) []dto.BridgeStatus {
	runMu.RLock()
	statuses := make([]dto.BridgeStatus, 0, len(runListens))
	runningPorts := make(map[uint16]struct{}, len(runListens))
	for port, listener := range runListens {
		runningPorts[port] = struct{}{}
		toAddr := listener.currentTarget()
		if !bridgeStatusMatches(port, toAddr, bridgePort, proxyAddr) {
			continue
		}
		status := dto.BridgeStatus{
			BridgePort: port,
			ProxyAddr:  toAddr,
			Listening:  !listener.closed.Load() && listener.listening.Load(),
			BindErr:    listener.lastBindErr(),
		}
		if !status.Listening {
			status.FailureReason, status.Solution = bridgeStatusAdvice(status, false)
		}
		statuses = append(statuses, status)
	}
	runMu.RUnlock()

	if cf != nil {
		for _, configured := range cf.All() {
			if configured == nil {
				continue
			}
			if _, ok := runningPorts[configured.Port]; ok {
				continue
			}
			if !bridgeStatusMatches(configured.Port, configured.ProxyAddr, bridgePort, proxyAddr) {
				continue
			}
			status := dto.BridgeStatus{
				BridgePort: configured.Port,
				ProxyAddr:  configured.ProxyAddr,
				Listening:  false,
			}
			status.FailureReason, status.Solution = bridgeStatusAdvice(status, false)
			statuses = append(statuses, status)
		}
	}

	sort.Slice(statuses, func(i, j int) bool { return statuses[i].BridgePort < statuses[j].BridgePort })
	if check {
		for i := range statuses {
			statuses[i] = checkRuntimeBridge(statuses[i])
		}
	}
	return statuses
}

func bridgeStatusMatches(port uint16, toAddr string, bridgePort uint16, proxyAddr string) bool {
	if bridgePort != 0 && port != bridgePort {
		return false
	}
	return proxyAddr == "" || toAddr == proxyAddr
}

func checkRuntimeBridge(status dto.BridgeStatus) dto.BridgeStatus {
	status.BridgeTCP, status.BridgeErr = tcpReachable(net.JoinHostPort("127.0.0.1", strconv.Itoa(int(status.BridgePort))))
	status.ProxyTCP, status.ProxyErr = tcpReachable(status.ProxyAddr)
	status.OK = status.Listening && status.BridgeTCP && status.ProxyTCP
	if !status.OK {
		status.FailureReason, status.Solution = bridgeStatusAdvice(status, true)
	}
	return status
}

func bridgeStatusAdvice(status dto.BridgeStatus, checked bool) (string, string) {
	var reasons []string
	var solutions []string
	if !status.Listening {
		reason := "运行态监听器不存在或未就绪"
		if status.BindErr != "" {
			reason += "：" + status.BindErr
		}
		reasons = append(reasons, reason)
		solutions = append(solutions, "检查端口占用和 bind 错误；可调用 POST /bridge/start?bridgePort=端口 重试。")
	}
	if checked && !status.BridgeTCP {
		reasons = append(reasons, "桥端口 TCP 探测失败："+status.BridgeErr)
		solutions = append(solutions, "检查 bridgePort 是否仍由 bridge-direct 监听以及本机防火墙策略。")
	}
	if checked && !status.ProxyTCP {
		reasons = append(reasons, "目标代理 TCP 探测失败："+status.ProxyErr)
		solutions = append(solutions, "检查 proxyAddr 的地址、端口、目标代理服务和网络策略。")
	}
	return strings.Join(reasons, "；"), strings.Join(solutions, " ")
}

func tcpReachable(addr string) (bool, string) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false, err.Error()
	}
	_ = conn.Close()
	return true, ""
}

func isLoopbackRequest(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
