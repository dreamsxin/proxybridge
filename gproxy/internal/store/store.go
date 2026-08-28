package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/baowk/gproxy/configs"
	"github.com/baowk/gproxy/internal/utils"
)

type ProxyInfo struct {
	Port      uint16               `json:"port"`      //监听端口
	Ip        string               `json:"ip"`        //监听ip
	Users     map[string]*UserInfo `json:"users"`     //用户
	UpdatedAt int64                `json:"updatedAt"` //最后更新时间
	mu        sync.Mutex           `json:"-"`
	//OutIp     string               `json:"outIp"`     //出口ip
	//PubId     string               `json:"pubId" yaml:"pubId"`         //公网ip
}

type UserInfo struct {
	Password   string `json:"password"`   //密码
	Expire     int64  `json:"expire"`     //有效期
	AccessMode uint8  `json:"accessMode"` //访问模式
}

func (s *ProxyInfo) Auth(username, password string) bool {
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.Users[username]; ok {
		if u.Password == password && (u.Expire == 0 || u.Expire > now) {
			if configs.Cfg.IsRemote() && now-s.UpdatedAt > 1800 {
				go func() {
					syncProxy(s)
				}()
			}
			return true
		} else if now-s.UpdatedAt > 60 {
			if configs.Cfg.IsRemote() {
				syncProxy(s)
				if u, ok := s.Users[username]; ok {
					if u.Password == password && (u.Expire == 0 || u.Expire > now) {
						return true
					}
				}
			}
		}
	} else if now-s.UpdatedAt > 60 {
		if configs.Cfg.IsRemote() {
			syncProxy(s)
			if u, ok := s.Users[username]; ok {
				if u.Password == password && (u.Expire == 0 || u.Expire > now) {
					return true
				}
			}
		}
	}
	return false
}

// 实例状态同步
type InstanceStateReq struct {
	InstanceId   string `json:"instanceId"`   //实列id
	SupplierCode string `json:"supplierCode"` //供应商代码
	Ip           string `json:"ip"`           //ip
	Port         uint16 `json:"port"`         //端口
}

func syncProxy(s *ProxyInfo) {
	key, _ := configs.Cfg.GetKey()
	resp, err := syncState(configs.Cfg.SupplierCode, s.Ip, key, configs.Cfg.GetCenterDomain())
	if err == nil {
		if resp.Ip != "" {
			for k, v := range s.Users {
				if _, ok := resp.Users[k]; ok {
					s.Users[k] = v
				} else {
					delete(s.Users, k)
				}
			}
			for k, v := range resp.Users {
				s.Users[k] = &v
			}
		}
		//s.UpdatedAt = time.Now().Unix()
		DbIns.SyncPut(s)
	} else {
		slog.Error("[sync]", "err", err)
	}
}

func syncState(supplierCode, ip, key, domain string) (resp *RespPi, err error) {
	req := InstanceStateReq{
		Ip:           ip,
		SupplierCode: supplierCode,
	}

	eReq, err := Encode(req, key)
	if err != nil {
		slog.Error("[SyncState]", "Encode", err)
		return
	}
	var eResp utils.NotifyResp
	err = utils.PostJson(domain+"/api/notify/instance/state/v2", eReq, &eResp, ip)
	if err != nil {
		//slog.Error("[PostJson]", "err", err)
		return
	}
	if eResp.Code != 200 {
		err = fmt.Errorf("err:%s", eResp.Msg)
		return
	}
	err = Decode(eResp.Data, key, &resp)
	return
}

type RespPi struct {
	Port  uint16              `json:"port"`  //监听端口
	Ip    string              `json:"ip"`    //监听ip
	Users map[string]UserInfo `json:"users"` //用户
}

func (s *ProxyInfo) GetKey() string {
	return s.Ip
}

func IsBlackSuffixes(domain string) bool {
	for _, v := range _blackDomainSuffixes {
		if strings.HasSuffix(domain, v) {
			return true
		}
	}
	return false
}

var (
	_blackDomainSuffixes []string
	_srcWhiteList        map[string]struct{}
	_accessMode          uint8
)

func CanAccess(ip string) bool {
	if len(_srcWhiteList) > 0 {
		if _, ok := _srcWhiteList[ip]; ok {
			return true
		}
	}
	switch _accessMode {
	case 1:
		return !utils.IsMainlandChina(ip)
	case 2:
		return utils.IsMainlandChina(ip)
	}
	return true
}

var DbIns *JDB

type JDB struct {
	file *os.File
	data map[string]*ProxyInfo
	rwm  *sync.RWMutex
}

func Init(filename string, srcWhiteList, blackDomainSuffixes []string, accessMode uint8) error {
	_blackDomainSuffixes = blackDomainSuffixes
	_accessMode = accessMode
	_srcWhiteList = make(map[string]struct{}, 0)
	for _, v := range srcWhiteList {
		_srcWhiteList[v] = struct{}{}
	}

	f, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE, os.ModePerm)
	if err != nil {
		slog.Error("openfile", "err", err)
		return err
	}
	DbIns = &JDB{
		file: f,
		rwm:  &sync.RWMutex{},
	}

	fi, err := f.Stat()
	if err != nil {
		slog.Error("Stat", "err", err)
		return err
	}
	if fi.Size() == 0 {
		DbIns.data = make(map[string]*ProxyInfo, 0)
		return nil
	}

	if err := DbIns.unmarshal(); err != nil {
		slog.Error("unmarshal", "err", err)
		return err
	}
	return nil
}

func (s *JDB) dump() error {
	if len(s.data) > 0 {
		s.file.Truncate(0)
		s.file.Seek(0, 0)
		data, err := json.Marshal(s.data)
		if err != nil {
			return err
		}
		s.file.Write(data)
	}
	return nil
}

func (s *JDB) unmarshal() error {
	s.rwm.Lock()
	defer s.rwm.Unlock()

	data, err := os.ReadFile(s.file.Name())
	if err != nil {
		return err
	}

	if len(data) == 0 {
		s.data = make(map[string]*ProxyInfo, 0)
		return nil
	}

	err = json.Unmarshal(data, &s.data)
	if err != nil {
		return err
	}

	return nil
}

func (s *JDB) SyncPut(pi *ProxyInfo) error {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	pi.UpdatedAt = time.Now().Unix()
	s.data[pi.GetKey()] = pi
	return s.dump()
}

func (s *JDB) BatchSyncPut(ips *map[string]*ProxyInfo) error {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	for _, v := range *ips {
		s.data[v.GetKey()] = v
	}
	return s.dump()
}

func (s *JDB) Sync() {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	s.dump()
}

func (s *JDB) Get(key string) *ProxyInfo {
	s.rwm.RLock()
	defer s.rwm.RUnlock()
	return s.data[key]
}

func (s *JDB) Contains(key string) bool {
	_, ok := s.data[key]
	return ok
}

func (s *JDB) Put(pi *ProxyInfo) error {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	pi.UpdatedAt = time.Now().Unix()
	s.data[pi.GetKey()] = pi
	return nil
}

func (s *JDB) All() map[string]*ProxyInfo {
	s.rwm.RLock()
	defer s.rwm.RUnlock()
	return s.data
}

func (s *JDB) Clear() {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	for k, _ := range s.data {
		delete(s.data, k)
	}
}

func (s *JDB) Close() {
	s.data = nil
	if s.file != nil {
		s.file.Close()
	}
}
