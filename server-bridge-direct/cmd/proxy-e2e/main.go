// Command proxy-e2e runs a repeatable bridge + proxy list end-to-end test.
//
// Each round adds one bridge port per proxy, sends concurrent requests through
// every successfully added bridge, then deletes all bridge ports before the
// next round. Proxy files may be one URL per line or CSV with the URL in the
// first column.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baowk/bridge-direct/server/dto"
	"github.com/baowk/bridge-direct/utils"
)

const (
	testKey    = "abcd1234poiu5678bvbvnbnb"
	targetURL  = "http://myip.ipipv.com/"
	targetHost = "myip.ipipv.com"
)

var (
	flagRounds           = flag.Int("rounds", 1, "number of add/request/delete rounds")
	flagProxyFile        = flag.String("proxy-file", "", "proxy list file: one URL per line or CSV first column")
	flagConcurrency      = flag.Int("concurrency", 10, "maximum concurrent requests")
	flagRequestsPerProxy = flag.Int("requests-per-proxy", 2, "requests sent through each proxy per round")
	flagTimeout          = flag.Duration("request-timeout", 25*time.Second, "timeout for one HTTP request")
	flagBridgeBin        = flag.String("bridge-bin", "", "bridge-direct binary; when empty, build the current module")
	flagBridgeURL        = flag.String("bridge-url", "", "bridge management API URL; empty starts a local bridge, e.g. http://10.0.0.8:5678")
	flagBridgeKey        = flag.String("bridge-key", "", "AES key for the bridge management API; required with -bridge-url")
	flagBridgeHost       = flag.String("bridge-host", "", "host where bridge listener ports can be reached; remote mode defaults to the management API host")
	flagBridgePortStart  = flag.Int("bridge-port-start", 0, "first bridge listener port; required for remote mode, then one port per proxy")
	flagReport           = flag.String("report", "", "optional JSON report output path")
	flagVerbose          = flag.Bool("verbose", false, "print per-proxy add/request/delete details")
	flagDryRun           = flag.Bool("dry-run", false, "only parse and validate the proxy file")
)

