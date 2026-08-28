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

	"github.com/baowk/bridge-direct/cachef"
	"github.com/baowk/bridge-direct/config"
	"github.com/baowk/bridge-direct/server/dto"
	"github.com/baowk/bridge-direct/utils"
	"github.com/gin-gonic/gin"
)

var cf *cachef.CacheF

func Start() {
	var err error
	if cf, err = cachef.New(config.Cfg.DataFilename); err != nil {
		panic(err)
	}

	if config.Cfg.Mode != config.MODE_LOCAL {
		//同步桥
		syncBridge()
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
		fmt.Println("Started Server Listen", config.Cfg.Addr)
		r.Run(config.Cfg.Addr)
	}
}

func AddBridge(c *gin.Context) {
	var bridge dto.UseBridge
	if err := decryptReq(c, &bridge); err != nil {
		c.JSON(http.StatusOK, dto.Res{
			Code: 500,
			Msg:  err.Error(),
		})
		return
	}
	if err := validateBridge(bridge); err != nil {
		c.JSON(http.StatusOK, dto.Res{
			Code: 500,
			Msg:  err.Error(),
		})
		return
	}

	var err error
	var err2 error
	toAddr := net.JoinHostPort(bridge.Ip, strconv.FormatUint(uint64(bridge.Port), 10))
	if cf.Get(bridge.BridgePort) != nil {
		err2 = ChangeBridgeHandler(bridge.BridgePort, toAddr)
	} else {
		err2 = AddBridgeHandler(bridge.BridgePort, toAddr)
	}
	if err2 != nil {
		c.JSON(http.StatusOK, dto.Res{
			Code: 500,
			Msg:  err2.Error(),
		})
		return
	}
	err = cf.Add(bridge.BridgePort, toAddr)
	if err != nil {
		DelBridgeHandler(bridge.BridgePort)
		c.JSON(http.StatusOK, dto.Res{
			Code: 500,
			Msg:  err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, dto.Res{
		Code: 200,
		Msg:  "ok",
	})
}

func DelBridge(c *gin.Context) {
	var bridge dto.UseBridge
	if err := decryptReq(c, &bridge); err != nil {
		c.JSON(http.StatusOK, dto.Res{
			Code: 500,
			Msg:  err.Error(),
		})
		return
	}
	if bridge.BridgePort == 0 {
		c.JSON(http.StatusOK, dto.Res{
			Code: 500,
			Msg:  "bridgePort is required",
		})
		return
	}
	slog.Info("DelBridge", "bridge", bridge)
	err := cf.Del(bridge.BridgePort)
	err2 := DelBridgeHandler(bridge.BridgePort)
	if err != nil {
		c.JSON(http.StatusOK, dto.Res{
			Code: 500,
			Msg:  err.Error(),
		})
		return
	}
	if err2 != nil {
		c.JSON(http.StatusOK, dto.Res{
			Code: 500,
			Msg:  err2.Error(),
		})
		return
	}
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
	if res.Code == 200 {
		cf.Clear()
		cf.BatchAdd(res.Data)
	}
	return nil
}
