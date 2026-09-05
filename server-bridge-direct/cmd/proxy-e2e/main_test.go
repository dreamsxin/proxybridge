package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baowk/bridge-direct/server/dto"
	"github.com/baowk/bridge-direct/utils"
)

func TestLoadProxiesFromCSVFirstColumn(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "proxies.csv")
	content := "proxy,unused\n\"socks5://user%40name:pass%21@198.51.100.10:1080\",x\n\"http://http-user:http-pass@198.51.100.11:8080\",x\n"
	if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	proxies, err := loadProxies(filename)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 2 {
		t.Fatalf("loaded %d proxies, want 2", len(proxies))
	}
	p := proxies[0]
	if p.Host != "198.51.100.10" || p.Port != 1080 || p.Username != "user@name" || p.Password != "pass!" {
		t.Fatalf("parsed proxy = %+v", p)
	}
	if proxies[1].Scheme != "http" || proxies[1].Host != "198.51.100.11" || proxies[1].Port != 8080 {
		t.Fatalf("parsed HTTP proxy = %+v", proxies[1])
	}
}

func TestLoadProxiesFromText(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "proxies.txt")
	content := "\ufeff# comment\n\nsocks5://198.51.100.11:1081\n198.51.100.12:1082\n"
	if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	proxies, err := loadProxies(filename)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 2 {
		t.Fatalf("loaded %d proxies, want 2", len(proxies))
	}
	if proxies[1].Scheme != "socks5" || proxies[1].Host != "198.51.100.12" || proxies[1].Port != 1082 {
		t.Fatalf("parsed bare proxy = %+v", proxies[1])
	}
}

func TestParseProxyAcceptsHostnamesForDNSResolution(t *testing.T) {
	p, err := parseProxy("socks5://user:pass@example.com:1080")
	if err != nil {
		t.Fatal(err)
	}
	if p.Host != "example.com" || p.Port != 1080 {
		t.Fatalf("parsed hostname proxy = %+v", p)
	}
}

func TestNormalizeBridgeURL(t *testing.T) {
	got, parsed, err := normalizeBridgeURL("10.0.0.8:5678/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://10.0.0.8:5678" || parsed.Hostname() != "10.0.0.8" {
		t.Fatalf("normalized URL = %q parsed=%+v", got, parsed)
	}
	if _, _, err := normalizeBridgeURL("http://10.0.0.8:5678/?token=secret"); err == nil {
		t.Fatal("expected query to be rejected")
	}
}

func TestRemoteBridgeCallUsesConfiguredURLAndKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bridge/add" {
			t.Errorf("request path = %q", r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var req dto.Req
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		ciphertext, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			t.Errorf("decode data: %v", err)
			return
		}
		plaintext, err := utils.AesDecryptCBC(ciphertext, []byte(testKey))
		if err != nil {
			t.Errorf("decrypt data: %v", err)
			return
		}
		var bridge dto.UseBridge
		if err := json.Unmarshal(plaintext, &bridge); err != nil {
			t.Errorf("decode bridge: %v", err)
			return
		}
		if bridge.BridgePort != 30000 || bridge.Ip != "198.51.100.10" || bridge.Port != 1080 {
			t.Errorf("bridge payload = %+v", bridge)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":200,"msg":"ok"}`)
	}))
	defer server.Close()

	bridge, err := newRemoteBridge(server.URL+"/api/", testKey, "127.0.0.1", 30000)
	if err != nil {
		t.Fatal(err)
	}
	res, err := bridge.call("/bridge/add", dto.UseBridge{BridgePort: 30000, Ip: "198.51.100.10", Port: 1080})
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != 200 {
		t.Fatalf("response = %+v", res)
	}
}

func TestRemoteBridgePortPlan(t *testing.T) {
	bridge := &bridgeProc{remote: true, portStart: 30000}
	for index, want := range map[int]int{1: 30000, 2: 30001, 10: 30009} {
		got, err := bridge.allocatePort(index)
		if err != nil || got != want {
			t.Fatalf("allocatePort(%d) = %d, %v; want %d", index, got, err, want)
		}
	}
	if err := validateBridgePortRange(65535, 2); err == nil {
		t.Fatal("expected bridge port range overflow")
	}
}

func TestHTTPConnectHandshakeUsesProxyCredentials(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	proxy := proxySpec{Scheme: "http", Username: "user", Password: "pass"}
	done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(server)
		request, err := httpReadHeader(reader)
		if err != nil {
			done <- err
			return
		}
		if !strings.HasPrefix(request, "CONNECT myip.ipipv.com:80 HTTP/1.1\r\n") {
			done <- fmt.Errorf("unexpected CONNECT request: %q", request)
			return
		}
		wantAuth := "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
		if !strings.Contains(request, wantAuth+"\r\n") {
			done <- fmt.Errorf("missing proxy authorization in %q", request)
			return
		}
		_, err = io.WriteString(server, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done <- err
	}()
	if err := httpConnectHandshake(client, proxy); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func httpReadHeader(reader *bufio.Reader) (string, error) {
	var builder strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		builder.WriteString(line)
		if strings.TrimSpace(line) == "" {
			return builder.String(), nil
		}
	}
}
