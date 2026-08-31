package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/baowk/bridge-direct/cachef"
	"github.com/baowk/bridge-direct/config"
	"github.com/baowk/bridge-direct/server/dto"
	"github.com/baowk/bridge-direct/utils"
	"github.com/gin-gonic/gin"
)

var cf *cachef.CacheF

// apiMu 串行化管理接口。cf（持久化映射表）和 runListens（实际监听）是两份独立状态，
// 并发的 add/del 会在「查缓存」和「改监听」之间产生分叉，表现为
// "port N already listening" 或删除后监听仍在。
var apiMu sync.Mutex

func Start() {
	var err error
	if cf, err = cachef.New(config.Cfg.DataFilename); err != nil {
		panic(err)
	}

	if config.Cfg.Mode != config.MODE_LOCAL {
		//同步桥
		if err := syncBridge(); err != nil {
			slog.Error("syncBridge", "syncDomain", config.Cfg.SyncDomain,
				"bridgeId", config.Cfg.BridgeId, "err", err)
		}
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

	apiMu.Lock()
	defer apiMu.Unlock()

	toAddr := net.JoinHostPort(bridge.Ip, strconv.FormatUint(uint64(bridge.Port), 10))
	slog.Info("AddBridge", "clientIP", c.ClientIP(), "port", bridge.BridgePort, "toAddr", toAddr)

	// 记录回滚目标：持久化失败时要恢复成数据文件里描述的状态
	var prevAddr string
	if prev := cf.Get(bridge.BridgePort); prev != nil {
		prevAddr = prev.ProxyAddr
	}

	// AddBridgeHandler 是幂等的，新增与改目标同一路径，不再按缓存有无分支
	if err := AddBridgeHandler(bridge.BridgePort, toAddr); err != nil {
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

	slog.Info("AddBridge ok", "port", bridge.BridgePort, "toAddr", toAddr)
	respOK(c)
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

	apiMu.Lock()
	defer apiMu.Unlock()

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
	if err := AddBridgeHandler(port, prevAddr); err != nil {
		slog.Error("rollback restore", "port", port, "toAddr", prevAddr, "err", err)
	}
}

func respFail(c *gin.Context, err error) {
	c.JSON(http.StatusOK, dto.Res{
		Code: 500,
		Msg:  err.Error(),
	})
}

func respOK(c *gin.Context) {
	c.JSON(http.StatusOK, dto.Res{
		Code: 200,
		Msg:  "ok",
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
	return nil
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
