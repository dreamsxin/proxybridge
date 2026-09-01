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
	Mode       string `json:"mode"`       //模式
	LogSource  bool   `json:"logSource"`  //日志是否显示代码行
	LogFile    string `json:"logFile"`    //日志文件
	LogConsole bool   `json:"logConsole"` //配了logFile时是否同时输出到控制台

	LogLevel     string `json:"logLevel"`     //日志级别 默认debug info warn error
	LogFormat    string `json:"logFormat"`    //logFormat 默认text，json
	Addr         string `json:"addr"`         //监听地址
	SyncDomain   string `json:"syncDomain"`   //同步域名
	Key          string `json:"key"`          //aws加密key
	DataFilename string `json:"dataFilename"` //数据文件
	BridgeId     uint   `json:"bridgeId"`     //桥id

	// PprofAddr pprof监听地址，空=关闭。只能绑到内网/回环，例如 127.0.0.1:6060
	PprofAddr string `json:"pprofAddr"`
	// StatsInterval 运行指标日志间隔(秒)，0=关闭。排查泄漏时建议 60
	StatsInterval int `json:"statsInterval"`
	// ConnIdleTimeout 单连接空闲超时(秒)，0=不限制。
	// 浏览器SOCKS5场景注意：设太小会掐掉连接池里的空闲socket和WebSocket长连接
	ConnIdleTimeout int `json:"connIdleTimeout"`
	// MaxConnsPerPort 单个桥端口最大并发连接数，0=不限制。
	// 作用是给fd用量兜底，先用statsInterval观测真实水位再定值
	MaxConnsPerPort int `json:"maxConnsPerPort"`
	// MaxConns 进程级最大并发连接数，0=不限制。
	// 单端口上限拦不住桥的总数，这道全局墙才是 fd 耗尽（EMFILE 打死 accept）的真正兜底
	MaxConns int `json:"maxConns"`
}

