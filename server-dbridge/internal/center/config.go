package center

import (
	"sync"
	"time"
)

// BridgeConfig holds limits fetched from the central server.
type BridgeConfig struct {
	MaxProxies        int     // max active upstream proxies
	MaxConnsPerProxy  int     // max concurrent connections per proxy
	DefaultSpeedLimit int64   // bytes/s per proxy, used when API returns 0
}

var defaultConfig = BridgeConfig{
	MaxProxies:       100,
	MaxConnsPerProxy: 200,
	DefaultSpeedLimit: 5 * 1024 * 1024, // 5 MB/s
}

type configManager struct {
	mu      sync.RWMutex
	current BridgeConfig
	client  *Client
}

var globalConfigManager *configManager

// InitConfig initializes the config manager and does the first fetch.
// Returns error if the central server is unreachable.
func InitConfig(c *Client) error {
	globalConfigManager = &configManager{client: c}
	if err := globalConfigManager.fetch(); err != nil {
		return err
	}
	go globalConfigManager.loop()
	return nil
}

// GetConfig returns the current bridge configuration.
func GetConfig() BridgeConfig {
	globalConfigManager.mu.RLock()
	defer globalConfigManager.mu.RUnlock()
	return globalConfigManager.current
}

func (m *configManager) fetch() error {
	cfg, err := m.client.FetchConfig()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.current = cfg
	m.mu.Unlock()
	return nil
}

func (m *configManager) loop() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		_ = m.fetch() // on failure keep last known config
	}
}
