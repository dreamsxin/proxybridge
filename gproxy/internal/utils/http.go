package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

func GetClient(outIp string) *http.Client {
	if outIp != "" {
		return &http.Client{
			Transport: &http.Transport{
				Dial: func(netW, addr string) (net.Conn, error) {
					//本地地址  ipaddr是本地外网IP
					lAddr, err := net.ResolveTCPAddr(netW, outIp+":0")
					if err != nil {
						return nil, err
					}
					//被请求的地址
					rAddr, err := net.ResolveTCPAddr(netW, addr)
					if err != nil {
						return nil, err
					}
					conn, err := net.DialTCP(netW, lAddr, rAddr)
					if err != nil {
						return nil, err
					}
					deadline := time.Now().Add(35 * time.Second)
					conn.SetDeadline(deadline)
					return conn, nil
				},
			},
		}
	} else {
		return &http.Client{}
	}
}

func GetClientByLocalAddr(lAddr *net.TCPAddr) *http.Client {
	if lAddr != nil {
		return &http.Client{
			Transport: &http.Transport{
				Dial: func(netW, addr string) (net.Conn, error) {
					//本地地址  ipaddr是本地外网IP
					// lAddr, err := net.ResolveTCPAddr(netw, outip+":0")
					// if err != nil {
					// 	return nil, err
					// }
					//被请求的地址
					rAddr, err := net.ResolveTCPAddr(netW, addr)
					if err != nil {
						return nil, err
					}
					conn, err := net.DialTCP(netW, lAddr, rAddr)
					if err != nil {
						return nil, err
					}
					deadline := time.Now().Add(35 * time.Second)
					conn.SetDeadline(deadline)
					return conn, nil
				},
			},
		}
	} else {
		return &http.Client{}
	}
}

func PostJson(url string, params any, resp *NotifyResp, outIp string) error {
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	request, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	ctx, _ := context.WithTimeout(context.Background(), time.Second*15)
	request = request.WithContext(ctx)
	client := GetClient(outIp)
	res, err := client.Do(request) // 发起客户端请求
	if err != nil {
		//slog.Error("post", "url", url, "data", string(data), "err", err)
		return err
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		slog.Error("read all", "url", url, "err", err)
		return err
	}
	//fmt.Println(string(body))
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("response failed with status code: %d", res.StatusCode)
	}
	return json.Unmarshal(body, resp)
}

type NotifyReq struct {
	Timestamp int64  `json:"timestamp"`
	Data      string `json:"data"`
}

type NotifyResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data string `json:"data"`
}
