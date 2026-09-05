// Command bridge-info prints bridge runtime information for operators.
// It can read Prometheus text from /metrics or the runtime snapshot returned
// by /bridge/status. In auto mode it tries /metrics first, then falls back to
// /bridge/status when metrics are unavailable.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	flagAddr       = flag.String("addr", "http://127.0.0.1:5678", "bridge management base URL")
	flagMetricsURL = flag.String("metrics-url", "", "Prometheus URL; default is <addr>/metrics")
	flagSource     = flag.String("source", "auto", "data source: auto, metrics, or status")
	flagBridgePort = flag.Uint("bridge-port", 0, "optional bridgePort filter for /bridge/status")
	flagCheck      = flag.Bool("check", false, "ask /bridge/status to probe bridge and proxy TCP connectivity")
	flagTimeout    = flag.Duration("timeout", 5*time.Second, "HTTP request timeout")
	flagWatch      = flag.Duration("watch", 0, "repeat at this interval; zero prints once")
	flagJSON       = flag.Bool("json", false, "print a JSON-compatible summary")
)

type runtimeStats struct {
	Goroutines  int    `json:"goroutines"`
	Bridges     int    `json:"bridges"`
	Listening   int    `json:"listening"`
	Conns       int    `json:"conns"`
	Accepted    int64  `json:"accepted"`
	Rejected    int64  `json:"rejected"`
	DialOK      int64  `json:"dialOK"`
	DialFail    int64  `json:"dialFail"`
	HeapAllocMB uint64 `json:"heapAllocMB"`
	SysMB       uint64 `json:"sysMB"`
	NumGC       uint32 `json:"numGC"`
}

type bridgeStatus struct {
	BridgePort    uint16 `json:"bridgePort"`
	ProxyAddr     string `json:"proxyAddr"`
	Listening     bool   `json:"listening"`
	BridgeTCP     bool   `json:"bridgeTcp"`
	ProxyTCP      bool   `json:"proxyTcp"`
	OK            bool   `json:"ok"`
	FailureReason string `json:"failureReason,omitempty"`
	Solution      string `json:"solution,omitempty"`
}

type statusResponse struct {
	Code  int            `json:"code"`
	Msg   string         `json:"msg"`
	Data  []bridgeStatus `json:"data"`
	Stats runtimeStats   `json:"stats"`
}

type operatorSummary struct {
	Source       string             `json:"source"`
	URL          string             `json:"url"`
	CollectedAt  time.Time          `json:"collectedAt"`
	Stats        runtimeStats       `json:"stats"`
	Bridges      []bridgeStatus     `json:"bridges,omitempty"`
	MetricValues map[string]float64 `json:"metrics,omitempty"`
}

func main() {
	flag.Parse()
	if *flagTimeout <= 0 || *flagWatch < 0 {
		fatalf("timeout must be positive and watch must not be negative")
	}
	source := strings.ToLower(strings.TrimSpace(*flagSource))
	if source != "auto" && source != "metrics" && source != "status" {
		fatalf("source must be auto, metrics, or status")
	}
	if *flagBridgePort > 65535 {
		fatalf("bridge-port must be between 0 and 65535")
	}

	client := &http.Client{Timeout: *flagTimeout}
	for {
		summary, err := collect(client, source)
		if err != nil {
			fatalf("collect bridge info: %v", err)
		}
		if *flagJSON {
			data, err := json.Marshal(summary)
			if err != nil {
				fatalf("encode summary: %v", err)
			}
			fmt.Println(string(data))
		} else {
			printSummary(summary)
		}
		if *flagWatch == 0 {
			return
		}
		time.Sleep(*flagWatch)
	}
}

