// #file:/root/works/gos/gproxy/cmd/main.go
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	// _ "net/http/pprof"

	"github.com/baowk/gproxy/configs"
	"github.com/baowk/gproxy/internal/enums"
	"github.com/baowk/gproxy/internal/logger"
	"github.com/baowk/gproxy/internal/server"
	"github.com/baowk/gproxy/internal/store"
	"github.com/baowk/gproxy/internal/utils"
)

func main() {
	// Define command-line flags
	configFile := flag.String("c", "configs/config.json", "Path to configuration file")
	flag.Parse()

	// Load configuration
	cfg, err := loadConfig(*configFile)
	if err != nil {
		log.Printf("Error loading configs file: %v, using default configs", err)
		cfg = configs.DefaultConfig()
	}
	configs.Cfg = cfg
	utils.Init(cfg.GeoDb)

	tcpTimeout := time.Duration(cfg.TcpTimeout) * time.Second
	udpTimeout := time.Duration(cfg.UdpTimeout) * time.Second

	logger.InitLog(&cfg.Log)
	fmt.Printf("[%s] starting, version:%s\n", enums.AppCode, enums.Version)
	slog.Info("server starting", "app", enums.AppCode, "version", enums.Version)

	// go func() {
	// 	fmt.Println("Starting pprof server...")
	// 	http.ListenAndServe(":6060", nil)
	// }()

	// if cfg.IsRemote() {
	// 	go server.NewHttpServer(cfg)
	// }

	var ipv4s, ipv6s []string
	if cfg.IpScan {
		ipv4s, ipv6s, err = utils.GetIps()
		if err != nil {
			log.Fatalf("Error getting IPs: %v", err)
		}
	}

	if err = store.Init(cfg.GetDBName(), cfg.SrcWhiteList, cfg.BlackDomainSuffixes, cfg.AccessMode); err != nil {
		log.Fatalf("Error init db: %v", err)
	}

	if cfg.IsRemote() {
		wg := &sync.WaitGroup{}
		wg.Add(1)
		go store.Check(wg, cfg)
		wg.Wait()
	}

	for _, ip := range ipv4s {
		if !store.DbIns.Contains(ip) {
			pi := newProxyInfo(ip, cfg)
			store.DbIns.Put(pi)
		}
	}

	if cfg.Ipv6 {
		for _, ip := range ipv6s {
			if !store.DbIns.Contains(ip) {
				pi := newProxyInfo(ip, cfg)
				store.DbIns.Put(pi)
			}
		}
	}
	for _, pi := range store.DbIns.All() {
		go run(pi, tcpTimeout, udpTimeout)
	}

	go store.DbIns.Sync()

	if cfg.UploadLog.Enable {
		key, err := cfg.GetKey()
		if err != nil {
			panic(err)
		}
		go logger.UploadLogs(cfg.Log.Output, key, cfg.GetUploadLogAddr())
	}

	// Handle graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	store.DbIns.Sync()
	log.Println("Shutting down server...")
	os.Exit(0)
}

func newProxyInfo(ip string, cfg *configs.Config) *store.ProxyInfo {
	pi := &store.ProxyInfo{
		Ip: ip,
	}
	if cfg.Port > 0 {
		pi.Port = cfg.Port
	} else {
		pi.Port = uint16(rand.IntN(55535)) + 5000
	}
	if cfg.Username != "" && cfg.Password != "" {
		pi.Users = map[string]*store.UserInfo{
			cfg.Username: {
				Password: cfg.Password,
			},
		}
	} else {
		u := utils.RandStringByLen(12)
		p := utils.RandStringByLen(12)
		pi.Users = map[string]*store.UserInfo{
			u: {
				Password: p,
			},
		}
	}
	// pi.Users = sync.Map{}

	// if cfg.Username != "" && cfg.Password != "" {
	// 	pi.Users.Store(cfg.Username, store.UserInfo{
	// 		Password: cfg.Password,
	// 	})
	// } else {
	// 	u := utils.RandStringByLen(12)
	// 	p := utils.RandStringByLen(12)
	// 	pi.Users.Store(u, store.UserInfo{
	// 		Password: p,
	// 	})
	// }
	return pi
}

func run(pi *store.ProxyInfo, tcpTimeout, udpTimeout time.Duration) error {
	// if pi.OutIp == "" {
	// 	pi.OutIp = pi.Ip
	// }
	// Start server
	err := server.NewServer(pi, tcpTimeout, udpTimeout)
	if err != nil {
		log.Fatalf("Error creating server: %v", err)
	}

	// err = srv.ListenAndServe(nil)
	// if err != nil {
	// 	return err
	// }
	// server.Servers[pi.GetKey()] = srv
	return nil
}

func loadConfig(filename string) (*configs.Config, error) {
	// If configs file doesn't exist, create a default one
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		cfg := configs.DefaultConfig()
		data, _ := json.MarshalIndent(cfg, "", "  ")
		os.WriteFile(filename, data, 0644)
		return cfg, nil
	}

	// Read configs file
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	// Parse JSON
	var cfg configs.Config
	err = json.Unmarshal(data, &cfg)
	if err != nil {
		return nil, err
	}

	return &cfg, nil
}