type proxySpec struct {
	Raw      string `json:"raw"`
	Scheme   string `json:"scheme"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func (p proxySpec) label() string {
	return net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

type stageError struct {
	Class string
	Err   error
}

func (e *stageError) Error() string { return e.Class + ": " + e.Err.Error() }
func (e *stageError) Unwrap() error { return e.Err }

type proxyState struct {
	Proxy      proxySpec
	Index      int
	BridgePort int
	BridgeHost string
	Added      bool
	ExpectedIP string
	Mu         sync.Mutex
}

type requestJob struct {
	State *proxyState
	Index int
}

type requestResult struct {
	State   *proxyState
	Index   int
	Class   string
	IP      string
	Err     error
	Elapsed time.Duration
}

type roundReport struct {
	Round            int                 `json:"round"`
	ProxyCount       int                 `json:"proxyCount"`
	AddOK            int                 `json:"addOK"`
	AddFailed        int                 `json:"addFailed"`
	RequestTotal     int                 `json:"requestTotal"`
	RequestOK        int                 `json:"requestOK"`
	DeleteOK         int                 `json:"deleteOK"`
	DeleteFailed     int                 `json:"deleteFailed"`
	DeleteVerifyFail int                 `json:"deleteVerifyFailed"`
	Classes          map[string]int      `json:"classes"`
	Samples          []string            `json:"samples,omitempty"`
	ProxyIPs         map[string][]string `json:"proxyIPs,omitempty"`
}

type testReport struct {
	ProxyFile        string        `json:"proxyFile"`
	BridgeURL        string        `json:"bridgeURL,omitempty"`
	BridgeHost       string        `json:"bridgeHost,omitempty"`
	RemoteBridge     bool          `json:"remoteBridge"`
	Rounds           int           `json:"rounds"`
	Concurrency      int           `json:"concurrency"`
	RequestsPerProxy int           `json:"requestsPerProxy"`
	StartedAt        time.Time     `json:"startedAt"`
	FinishedAt       time.Time     `json:"finishedAt"`
	ProxyCount       int           `json:"proxyCount"`
	RoundReports     []roundReport `json:"roundReports"`
}

func main() {
	flag.Parse()
	fmt.Printf("proxy-e2e starting: proxyFile=%s rounds=%d concurrency=%d requestsPerProxy=%d bridgeURL=%s\n", *flagProxyFile, *flagRounds, *flagConcurrency, *flagRequestsPerProxy, *flagBridgeURL)
	if *flagProxyFile == "" {
		fatalf("-proxy-file is required")
	}
	if *flagRounds <= 0 || *flagConcurrency <= 0 || *flagRequestsPerProxy <= 0 {
		fatalf("rounds, concurrency and requests-per-proxy must be positive")
	}
	proxies, err := loadProxies(*flagProxyFile)
	if err != nil {
		fatalf("load proxy file: %v", err)
	}
	if len(proxies) == 0 {
		fatalf("proxy file contains no valid proxies")
	}
	fmt.Printf("proxy-e2e loaded %d valid proxies\n", len(proxies))
	if *flagDryRun {
		fmt.Printf("proxy file %s: %d valid entries\n", *flagProxyFile, len(proxies))
		for i, p := range proxies {
			if i >= 10 {
				fmt.Printf("... and %d more\n", len(proxies)-i)
				break
			}
			fmt.Printf("%d: %s auth=%t\n", i+1, p.label(), p.Username != "" || p.Password != "")
		}
		return
	}

	bridge, err := prepareBridge(len(proxies))
	if err != nil {
		fatalf("prepare bridge: %v", err)
	}
	defer bridge.Close()
	fmt.Printf("proxy-e2e bridge management API ready at %s listenerHost=%s remote=%t\n", bridge.adminURL, bridge.bridgeHost, bridge.remote)

	report := testReport{
		ProxyFile:        *flagProxyFile,
		BridgeURL:        bridge.adminURL,
		BridgeHost:       bridge.bridgeHost,
		RemoteBridge:     bridge.remote,
		Rounds:           *flagRounds,
		Concurrency:      *flagConcurrency,
		RequestsPerProxy: *flagRequestsPerProxy,
		StartedAt:        time.Now(),
		ProxyCount:       len(proxies),
	}
	failed := false
	for round := 1; round <= *flagRounds; round++ {
		fmt.Printf("round=%d/%d phase=add start total=%d\n", round, *flagRounds, len(proxies))
		r := runRound(bridge, proxies, round)
		report.RoundReports = append(report.RoundReports, r)
		if r.AddFailed > 0 || r.RequestOK != r.RequestTotal || r.DeleteFailed > 0 || r.DeleteVerifyFail > 0 {
			failed = true
		}
		printRound(r)
		fmt.Printf("round=%d/%d complete\n", round, *flagRounds)
	}
	report.FinishedAt = time.Now()

	if *flagReport != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fatalf("encode report: %v", err)
		}
		if err := os.WriteFile(*flagReport, append(data, '\n'), 0600); err != nil {
			fatalf("write report: %v", err)
		}
		fmt.Printf("report written to %s\n", *flagReport)
	}
	if failed {
		os.Exit(1)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "proxy-e2e: "+format+"\n", args...)
	os.Exit(2)
}

func loadProxies(filename string) ([]proxySpec, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var raws []string
	if strings.EqualFold(filepath.Ext(filename), ".csv") {
		r := csv.NewReader(f)
		r.FieldsPerRecord = -1
		for line := 1; ; line++ {
			record, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("CSV line %d: %w", line, err)
			}
			if len(record) == 0 {
				continue
			}
			raw := strings.TrimSpace(strings.TrimPrefix(record[0], "\ufeff"))
			if raw == "" || isProxyHeader(raw) {
				continue
			}
			raws = append(raws, raw)
		}
	} else {
		s := bufio.NewScanner(f)
		for line := 1; s.Scan(); line++ {
			raw := strings.TrimSpace(strings.TrimPrefix(s.Text(), "\ufeff"))
			if raw == "" || strings.HasPrefix(raw, "#") || isProxyHeader(raw) {
				continue
			}
			raws = append(raws, raw)
		}
		if err := s.Err(); err != nil {
			return nil, err
		}
	}

	proxies := make([]proxySpec, 0, len(raws))
	for i, raw := range raws {
		p, err := parseProxy(raw)
		if err != nil {
			return nil, fmt.Errorf("proxy entry %d %q: %w", i+1, maskProxy(raw), err)
		}
		proxies = append(proxies, p)
	}
	return proxies, nil
}

func isProxyHeader(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "proxy" || s == "proxy_url" || s == "proxyurl" || strings.Contains(s, "代理")
}

func parseProxy(raw string) (proxySpec, error) {
	p := proxySpec{Raw: raw}
	value := raw
	if !strings.Contains(value, "://") {
		value = "socks5://" + value
	}
	u, err := url.Parse(value)
	if err != nil {
		return p, err
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" && u.Scheme != "http" {
		return p, fmt.Errorf("unsupported scheme %q, want socks5, socks5h or http", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return p, errors.New("proxy host is empty")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return p, fmt.Errorf("invalid proxy port %q", u.Port())
	}
	p.Scheme, p.Host, p.Port = u.Scheme, host, port
	if u.User != nil {
		p.Username = u.User.Username()
		p.Password, _ = u.User.Password()
	}
	return p, nil
}

func resolveProxyIP(host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", err
	}
	// Prefer IPv4 because bridge/add deployments primarily use IPv4; fall back
	// to the first resolved address for an IPv6-only hostname.
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), nil
		}
	}
	if len(ips) > 0 {
		return ips[0].String(), nil
	}
	return "", fmt.Errorf("no IP address found for %q", host)
}

func resolveBridgeBinary(explicit string) (string, error) {
	if explicit != "" {
		absolute, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(absolute); err != nil {
			return "", err
		}
		return absolute, nil
	}
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	out, err := os.CreateTemp("", "bridge-direct-e2e-*.exe")
	if err != nil {
		return "", err
	}
	outPath := out.Name()
	out.Close()
	os.Remove(outPath)
	cmd := exec.Command("go", "build", "-o", outPath, ".")
	cmd.Dir = root
	if data, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w\n%s", err, data)
	}
	return outPath, nil
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found from current directory")
		}
		dir = parent
	}
}

type bridgeProc struct {
	adminURL   string
	adminAddr  string
	bridgeHost string
	bridgeKey  string
	portStart  int
	remote     bool
	bin        string
	dir        string
	cmd        *exec.Cmd
	done       chan struct{}
	exitErr    error
	logs       *safeBuffer
	sync       *httptest.Server
}

// prepareBridge selects local or remote mode. Local mode remains the default
// so existing invocations keep building and starting an isolated bridge.
// Remote mode only talks to the supplied management API and never starts or
// stops a local bridge process.
func prepareBridge(proxyCount int) (*bridgeProc, error) {
	if *flagBridgeURL != "" {
		if *flagBridgeBin != "" {
			return nil, errors.New("-bridge-bin cannot be used with -bridge-url")
		}
		if *flagBridgeKey == "" {
			return nil, errors.New("-bridge-key is required with -bridge-url")
		}
		if err := validateBridgeKey(*flagBridgeKey); err != nil {
			return nil, err
		}
		if *flagBridgePortStart <= 0 {
			return nil, errors.New("-bridge-port-start is required and must be positive with -bridge-url")
		}
		if err := validateBridgePortRange(*flagBridgePortStart, proxyCount); err != nil {
			return nil, err
		}
		return newRemoteBridge(*flagBridgeURL, *flagBridgeKey, *flagBridgeHost, *flagBridgePortStart)
	}

	if err := validateOptionalBridgePortStart(*flagBridgePortStart, proxyCount); err != nil {
		return nil, err
	}
	fmt.Printf("proxy-e2e preparing bridge binary\n")
	bin, err := resolveBridgeBinary(*flagBridgeBin)
	if err != nil {
		return nil, fmt.Errorf("prepare bridge binary: %w", err)
	}
	fmt.Printf("proxy-e2e starting bridge process\n")
	bridge, err := startBridge(bin)
	if err != nil {
		if *flagBridgeBin == "" {
			_ = os.Remove(bin)
		}
		return nil, err
	}
	if *flagBridgeBin == "" {
		// Keep the generated executable until the child process exits. Windows
		// cannot remove a running executable, while Unix permits it.
		bridge.bin = bin
	}
	bridge.portStart = *flagBridgePortStart
	return bridge, nil
}

func validateBridgeKey(key string) error {
	switch len([]byte(key)) {
	case 16, 24, 32:
		return nil
	default:
		return fmt.Errorf("bridge key must be 16, 24, or 32 bytes, got %d", len([]byte(key)))
	}
}

func validateOptionalBridgePortStart(start, proxyCount int) error {
	if start == 0 {
		return nil
	}
	return validateBridgePortRange(start, proxyCount)
}

func validateBridgePortRange(start, proxyCount int) error {
	if start < 1 || start > 65535 {
		return fmt.Errorf("bridge-port-start must be between 1 and 65535, got %d", start)
	}
	if proxyCount < 1 {
		return nil
	}
	last := int64(start) + int64(proxyCount) - 1
	if last > 65535 {
		return fmt.Errorf("bridge-port-start=%d with %d proxies exceeds port 65535", start, proxyCount)
	}
	return nil
}

func normalizeBridgeURL(raw string) (string, *url.URL, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil, errors.New("bridge URL is empty")
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", nil, fmt.Errorf("parse bridge URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", nil, fmt.Errorf("unsupported bridge URL scheme %q, want http or https", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", nil, errors.New("bridge URL must include a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", nil, errors.New("bridge URL must not contain user info, query, or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return strings.TrimRight(u.String(), "/"), u, nil
}

func newRemoteBridge(rawURL, key, listenerHost string, portStart int) (*bridgeProc, error) {
	adminURL, parsed, err := normalizeBridgeURL(rawURL)
	if err != nil {
		return nil, err
	}
	listenerHost = strings.TrimSpace(listenerHost)
	if listenerHost == "" {
		listenerHost = parsed.Hostname()
	}
	b := &bridgeProc{
		adminURL:   adminURL,
		bridgeHost: listenerHost,
		bridgeKey:  key,
		portStart:  portStart,
		remote:     true,
	}
	if err := waitManagementAPI(parsed, 10*time.Second); err != nil {
		return nil, err
	}
	return b, nil
}

func waitManagementAPI(u *url.URL, within time.Duration) error {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(u.Hostname(), port)
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("bridge management API %s is unreachable: %w", u.String(), lastErr)
}

func startBridge(bin string) (*bridgeProc, error) {
	syncServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":200,"msg":"ok","data":[]}`)
	}))
	dir, err := os.MkdirTemp("", "bridge-direct-e2e-")
	if err != nil {
		syncServer.Close()
		return nil, err
	}
	adminPort, err := freePort()
	if err != nil {
		syncServer.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	cfg := map[string]any{
		"mode": "remote", "addr": net.JoinHostPort("127.0.0.1", strconv.Itoa(adminPort)),
		"logFile": "", "logLevel": "error", "logFormat": "text", "logSource": false,
		"syncDomain": syncServer.URL, "key": testKey, "dataFilename": "bridge.db", "bridgeId": 1,
		"pprofAddr": "", "statsInterval": 0,
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		syncServer.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0600); err != nil {
		syncServer.Close()
		os.RemoveAll(dir)
		return nil, err
	}

	logs := &safeBuffer{}
	cmd := exec.Command(bin, "-c", "config.json")
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, logs, logs
	if err := cmd.Start(); err != nil {
		syncServer.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	done := make(chan struct{})
	b := &bridgeProc{
		adminAddr:  net.JoinHostPort("127.0.0.1", strconv.Itoa(adminPort)),
		adminURL:   "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(adminPort)),
		bridgeHost: "127.0.0.1",
		bridgeKey:  testKey,
		dir:        dir,
		cmd:        cmd, done: done, logs: logs, sync: syncServer,
	}
	go func() {
		b.exitErr = cmd.Wait()
		close(done)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", b.adminAddr, 300*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return b, nil
		}
		select {
		case <-done:
			b.Close()
			return nil, fmt.Errorf("bridge exited during startup: %v\n%s", b.exitErr, logs.String())
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.Close()
	return nil, fmt.Errorf("management API %s did not start\n%s", b.adminAddr, logs.String())
}

func (b *bridgeProc) Close() {
	if b == nil {
		return
	}
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		select {
		case <-b.done:
		case <-time.After(5 * time.Second):
		}
	}
	if b.sync != nil {
		b.sync.Close()
	}
	if b.dir != "" {
		_ = os.RemoveAll(b.dir)
	}
	if b.bin != "" {
		_ = os.Remove(b.bin)
	}
}

func (b *bridgeProc) call(path string, payload dto.UseBridge) (dto.Res, error) {
	var res dto.Res
	plain, err := json.Marshal(payload)
	if err != nil {
		return res, err
	}
	enc, err := utils.AesEncryptCBC(plain, []byte(b.bridgeKey))
	if err != nil {
		return res, err
	}
	body, err := json.Marshal(dto.Req{Ver: "1", Timestamp: time.Now().Unix(), Data: base64.StdEncoding.EncodeToString(enc)})
	if err != nil {
		return res, err
	}
	req, err := http.NewRequest(http.MethodPost, b.adminURL+path, bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return res, err
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return res, fmt.Errorf("decode response %q: %w", truncate(raw, 300), err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return res, fmt.Errorf("management API HTTP status %d: %s", resp.StatusCode, truncate(raw, 300))
	}
	return res, nil
}

func (b *bridgeProc) allocatePort(index int) (int, error) {
	if b.portStart > 0 {
		port := b.portStart + index - 1
		if port < 1 || port > 65535 {
			return 0, fmt.Errorf("bridge port %d is outside 1..65535", port)
		}
		return port, nil
	}
	if b.remote {
		return 0, errors.New("remote bridge requires -bridge-port-start")
	}
	return freePort()
}

func runRound(b *bridgeProc, proxies []proxySpec, round int) roundReport {
	r := roundReport{
		Round: round, ProxyCount: len(proxies), Classes: make(map[string]int),
		ProxyIPs: make(map[string][]string),
	}
	states := make([]*proxyState, len(proxies))
	for i, p := range proxies {
		state := &proxyState{Proxy: p, Index: i + 1, BridgeHost: b.bridgeHost}
		states[i] = state
		port, err := b.allocatePort(i + 1)
		if err != nil {
			r.AddFailed++
			addClass(&r, "add_port_alloc")
			addSample(&r, fmt.Sprintf("proxy %d %s: allocate bridge port: %v", i+1, p.label(), err))
			fmt.Printf("round=%d phase=add progress=%d/%d status=failed class=add_port_alloc proxy=#%d %s\n", round, i+1, len(proxies), i+1, p.label())
			continue
		}
		state.BridgePort = port
		bridgeIP, err := resolveProxyIP(p.Host)
		if err != nil {
			r.AddFailed++
			addClass(&r, "add_proxy_resolve")
			addSample(&r, fmt.Sprintf("proxy %d %s: resolve host for bridge/add: %v", i+1, p.label(), err))
			fmt.Printf("round=%d phase=add progress=%d/%d status=failed class=add_proxy_resolve proxy=#%d %s\n", round, i+1, len(proxies), i+1, p.label())
			continue
		}
		res, err := b.call("/bridge/add", dto.UseBridge{BridgePort: uint16(port), Ip: bridgeIP, Port: uint16(p.Port)})
		if err != nil || res.Code != 200 {
			r.AddFailed++
			addClass(&r, "add_api")
			if err != nil {
				addSample(&r, fmt.Sprintf("proxy %d %s: add: %v", i+1, p.label(), err))
			} else {
				addSample(&r, fmt.Sprintf("proxy %d %s: add code=%d msg=%s", i+1, p.label(), res.Code, res.Msg))
			}
			fmt.Printf("round=%d phase=add progress=%d/%d status=failed class=add_api proxy=#%d %s\n", round, i+1, len(proxies), i+1, p.label())
			continue
		}
		r.AddOK++
		state.Added = true
		fmt.Printf("round=%d phase=add progress=%d/%d status=ok bridgePort=%d proxy=#%d %s\n", round, i+1, len(proxies), port, i+1, p.label())
		if *flagVerbose {
			fmt.Printf("round=%d proxy=%d/%d add=ok bridgePort=%d proxy=%s\n", round, i+1, len(proxies), port, p.label())
		}
	}

	jobs := make(chan requestJob)
	results := make(chan requestResult)
	var workers sync.WaitGroup
	workerCount := *flagConcurrency
	totalJobs := r.AddOK * *flagRequestsPerProxy
	if workerCount > totalJobs {
		workerCount = totalJobs
	}
	if workerCount == 0 {
		// Keep the producer/consumer pipeline well-formed when every add failed;
		// the producer will have no jobs, but it still needs a worker to drain the
		// channel lifecycle cleanly.
		workerCount = 1
	}
	fmt.Printf("round=%d phase=request start total=%d concurrency=%d\n", round, totalJobs, workerCount)
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				started := time.Now()
				ip, class, err := fetchViaProxy(job.State)
				results <- requestResult{State: job.State, Index: job.Index, Class: class, IP: ip, Err: err, Elapsed: time.Since(started)}
			}
		}()
	}
	go func() {
		for _, state := range states {
			if state.BridgePort == 0 || !state.Added {
				continue
			}
			for i := 0; i < *flagRequestsPerProxy; i++ {
				jobs <- requestJob{State: state, Index: i}
			}
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()
	for result := range results {
		r.RequestTotal++
		class := result.Class
		if result.Err == nil {
			result.State.Mu.Lock()
			if result.State.ExpectedIP == "" {
				result.State.ExpectedIP = result.IP
			} else if result.State.ExpectedIP != result.IP {
				class = "ip_mismatch"
				result.Err = fmt.Errorf("expected IP %s, got %s", result.State.ExpectedIP, result.IP)
			}
			result.State.Mu.Unlock()
		}
		if result.Err == nil {
			r.RequestOK++
		} else {
			addSample(&r, fmt.Sprintf("proxy %s bridgePort=%d class=%s: %v", result.State.Proxy.label(), result.State.BridgePort, class, result.Err))
		}
		addClass(&r, class)
		if result.Err != nil || r.RequestTotal == 1 || r.RequestTotal%10 == 0 || r.RequestTotal == totalJobs {
			fmt.Printf("round=%d phase=request progress=%d/%d class=%s proxy=#%d %s ip=%s\n", round, r.RequestTotal, totalJobs, class, result.State.Index, result.State.Proxy.label(), result.IP)
		}
		if *flagVerbose {
			fmt.Printf("round=%d proxy=#%d %s request=%d class=%s ip=%s elapsed=%s err=%v\n", round, result.State.Index, result.State.Proxy.label(), result.Index+1, class, result.IP, result.Elapsed.Round(time.Millisecond), result.Err)
		}
	}
	for _, state := range states {
		if state.ExpectedIP != "" {
			key := fmt.Sprintf("#%d %s", state.Index, state.Proxy.label())
			r.ProxyIPs[key] = append(r.ProxyIPs[key], state.ExpectedIP)
		}
	}

	delTotal := 0
	for _, state := range states {
		if state.BridgePort != 0 && (!b.remote || state.Added) {
			delTotal++
		}
	}
	fmt.Printf("round=%d phase=del start total=%d\n", round, delTotal)
	delProgress := 0
	for i, state := range states {
		// A remote port may already belong to another test. Never issue DEL for
		// an add that did not succeed in remote mode, or a collision could remove
		// someone else's bridge. Local mode allocates a free port and retains the
		// old cleanup behavior for partially applied adds.
		if state.BridgePort == 0 || (b.remote && !state.Added) {
			continue
		}
		delProgress++
		res, err := b.call("/bridge/del", dto.UseBridge{BridgePort: uint16(state.BridgePort)})
		if err != nil || res.Code != 200 {
			r.DeleteFailed++
			addClass(&r, "del_api")
			if err != nil {
				addSample(&r, fmt.Sprintf("proxy %d %s: del: %v", i+1, state.Proxy.label(), err))
			} else {
				addSample(&r, fmt.Sprintf("proxy %d %s: del code=%d msg=%s", i+1, state.Proxy.label(), res.Code, res.Msg))
			}
			fmt.Printf("round=%d phase=del progress=%d/%d status=failed class=del_api proxy=#%d %s\n", round, delProgress, delTotal, i+1, state.Proxy.label())
			continue
		}
		r.DeleteOK++
		if !waitPortClosed(state.BridgeHost, state.BridgePort, 3*time.Second) {
			r.DeleteVerifyFail++
			addClass(&r, "del_verify")
			addSample(&r, fmt.Sprintf("proxy %d %s: bridge port %d still accepts connections", i+1, state.Proxy.label(), state.BridgePort))
			fmt.Printf("round=%d phase=del progress=%d/%d status=failed class=del_verify proxy=#%d %s\n", round, delProgress, delTotal, i+1, state.Proxy.label())
		} else {
			fmt.Printf("round=%d phase=del progress=%d/%d status=ok bridgePort=%d proxy=#%d %s\n", round, delProgress, delTotal, state.BridgePort, i+1, state.Proxy.label())
		}
		if *flagVerbose {
			fmt.Printf("round=%d proxy=%d/%d del=ok bridgePort=%d\n", round, i+1, len(proxies), state.BridgePort)
		}
	}
	return r
}

func addClass(r *roundReport, class string) {
	r.Classes[class]++
}

func addSample(r *roundReport, sample string) {
	if len(r.Samples) < 20 {
		r.Samples = append(r.Samples, sample)
	}
}

func printRound(r roundReport) {
	rate := 0.0
	if r.RequestTotal > 0 {
		rate = float64(r.RequestOK) * 100 / float64(r.RequestTotal)
	}
	fmt.Printf("round=%d proxies=%d addOK=%d addFailed=%d requests=%d ok=%d successRate=%.2f%% delOK=%d delFailed=%d delVerifyFailed=%d classes=%v\n", r.Round, r.ProxyCount, r.AddOK, r.AddFailed, r.RequestTotal, r.RequestOK, rate, r.DeleteOK, r.DeleteFailed, r.DeleteVerifyFail, r.Classes)
	for _, sample := range r.Samples {
		fmt.Printf("  sample: %s\n", sample)
	}
	for label, ips := range r.ProxyIPs {
		fmt.Printf("  proxy=%s expectedIPs=%v\n", label, ips)
	}
}

func fetchViaProxy(state *proxyState) (string, string, error) {
	proxy := state.Proxy
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(state.BridgeHost, strconv.Itoa(state.BridgePort)))
			if err != nil {
				return nil, &stageError{Class: "bridge_connect", Err: err}
			}
			if err := proxyHandshake(conn, proxy); err != nil {
				conn.Close()
				return nil, err
			}
			return conn, nil
		},
	}
	client := &http.Client{Transport: transport, Timeout: *flagTimeout}
	resp, err := client.Get(targetURL)
	if err != nil {
		var staged *stageError
		if errors.As(err, &staged) {
			return "", staged.Class, err
		}
		return "", "target_http", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", "target_http", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", "target_http", fmt.Errorf("HTTP status %d: %s", resp.StatusCode, truncate(body, 200))
	}
	var payload struct {
		IP string `json:"Ip"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "response_invalid", err
	}
	if payload.IP == "" {
		return "", "response_invalid", errors.New("response has no Ip field")
	}
	return payload.IP, "success", nil
}

func socks5Handshake(conn net.Conn, proxy proxySpec) error {
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return &stageError{"proxy_handshake", err}
	}
	methods := []byte{0x00}
	if proxy.Username != "" || proxy.Password != "" {
		methods = []byte{0x00, 0x02}
	}
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return &stageError{"proxy_handshake", err}
	}
	selected := make([]byte, 2)
	if _, err := io.ReadFull(conn, selected); err != nil {
		return &stageError{"proxy_handshake", err}
	}
	if selected[0] != 0x05 {
		return &stageError{"proxy_handshake", fmt.Errorf("invalid SOCKS version %d", selected[0])}
	}
	if selected[1] == 0x02 {
		if proxy.Username == "" && proxy.Password == "" {
			return &stageError{"proxy_auth", errors.New("proxy requested username/password but credentials are empty")}
		}
		if len(proxy.Username) > 255 || len(proxy.Password) > 255 {
			return &stageError{"proxy_auth", errors.New("proxy credentials exceed SOCKS5 length limit")}
		}
		auth := []byte{0x01, byte(len(proxy.Username))}
		auth = append(auth, proxy.Username...)
		auth = append(auth, byte(len(proxy.Password)))
		auth = append(auth, proxy.Password...)
		if _, err := conn.Write(auth); err != nil {
			return &stageError{"proxy_auth", err}
		}
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(conn, authResp); err != nil {
			return &stageError{"proxy_auth", err}
		}
		if authResp[1] != 0x00 {
			return &stageError{"proxy_auth", fmt.Errorf("proxy rejected credentials, status=%d", authResp[1])}
		}
	} else if selected[1] != 0x00 {
		return &stageError{"proxy_auth", fmt.Errorf("proxy selected unsupported method %d", selected[1])}
	}

	host := targetHost
	request := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	request = append(request, host...)
	request = append(request, 0x00, 0x50) // targetHost:80
	if _, err := conn.Write(request); err != nil {
		return &stageError{"proxy_connect", err}
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return &stageError{"proxy_connect", err}
	}
	if head[1] != 0x00 {
		return &stageError{"proxy_connect", fmt.Errorf("proxy CONNECT rejected, rep=%d", head[1])}
	}
	if err := discardSocksBoundAddr(conn, head[3]); err != nil {
		return &stageError{"proxy_connect", err}
	}
	return conn.SetDeadline(time.Time{})
}

func proxyHandshake(conn net.Conn, proxy proxySpec) error {
	if proxy.Scheme == "http" {
		return httpConnectHandshake(conn, proxy)
	}
	return socks5Handshake(conn, proxy)
}

func httpConnectHandshake(conn net.Conn, proxy proxySpec) error {
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return &stageError{"proxy_handshake", err}
	}
	var request strings.Builder
	fmt.Fprintf(&request, "CONNECT %s:80 HTTP/1.1\r\nHost: %s:80\r\n", targetHost, targetHost)
	if proxy.Username != "" || proxy.Password != "" {
		credentials := base64.StdEncoding.EncodeToString([]byte(proxy.Username + ":" + proxy.Password))
		request.WriteString("Proxy-Authorization: Basic ")
		request.WriteString(credentials)
		request.WriteString("\r\n")
	}
	request.WriteString("Proxy-Connection: Keep-Alive\r\n\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		return &stageError{"proxy_handshake", err}
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return &stageError{"proxy_handshake", err}
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return &stageError{"proxy_handshake", fmt.Errorf("invalid HTTP proxy response %q", strings.TrimSpace(line))}
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return &stageError{"proxy_handshake", fmt.Errorf("invalid HTTP proxy status %q", fields[1])}
	}
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			return &stageError{"proxy_handshake", err}
		}
		if strings.TrimSpace(header) == "" {
			break
		}
	}
	if status == http.StatusProxyAuthRequired {
		return &stageError{"proxy_auth", fmt.Errorf("HTTP proxy rejected credentials, status=%d", status)}
	}
	if status != http.StatusOK {
		return &stageError{"proxy_connect", fmt.Errorf("HTTP proxy CONNECT rejected, status=%d", status)}
	}
	return conn.SetDeadline(time.Time{})
}

func discardSocksBoundAddr(conn net.Conn, atyp byte) error {
	switch atyp {
	case 0x01:
		buf := make([]byte, 4+2)
		_, err := io.ReadFull(conn, buf)
		return err
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return err
		}
		buf := make([]byte, int(length[0])+2)
		_, err := io.ReadFull(conn, buf)
		return err
	case 0x04:
		buf := make([]byte, 16+2)
		_, err := io.ReadFull(conn, buf)
		return err
	default:
		return fmt.Errorf("unsupported SOCKS bound address type %d", atyp)
	}
}

func waitPortClosed(host string, port int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return true
		}
		conn.Close()
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(port)
}

func maskProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.User = nil
	return u.String()
}

func truncate(data []byte, n int) string {
	if len(data) <= n {
		return string(data)
	}
	return string(data[:n]) + "..."
}

type safeBuffer struct {
	Mu  sync.Mutex
	Buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	return b.Buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	return b.Buf.String()
}
