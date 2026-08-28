package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/baowk/gproxy/configs"
)

// 定义处理/about路径请求的处理函数
func (s *HttpServer) httpProxyHandler(w http.ResponseWriter, r *http.Request) {
	apiKey := r.URL.Query().Get("apiKey")
	reqId := r.URL.Query().Get("reqId")

	if !s.verify(apiKey) {
		http.Error(w, NewErrResp("", http.StatusUnauthorized, "未授权").ToString(), http.StatusUnauthorized)
		return
	}

	host, _, _ := net.SplitHostPort(r.Host)
	fmt.Println(host)

	// ver := enums.VersionToNum()
	// key, _ := s.cfg.GetKey()
	// pis := store.DbIns.All()

	// for _, pi := range pis {
	// 	if pi.OutIp == host || pi.Ip == host {
	// 		s, err := NewServer(pi, 0, 0)
	// 		store.DbIns.Sync()
	// 		break
	// 	}
	// }

	// 可以在这里对解析后的person结构体数据进行后续处理，比如打印或者存入数据库等
	w.Write(NewResp(reqId, 200, "OK").ToBytes())
}

func (s *HttpServer) verify(apiKey string) bool {
	return apiKey == s.apiKey
}

// type Req struct {
// 	ReqId     string     `json:"reqId"`     // 请求id
// 	Ver       string     `json:"ver"`       // 版本
// 	Option    string     `json:"option"`    // 操作 upsert del
// 	ProxyInfo *ProxyInfo `json:"proxyInfo"` // 请求数据
// }

// type ProxyInfo struct {
// 	Ip       string `json:"ip"`
// 	Port     int    `json:"port"`
// 	Username string `json:"username"`
// 	Password string `json:"password"`
// 	Expire   int64  `json:"expire"`
// }

type Resp struct {
	ReqId string `json:"reqId"` // 请求id
	Code  int    `json:"code"`  // 返回code
	Msg   string `json:"msg"`   // 消息
	//ProxyInfo *ProxyInfo `json:"proxyInfo"` // 请求数据
}

func NewResp(reqId string, code int, msg string) *Resp {
	return &Resp{
		ReqId: reqId,
		Code:  code,
		Msg:   msg,
		//		ProxyInfo: proxyInfo,
	}
}

func NewErrResp(reqId string, code int, msg string) *Resp {
	return &Resp{
		ReqId: reqId,
		Code:  code,
		Msg:   msg,
	}
}

func (s *Resp) ToString() string {
	jsonData, _ := json.Marshal(s)
	return string(jsonData)
}

func (s *Resp) ToBytes() []byte {
	jsonData, _ := json.Marshal(s)
	return jsonData
}

func (s *HttpServer) start(port uint16) error {
	var err error
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/proxy", s.httpProxyHandler)
		err = http.ListenAndServe(fmt.Sprintf(":%d", port), mux)
	}()
	if err != nil {
		slog.Error("启动HTTP服务出错", "err", err)
		return err
	}
	slog.Info("启动HTTP服务成功", "port", port)
	return nil
}

type HttpServer struct {
	cfg    *configs.Config
	port   uint16
	apiKey string
}

// func NewHttpServer(cfg *configs.Config) (*HttpServer, error) {
// 	hs := &HttpServer{
// 		cfg: cfg,
// 	}
// 	err := hs.start(cfg.HttpPort)

// 	return hs, err
// }
