package dto

import "github.com/baowk/bridge-direct/cachef"

type Req struct {
	Ver       string `json:"ver"`
	Timestamp int64  `json:"timestamp"`
	Data      string `json:"data"`
}

type Res struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

type GetBridgesReq struct {
	BridgeId uint `json:"bridgeId"`
}

type GetBridgesResp struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data []cachef.Bridge `json:"data"`
}

type UseBridge struct {
	BridgePort uint16 `json:"bridgePort"`
	Ip         string `json:"ip"`
	Port       uint16 `json:"port"`
}
