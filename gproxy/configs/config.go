package configs

import (
	"github.com/baowk/gproxy/internal/utils"
)

const (
	MODE_LOCAL  = "local"
	MODE_REMOTE = "remote"
)

var Cfg *Config

// Config holds the proxy server configuration
type Config struct {
	Os                  string    `json:"os"`                           //操作系统
	Arch                string    `json:"arch"`                         //架构
	SupplierCode        string    `json:"supplierCode"`                 //供应商代码
	Mode                string    `json:"mode" yaml:"mode"`             //local,remote
	CenterDomain        string    `json:"centerDomain"`                 //中心服务域名 远程的时候可用
	IpScan              bool      `json:"ipScan" yaml:"ipScan"`         //本地ip扫描
	Ipv6                bool      `json:"ipv6" yaml:"ipv6"`             //是否启用ipv6
	Port                uint16    `json:"port" yaml:"port"`             //默认端口
	Username            string    `json:"username" yaml:"username"`     //默认用户名
	Password            string    `json:"password" yaml:"password"`     //默认密码
	TcpTimeout          int       `json:"tcpTimeout" yaml:"tcpTimeout"` //tcp超时时间 默认0
	UdpTimeout          int       `json:"udpTimeout" yaml:"udpTimeout"` //udp超时时间 默认60
	DBName              string    `json:"dbName"`                       //数据库名
	Log                 Log       `json:"log" yaml:"log"`               //日志
	UploadLog           UploadLog `json:"uploadLog"`                    //上传日志组件
	SrcWhiteList        []string  `json:"srcWhiteList"`                 //源ip白名单 local模式有效
	BlackDomainSuffixes []string  `json:"blackDomainSuffixes"`          //禁止访问的域名 local模式有效
	Key                 string    `json:"key"`                          //aws加密key
	AccessMode          uint8     `json:"accessMode"`                   //0无限制 1 限制中国大陆ip访问 2限制大陆以外ip访问 0无限制
	GeoDb               string    `json:"geoDb"`                        //ip库
	//HttpPort            uint16    `json:"httpPort"`                     // http服务端口
	//ApiKey              string    `json:"apiKey"`                       //api key
}

const _KEY = "Ybcd1234efgh5678#11122GG"

func (c *Config) GetKey() (string, error) {
	dstr, err := utils.AesDecryptCBCStr(c.Key, _KEY)
	if err != nil {
		return "", err
	}
	return string(dstr), nil
}

func (c *Config) IsRemote() bool {
	return c.Mode == MODE_REMOTE
}

// func (c *Config) GetHttpPort() uint16 {
// 	if c.HttpPort == 0 {
// 		c.HttpPort = 8080
// 	}
// 	return c.HttpPort
// }

type UploadLog struct {
	Enable bool   `json:"enable"`
	Addr   string `json:"addr"`
}

func (c *Config) GetUploadLogAddr() string {
	return c.UploadLog.Addr
}

type Log struct {
	Output     string `json:"output" yaml:"output"`
	Level      string `json:"level" yaml:"level"`
	MaxAge     int    `json:"maxAge" yaml:"maxAge"`
	MaxSize    int    `json:"maxSize" yaml:"maxSize"`
	MaxBackups int    `json:"maxBackups" yaml:"maxBackups"`
	Format     string `json:"format" yaml:"format"`
	AddSource  bool   `json:"addSource" yaml:"addSource"`
}

// DefaultConfig returns a default configuration
func DefaultConfig() *Config {
	return &Config{
		IpScan:     true,
		Username:   "test",
		Password:   "123456",
		TcpTimeout: 0,
		UdpTimeout: 60,
		Port:       5206,
		Log: Log{
			Level:      "debug",
			MaxAge:     7,
			MaxSize:    100,
			MaxBackups: 3,
			Format:     "json",
		},
		DBName: "proxies.db",
	}
}

func (c *Config) GetDBName() string {
	if c.DBName == "" {
		c.DBName = "proxies.db"
	}
	return c.DBName
}

func (c *Config) GetCenterDomain() string {
	if c.CenterDomain == "" {
		return "https://api.ipipv.com"
	}
	return c.CenterDomain
}

// HasAuth checks if any authentication is configured
