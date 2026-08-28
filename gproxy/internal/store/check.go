package store

import (
	"crypto/md5"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/baowk/gproxy/configs"
	"github.com/baowk/gproxy/internal/enums"
)

func Check(wg *sync.WaitGroup, cfg *configs.Config) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	done := true
	ver := enums.VersionToNum()
	key, _ := cfg.GetKey()
	for {
		<-t.C
		//slog.Info("[check]", "curver", ver)
		resp, err := proxyCheck(enums.AppCode, ver, cfg.AccessMode, cfg.Os, cfg.Arch, key, cfg.GetCenterDomain())
		if err != nil {
			slog.Error("[check]", "ProxyCheck", err)
			t.Reset(time.Minute)
		} else {
			t.Reset(time.Hour * 2)
			if len(resp.BWIps) > 0 {
				for k, v := range resp.BWIps {
					if v == 1 {
						_srcWhiteList[k] = struct{}{}
					}
				}
			}

			_blackDomainSuffixes = resp.DialBlacks

			if resp.LogAddr != "" {
				cfg.UploadLog.Addr = resp.LogAddr
			}

			if resp.Version.Ver > 0 {
				ds := time.Now().Format("0102")

				newfile := enums.AppCode + "_new_" + ds
				if err := downloadFile(resp.Version.DownloadUrl, newfile); err != nil {
					slog.Error("[check]", "download", err, "url", resp.Version.DownloadUrl, "ver", resp.Version.Ver)
					if done {
						done = false
						wg.Done()
					}
					continue
				}
				if done {
					done = false
					wg.Done()
				}
				data, err := calculateMD5(newfile)
				if err != nil {
					slog.Error("[check]", "calculateMD5", err, "ver", resp.Version.Ver)
					continue
				}

				locald := fmt.Sprintf("%x", data)
				if locald != resp.Version.Md5sum {
					slog.Error("[check]", "md5sum server", resp.Version.Md5sum, "local", locald, "ver", resp.Version.Ver)
					continue
				}

				if err := os.Chmod(newfile, 0755); err != nil {
					slog.Error("[check]", "Chmod", err, "ver", resp.Version.Ver)
					continue
				}

				if err := os.Rename(enums.AppCode, enums.AppCode+"_bak_"+ds); err != nil {
					slog.Error("[check]", "rename old", err)
					continue
				}

				if err := os.Rename(newfile, enums.AppCode); err != nil {
					slog.Error("[check]", "rename old", err)
					continue
				}
				slog.Info("[check]", "success", resp)
				os.Exit(0)
			} else {
				slog.Info("[check]", "lastver", resp)
			}
		}
		if done {
			done = false
			wg.Done()
		}
	}
}

func downloadFile(url string, filepath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}

	return nil
}

func calculateMD5(filepath string) ([]byte, error) {
	f, err := os.Open(filepath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}

	return h.Sum(nil), nil
}

// type AuthErrIp struct {
// 	Username  string `json:"username"`
// 	Pwd       string `json:"pwd"`
// 	Num       int    `json:"num"`
// 	LastTime  int64  `json:"lastTime"`
// 	FirstTime int64  `json:"firstTime"`
// }

// func AddAuthErrIp(ip string, username, pwd string) {
// 	slog.Debug("[AddAuthErrIp]", "ip", ip, username, pwd)
// 	//白名单 不添加访问控制
// 	if v, ok := bwIps[ip]; ok {
// 		if v == 1 {
// 			slog.Debug("[AddAuthErrIp]", "white", ip)
// 			return
// 		}
// 	}
// 	slog.Debug("[AddAuthErrIp]", "not whiles ip", ip)
// 	if aip, ok := authErrIP.Get(ip); ok {
// 		aei := aip.(AuthErrIp)
// 		slog.Debug("[AddAuthErrIp]", ip, aei, username, pwd)
// 		if aei.Username != username || aei.Pwd != pwd {
// 			aei.Username = username
// 			aei.Pwd = pwd
// 			aei.LastTime = time.Now().Unix()
// 			aei.Num++
// 			authErrIP.Set(ip, aei, time.Minute*10)
// 			slog.Debug("[AddAuthErrIp]", "ip", ip, "username", username, "pwd", pwd, "num", aei.Num)
// 		}
// 	} else {
// 		aei := AuthErrIp{
// 			Num:       1,
// 			FirstTime: time.Now().Unix(),
// 			Username:  username,
// 			Pwd:       pwd,
// 		}
// 		aei.LastTime = aei.FirstTime
// 		authErrIP.Set(ip, aei, time.Minute*10)
// 		slog.Debug("[AddAuthErrIp]", "add ip first", ip, "username", username, "pwd", pwd, "num", aei.Num)
// 	}
// }

// func IsAuthErrIp(ip string) bool {
// 	// //白名单
// 	// if v, ok := bwIps[ip]; ok {
// 	// 	if v == 1 {
// 	// 		return false
// 	// 	}
// 	// }
// 	if aip, ok := authErrIP.Get(ip); ok {
// 		aei := aip.(AuthErrIp)
// 		if aei.Num > 150 {
// 			return true
// 		} else if aei.LastTime-aei.FirstTime < 60 && aei.Num > 20 {
// 			return true
// 		} else if aei.LastTime-aei.FirstTime < 120 && aei.Num > 50 {
// 			return true
// 		} else if aei.LastTime-aei.FirstTime < 200 && aei.Num > 100 {
// 			return true
// 		}
// 	}
// 	return false
// }

// func GetIp(addr net.Addr) string {
// 	return GetIpString(addr.String())
// }

// func GetIpString(addr string) string {
// 	var ip string
// 	if strings.Contains(addr, ".") {
// 		arr := strings.Split(addr, ":")
// 		ip = arr[0]
// 	} else { //ipv6
// 		ip = addr[1:strings.Index(addr, "]:")]
// 	}
// 	return ip
// }
