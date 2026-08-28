package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/baowk/gproxy/internal/utils"
)

func proxyCheck(appCode string, ver int, disableIpMode uint8, os, arch, key, domain string) (resp CheckResp, err error) {
	req := CheckReq{
		AppCode:       appCode,
		Ver:           ver,
		Os:            os,
		Arch:          arch,
		DisableIpMode: disableIpMode,
	}

	eReq, err := Encode(req, key)
	if err != nil {
		slog.Error("[ProxyCheck]", "Encode", err)
		return
	}
	var eResp utils.NotifyResp
	err = utils.PostJson(domain+"/api/notify/proxy/check", eReq, &eResp, "")
	if err != nil {
		slog.Error("[Encode]", "PostJson", err)
		return
	}
	if eResp.Code != 200 {
		err = fmt.Errorf("err:%s", eResp.Msg)
		return
	}
	err = Decode(eResp.Data, key, &resp)
	slog.Debug("[ProxyCheck]", "res", resp)
	return
}

func Encode(req any, key string) (*utils.NotifyReq, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	strD, err := utils.AesEncryptCBC(data, []byte(key))
	if err != nil {
		return nil, err
	}
	reqD := &utils.NotifyReq{
		Data:      base64.StdEncoding.EncodeToString(strD),
		Timestamp: time.Now().Unix(),
	}
	return reqD, nil
}

func Decode(data, key string, res any) error {
	bs, err := utils.AesDecryptCBCStr(data, key)
	if err != nil {
		slog.Error("[decode]", "err", err)
		return err
	}
	//fmt.Println("[decode]", string(bs))
	return json.Unmarshal(bs, res)
}

type CheckReq struct {
	DisableIpMode uint8  `json:"disableIpMode"` //禁用ip类型
	AppCode       string `json:"appCode"`       //应用名
	SupplierCode  string `json:"supplierCode"`  //供应商代码
	Ver           int    `json:"ver"`           //版本
	Os            string `json:"os"`            //操作系统
	Arch          string `json:"arch"`          //架构
}

type CheckResp struct {
	BWIps      map[string]int8 `json:"whiteIps"`  //白名单
	LogAddr    string          `json:"logAddr"`   //日志上传地址
	Version    RVersion        `json:"version"`   //日志信息
	DialWhites []string        `json:"dialWhite"` //访问白名单
	DialBlacks []string        `json:"dialBlack"` //访问黑名单
}

type RVersion struct {
	Ver         uint   `json:"ver"`         //版本
	DownloadUrl string `json:"downloadUrl"` //下载地址
	Md5sum      string `json:"md5sum"`      //当前版本Md5sum值
}
