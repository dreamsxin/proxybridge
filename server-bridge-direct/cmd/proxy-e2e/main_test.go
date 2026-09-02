package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
