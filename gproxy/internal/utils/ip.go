package utils

import (
	"net"
	"net/netip"

	"github.com/oschwald/maxminddb-golang/v2"
)

var (
	db *maxminddb.Reader
)

func Init(path string) error {
	var err error
	db, err = maxminddb.Open(path)
	return err
}

func GetIpInfo(ipStr string) (*IpRecord, error) {
	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return nil, err
	}

	var record IpRecord

	err = db.Lookup(ip).Decode(&record)
	return &record, err
}

func IsMainlandChina(ip string) bool {
	//slog.Info("GetIpInfo", "ip", ip)
	i, err := GetIpInfo(ip)
	if err != nil {
		return false
	}
	return i.IsMainlandChina()
}

func CloseDB() {
	db.Close()
}

type IpRecord struct {
	Country struct {
		ISOCode string            `maxminddb:"iso_code"`
		Names   map[string]string `maxminddb:"names"`
	} `maxminddb:"country"`
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
}

func (i *IpRecord) IsMainlandChina() bool {
	//slog.Info("GetIpInfo", "ISOCode", i.Country.ISOCode)
	return i.Country.ISOCode == "CN"
}

func (i *IpRecord) GetCountryEn() string {
	return i.Country.Names["en"]
}
func (i *IpRecord) GetCountryDe() string {
	return i.Country.Names["de"]
}

func (i *IpRecord) GetCountryEs() string {
	return i.Country.Names["es"]
}

func (i *IpRecord) GetCountryFr() string {
	return i.Country.Names["fr"]
}
func (i *IpRecord) GetCountryJa() string {
	return i.Country.Names["ja"]
}

func (i *IpRecord) GetCountryRu() string {
	return i.Country.Names["ru"]
}
func (i *IpRecord) GetCountryZh() string {
	return i.Country.Names["zh-CN"]
}

func (i *IpRecord) GetCountryPtBr() string {
	return i.Country.Names["pt-BR"]
}

func (i *IpRecord) GetCityEn() string {
	return i.City.Names["en"]
}
func (i *IpRecord) GetCityDe() string {
	return i.City.Names["de"]
}

func (i *IpRecord) GetCityEs() string {
	return i.City.Names["es"]
}

func (i *IpRecord) GetCityFr() string {
	return i.City.Names["fr"]
}
func (i *IpRecord) GetCityJa() string {
	return i.City.Names["ja"]
}

func (i *IpRecord) GetCityRu() string {
	return i.City.Names["ru"]
}
func (i *IpRecord) GetCityZh() string {
	return i.City.Names["zh-CN"]
}

func (i *IpRecord) GetCityPtBr() string {
	return i.City.Names["pt-BR"]
}

func GetIps() (ipv4s []string, ipv6s []string, err error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return
	}
	for _, i := range interfaces {
		addrs, err := i.Addrs()
		if err != nil {
			panic(err)
		}
		// handle err
		for _, addr := range addrs {
			switch v := addr.(type) {
			case *net.IPNet:
				if !v.IP.IsLoopback() {
					if v.IP.To4() != nil {
						ipv4s = append(ipv4s, v.IP.String())
					} else if v.IP.To16() != nil {
						ipv6s = append(ipv6s, v.IP.String())
					}
				}
			case *net.IPAddr:
				if !v.IP.IsLoopback() {
					if v.IP.To4() != nil {
						ipv4s = append(ipv4s, v.IP.String())
					} else if v.IP.To16() != nil {
						ipv6s = append(ipv6s, v.IP.String())
					}
				}
			}

		}
	}
	return
}

func GetIPv4(ifname string) string {
	a, err := net.InterfaceByName(ifname)
	if err != nil {
		//fmt.Println(err)
		return ""
	}
	addr, _ := a.Addrs()
	for _, address := range addr {
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

func GetAddr(ifname string) net.Addr {
	a, err := net.InterfaceByName(ifname)
	if err != nil {
		//fmt.Println(err)
		return nil
	}
	addrs, _ := a.Addrs()
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return addr
			}
		}
	}
	return nil
}
