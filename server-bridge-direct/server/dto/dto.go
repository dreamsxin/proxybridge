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

// BridgeStatus 描述一条桥的运行态和可选 TCP 探测结果。
type BridgeStatus struct {
	BridgePort    uint16 `json:"bridgePort"`
	ProxyAddr     string `json:"proxyAddr"`
	Listening     bool   `json:"listening"`
	BridgeTCP     bool   `json:"bridgeTcp"`
	ProxyTCP      bool   `json:"proxyTcp"`
	OK            bool   `json:"ok"`
	BindErr       string `json:"bindErr,omitempty"`
	BridgeErr     string `json:"bridgeErr,omitempty"`
	ProxyErr      string `json:"proxyErr,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	Solution      string `json:"solution,omitempty"`
}

// BridgeStartResult 是 /bridge/start 的结果。started 表示已创建监听，
// alreadyListening 表示原监听已经在运行，failed 表示后台可能仍在重试但当前未就绪。
type BridgeStartResult struct {
	BridgePort uint16 `json:"bridgePort"`
	ProxyAddr  string `json:"proxyAddr"`
	Status     string `json:"status"`
	Retrying   bool   `json:"retrying,omitempty"`
	Err        string `json:"err,omitempty"`
}
