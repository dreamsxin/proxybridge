package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baowk/bridge-direct/config"
	"github.com/baowk/bridge-direct/server/dto"
)

func TestMetricsHandlerExposesRuntimeAndBridgeMetrics(t *testing.T) {
	setupServerTest(t)
	target := startTCPServer(t, "metrics")
	bridgePort := freeTCPPort(t)
	if resp := performAddBridge(t, dto.UseBridge{BridgePort: bridgePort, Ip: target.host, Port: target.port}); resp.Code != 200 {
		t.Fatalf("add failed: %+v", resp)
	}

	previousID := config.Cfg.BridgeId
	config.Cfg.BridgeId = 42
	t.Cleanup(func() { config.Cfg.BridgeId = previousID })
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, body=%q", rec.Code, rec.Body.String())
	}
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, metric := range []string{
		"bridge_proxies_configured",
		"bridge_listeners",
		"bridge_listener_up",
		"bridge_listener_connections_active",
		"bridge_runtime_goroutines",
		"bridge_connection_limit",
	} {
		if !strings.Contains(text, metric) {
			t.Fatalf("metrics output missing %q:\n%s", metric, text)
		}
	}
	if !strings.Contains(text, `bridge_id="42"`) || !strings.Contains(text, `bridge_port="`+formatPort(bridgePort)+`"`) {
		t.Fatalf("metrics output missing bridge labels:\n%s", text)
	}
}