func collect(client *http.Client, source string) (operatorSummary, error) {
	metricsURL := *flagMetricsURL
	if metricsURL == "" {
		metricsURL = joinURL(*flagAddr, "/metrics")
	}
	statusURL := joinURL(*flagAddr, "/bridge/status")
	if *flagBridgePort != 0 {
		statusURL += "?bridgePort=" + strconv.FormatUint(uint64(*flagBridgePort), 10)
		if *flagCheck {
			statusURL += "&check=1"
		}
	} else if *flagCheck {
		statusURL += "?check=1"
	}

	if source == "metrics" || source == "auto" {
		metrics, err := fetchMetrics(client, metricsURL)
		if err == nil {
			return operatorSummary{
				Source:       "metrics",
				URL:          metricsURL,
				CollectedAt:  time.Now(),
				Stats:        statsFromMetrics(metrics),
				MetricValues: metrics,
			}, nil
		} else if source == "metrics" {
			return operatorSummary{}, err
		}
	}

	status, err := fetchStatus(client, statusURL)
	if err != nil {
		if source == "auto" {
			return operatorSummary{}, fmt.Errorf("metrics unavailable at %s and status failed: %w", metricsURL, err)
		}
		return operatorSummary{}, err
	}
	if status.Code != http.StatusOK {
		return operatorSummary{}, fmt.Errorf("status API code=%d msg=%s", status.Code, status.Msg)
	}
	return operatorSummary{
		Source:      "status",
		URL:         statusURL,
		CollectedAt: time.Now(),
		Stats:       status.Stats,
		Bridges:     status.Data,
	}, nil
}

func fetchMetrics(client *http.Client, endpoint string) (map[string]float64, error) {
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("metrics HTTP status %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parsePrometheusText(string(body))
}

func fetchStatus(client *http.Client, endpoint string) (statusResponse, error) {
	var result statusResponse
	resp, err := client.Get(endpoint)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return result, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, fmt.Errorf("status HTTP status %s", resp.Status)
	}
	return result, nil
}

func parsePrometheusText(text string) (map[string]float64, error) {
	values := make(map[string]float64)
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := fields[0]
		labels := ""
		if brace := strings.IndexByte(name, '{'); brace >= 0 {
			labels = name[brace+1 : len(name)-1]
			name = name[:brace]
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		values[name] += value
		for _, label := range []string{"result", "reason", "direction"} {
			if labelValue := prometheusLabelValue(labels, label); labelValue != "" {
				values[name+"."+label+"="+labelValue] += value
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, errors.New("no Prometheus samples found")
	}
	return values, nil
}

func statsFromMetrics(metrics map[string]float64) runtimeStats {
	dialOK := metrics["bridge_dials_total.result=success"]
	dialFail := metrics["bridge_dials_total.result=failure"]
	if dialOK == 0 && dialFail == 0 {
		dialOK = metrics["bridge_dials_total"]
	}
	return runtimeStats{
		Goroutines:  int(metrics["bridge_runtime_goroutines"]),
		Bridges:     int(metrics["bridge_bridges"]),
		Listening:   int(metrics["bridge_listeners"]),
		Conns:       int(metrics["bridge_connections_active_total"]),
		Accepted:    int64(metrics["bridge_connections_accepted_total"]),
		Rejected:    int64(metrics["bridge_connections_rejected_total"]),
		DialOK:      int64(dialOK),
		DialFail:    int64(dialFail),
		HeapAllocMB: uint64(metrics["bridge_runtime_heap_alloc_bytes"] / (1024 * 1024)),
		SysMB:       uint64(metrics["bridge_runtime_sys_bytes"] / (1024 * 1024)),
		NumGC:       uint32(metrics["bridge_runtime_gc_total"]),
	}
}

func prometheusLabelValue(labels, wanted string) string {
	for _, part := range strings.Split(labels, ",") {
		part = strings.TrimSpace(part)
		prefix := wanted + "="
		if !strings.HasPrefix(part, prefix) {
			continue
		}
		return strings.Trim(part[len(prefix):], "\"")
	}
	return ""
}

func printSummary(summary operatorSummary) {
	s := summary.Stats
	fmt.Printf("source=%s url=%s collectedAt=%s bridges=%d listening=%d conns=%d accepted=%d rejected=%d dialOK=%d dialFail=%d goroutines=%d heapAllocMB=%d sysMB=%d numGC=%d\n",
		summary.Source, summary.URL, summary.CollectedAt.Format(time.RFC3339), s.Bridges, s.Listening,
		s.Conns, s.Accepted, s.Rejected, s.DialOK, s.DialFail, s.Goroutines, s.HeapAllocMB, s.SysMB, s.NumGC)
	for _, bridge := range summary.Bridges {
		fmt.Printf("bridge port=%d proxy=%s listening=%t bridgeTcp=%t proxyTcp=%t ok=%t reason=%s\n",
			bridge.BridgePort, bridge.ProxyAddr, bridge.Listening, bridge.BridgeTCP, bridge.ProxyTCP, bridge.OK, bridge.FailureReason)
	}
}

func joinURL(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return base + path
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bridge-info: "+format+"\n", args...)
	os.Exit(2)
}
