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

// 配置缺省值。配置项缺失或非法（<=0）时使用这些值，保证日志占用有上界。
const (
	DefaultLogLevel      = "info"
	DefaultLogFormat     = "text"
	DefaultLogMaxSizeMB  = 100
	DefaultLogMaxAgeDays = 7
	DefaultLogMaxBackups = 10
)

type Config struct {
	Mode       string `json:"mode"`       //模式
	LogSource  bool   `json:"logSource"`  //日志是否显示代码行
	LogFile    string `json:"logFile"`    //日志文件
	LogConsole bool   `json:"logConsole"` //配了logFile时是否同时输出到控制台

	LogLevel  string `json:"logLevel"`  //日志级别 debug info warn error，缺省 info
	LogFormat string `json:"logFormat"` //logFormat text 或 json，缺省 text

	// LogMaxSizeMB 单个日志文件大小上限(MB)，到达就切割归档。缺省 100
	LogMaxSizeMB int `json:"logMaxSizeMB"`
	// LogMaxAgeDays 归档保留天数。缺省 7
	LogMaxAgeDays int `json:"logMaxAgeDays"`
	// LogMaxBackups 归档保留个数，与 LogMaxAgeDays 谁先满足谁生效。缺省 10
	LogMaxBackups int `json:"logMaxBackups"`
	// LogCompress 归档是否压缩，缺省压缩（文本日志压缩比约 8:1）。
	// 用指针是为了区分「没配」和「显式配了 false」——bool 的零值本身就是 false
	LogCompress *bool `json:"logCompress"`

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

// ApplyDefaults 给缺失或非法的配置项填缺省值，必须在读取配置之后、使用配置之前调用。
//
// 只对「缺失等价于错误」的项兜底：日志级别缺省 info（缺省 debug 会让每条连接打两行
// flow 日志，SOCKS5 场景下日志量按连接数放大），轮转参数缺省给出 100MB×10 份、7 天，
// 保证日志占用有上界。logConsole / logSource 的缺省就是 bool 零值 false：
// 生产日志一般交给 journald 收集，双写只会重复采集。
func (c *Config) ApplyDefaults() {
	if c.LogLevel == "" {
		c.LogLevel = DefaultLogLevel
	}
	if c.LogFormat == "" {
		c.LogFormat = DefaultLogFormat
	}
	if c.LogMaxSizeMB <= 0 {
		c.LogMaxSizeMB = DefaultLogMaxSizeMB
	}
	if c.LogMaxAgeDays <= 0 {
		c.LogMaxAgeDays = DefaultLogMaxAgeDays
	}
	if c.LogMaxBackups <= 0 {
		c.LogMaxBackups = DefaultLogMaxBackups
	}
}

// LogCompressEnabled 归档是否压缩，没配时默认开启
func (c *Config) LogCompressEnabled() bool {
	if c.LogCompress == nil {
		return true
	}
	return *c.LogCompress
}
