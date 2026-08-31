package config

import (
	"log/slog"
)

var (
	CfgFile string
	Logger  *slog.Logger
	Cfg     Config
)

const (
	MODE_LOCAL  = "local"
	MODE_REMOTE = "remote"
)

type Config struct {
	Mode         string `json:"mode"`         //模式
	LogSource    bool   `json:"logSource"`    //日志是否显示代码行
	LogFile      string `json:"logFile"`      //日志文件
	LogConsole   bool   `json:"logConsole"`   //配了logFile时是否同时输出到控制台

	LogLevel     string `json:"logLevel"`     //日志级别 默认debug info warn error
	LogFormat    string `json:"logFormat"`    //logFormat 默认text，json
	Addr         string `json:"addr"`         //监听地址
	SyncDomain   string `json:"syncDomain"`   //同步域名
	Key          string `json:"key"`          //aws加密key
	DataFilename string `json:"dataFilename"` //数据文件
	BridgeId     uint   `json:"bridgeId"`     //桥id
}
