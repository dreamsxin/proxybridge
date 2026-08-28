package center

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client communicates with the central server.
type Client struct {
	baseURL      string
	bridgeID     uint
	bridgeSecret string
	httpClient   *http.Client
}

func NewClient(baseURL string, bridgeID uint, bridgeSecret string) *Client {
	return &Client{
		baseURL:      baseURL,
		bridgeID:     bridgeID,
		bridgeSecret: bridgeSecret,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// authReq is sent to /api/open/bridge/auth
type authReq struct {
	BridgeID     uint   `json:"bridge_id"`
	BridgeSecret string `json:"bridge_secret"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	ClientIP     string `json:"client_ip"`
}

// AuthRequest is the caller-facing request (no bridge credentials needed).
type AuthRequest struct {
	Username string
	Password string
	ClientIP string
}

// AuthResponse is returned by the auth endpoint.
type AuthResponse struct {
	Protocol   string `json:"protocol"`    // "http" or "socks5"
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	ProxyUser  string `json:"proxy_user"`
	ProxyPass  string `json:"proxy_pass"`
	SpeedLimit int64  `json:"speed_limit"` // bytes/s, 0 = use default
}

// ConfigResponse is returned by the config endpoint.
type ConfigResponse struct {
	MaxProxies       int   `json:"max_proxies"`
	MaxConnsPerProxy int   `json:"max_conns_per_proxy"`
	DefaultSpeed     int64 `json:"default_speed"` // bytes/s
}

// bridgeReq is the base request carrying bridge identity.
type bridgeReq struct {
	BridgeID     uint   `json:"bridge_id"`
	BridgeSecret string `json:"bridge_secret"`
}

// reportReq mirrors center.ReportStats with bridge credentials.
type reportReq struct {
	BridgeID      uint   `json:"bridge_id"`
	BridgeSecret  string `json:"bridge_secret"`
	ActiveProxies int    `json:"active_proxies"`
	TotalConns    int    `json:"total_conns"`
	BandwidthBps  int64  `json:"bandwidth_bps"`
}

// Authenticate calls the auth API and returns the upstream proxy info.
func (c *Client) Authenticate(ctx context.Context, req AuthRequest) (*AuthResponse, error) {
	body, err := c.postJSON(ctx, "/api/open/bridge/auth", authReq{
		BridgeID:     c.bridgeID,
		BridgeSecret: c.bridgeSecret,
		Username:     req.Username,
		Password:     req.Password,
		ClientIP:     req.ClientIP,
	})
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		Code int          `json:"code"`
		Msg  string       `json:"msg"`
		Data *AuthResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("auth decode: %w", err)
	}
	if wrapper.Code != 200 || wrapper.Data == nil {
		return nil, fmt.Errorf("auth failed: %s", wrapper.Msg)
	}
	return wrapper.Data, nil
}

// FetchConfig retrieves bridge limits from the central server.
func (c *Client) FetchConfig() (BridgeConfig, error) {
	body, err := c.postJSON(context.Background(), "/api/open/bridge/config", bridgeReq{
		BridgeID:     c.bridgeID,
		BridgeSecret: c.bridgeSecret,
	})
	if err != nil {
		return BridgeConfig{}, err
	}

	var wrapper struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data *ConfigResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return BridgeConfig{}, fmt.Errorf("config decode: %w", err)
	}
	if wrapper.Code != 200 || wrapper.Data == nil {
		return BridgeConfig{}, fmt.Errorf("config fetch failed: %s", wrapper.Msg)
	}

	return BridgeConfig{
		MaxProxies:        wrapper.Data.MaxProxies,
		MaxConnsPerProxy:  wrapper.Data.MaxConnsPerProxy,
		DefaultSpeedLimit: wrapper.Data.DefaultSpeed,
	}, nil
}

// Report sends the current bridge stats to the central server.
func (c *Client) Report(ctx context.Context, stats ReportStats) error {
	_, err := c.postJSON(ctx, "/api/open/bridge/report", reportReq{
		BridgeID:      c.bridgeID,
		BridgeSecret:  c.bridgeSecret,
		ActiveProxies: stats.ActiveProxies,
		TotalConns:    stats.TotalConns,
		BandwidthBps:  stats.BandwidthBps,
	})
	return err
}

func (c *Client) postJSON(ctx context.Context, path string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, jsonReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("center server unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("center server returned %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
