package server

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/baowk/bridge-direct/cachef"
	"github.com/baowk/bridge-direct/config"
	"github.com/baowk/bridge-direct/server/dto"
	"github.com/baowk/bridge-direct/utils"
	"github.com/gin-gonic/gin"
)

const testKey = "abcd1234poiu5678bvbvnbnb"

func TestAddBridgeUpdatesExistingBridgePort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetListeners(t)
	t.Cleanup(func() {
		resetListeners(t)
	})
	config.Cfg.Key = testKey

	var err error
	cf, err = cachef.New(t.TempDir() + "/bridge.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)

	firstTarget := startTCPServer(t, "first")
	secondTarget := startTCPServer(t, "second")
	bridgePort := freeTCPPort(t)

	performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort,
		Ip:         firstTarget.host,
		Port:       firstTarget.port,
	})
	assertProxyResponse(t, bridgePort, "first")

	performAddBridge(t, dto.UseBridge{
		BridgePort: bridgePort,
		Ip:         secondTarget.host,
		Port:       secondTarget.port,
	})
	assertProxyResponse(t, bridgePort, "second")
}

func TestAddBridgeRejectsInvalidIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetListeners(t)
	t.Cleanup(func() {
		resetListeners(t)
	})
	config.Cfg.Key = testKey

	var err error
	cf, err = cachef.New(t.TempDir() + "/bridge.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)

	resp := performAddBridge(t, dto.UseBridge{
		BridgePort: freeTCPPort(t),
		Ip:         "not-an-ip",
		Port:       8080,
	})
	if resp.Code == 200 {
		t.Fatalf("expected invalid ip failure, got %+v", resp)
	}
}

type tcpTarget struct {
	host string
	port uint16
}

func performAddBridge(t *testing.T, bridge dto.UseBridge) dto.Res {
	t.Helper()
	body, err := json.Marshal(bridge)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := utils.AesEncryptCBCStr(string(body), testKey)
	if err != nil {
		t.Fatal(err)
	}
	reqBody, err := json.Marshal(dto.Req{
		Ver:       "1",
		Timestamp: time.Now().Unix(),
		Data:      encrypted,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/bridge/add", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	AddBridge(c)

	var resp dto.Res
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response %q: %v", rec.Body.String(), err)
	}
	return resp
}

func startTCPServer(t *testing.T, response string) tcpTarget {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.Write([]byte(response))
			}()
		}
	}()
	host, portString, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := net.LookupPort("tcp", portString)
	if err != nil {
		t.Fatal(err)
	}
	return tcpTarget{host: host, port: uint16(port)}
}

func freeTCPPort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, portString, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := net.LookupPort("tcp", portString)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(port)
}

func assertProxyResponse(t *testing.T, port uint16, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", formatPort(port)), 100*time.Millisecond)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		buf := make([]byte, len(want))
		conn.SetReadDeadline(time.Now().Add(time.Second))
		_, err = conn.Read(buf)
		conn.Close()
		if err == nil && string(buf) == want {
			return
		}
		t.Fatalf("got %q, want %q, err=%v", string(buf), want, err)
	}
	t.Fatalf("proxy on port %d did not respond with %q", port, want)
}

func formatPort(port uint16) string {
	return strconv.FormatUint(uint64(port), 10)
}

func resetListeners(t *testing.T) {
	t.Helper()
	runMu.Lock()
	listeners := make([]*bridgeListener, 0, len(runListens))
	for port, listener := range runListens {
		delete(runListens, port)
		listeners = append(listeners, listener)
	}
	runMu.Unlock()
	for _, listener := range listeners {
		listener.stop()
		<-listener.done
	}
}
