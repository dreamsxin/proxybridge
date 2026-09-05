package server

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/baowk/bridge-direct/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics are intentionally opt-in: no listener is opened unless
// config.metricsAddr is set. Collecting a snapshot only reads atomics and a
// short-lived runListens read lock; it never performs network probes or writes
// logs. Relay byte counters and dial histograms are updated on the data path,
// so they use atomics and do not allocate.
const (
	dialDurationBucketCount = 10
)

var dialDurationBounds = [...]float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10}

func dialDurationBucket(duration time.Duration) int {
	seconds := duration.Seconds()
	for i, upper := range dialDurationBounds {
		if seconds <= upper {
			return i
		}
	}
	return len(dialDurationBounds)
}

type bridgeCollector struct {
	configured      *prometheus.Desc
	bridges         *prometheus.Desc
	listeners       *prometheus.Desc
	activeTotal     *prometheus.Desc
	acceptedTotal   *prometheus.Desc
	rejectedTotal   *prometheus.Desc
	dialsTotal      *prometheus.Desc
	relayBytesTotal *prometheus.Desc
	listenerErrors  *prometheus.Desc
	goroutines      *prometheus.Desc
	heapAlloc       *prometheus.Desc
	sysBytes        *prometheus.Desc
	numGC           *prometheus.Desc
	maxConns        *prometheus.Desc
	maxConnsPort    *prometheus.Desc

	listenerUp         *prometheus.Desc
	listenerActive     *prometheus.Desc
	listenerAccepted   *prometheus.Desc
	listenerRejected   *prometheus.Desc
	listenerDials      *prometheus.Desc
	listenerRelayBytes *prometheus.Desc
	listenerErrorsBy   *prometheus.Desc
	dialDuration       *prometheus.Desc
}

var (
	metricsRegistry = prometheus.NewRegistry()
	metricsOnce     sync.Once
)

func initPrometheusMetrics() {
	metricsOnce.Do(func() {
		metricsRegistry.MustRegister(newBridgeCollector())
	})
}

func newBridgeCollector() *bridgeCollector {
	return &bridgeCollector{
		configured:      prometheus.NewDesc("bridge_proxies_configured", "Number of proxy targets in bridge.db.", nil, nil),
		bridges:         prometheus.NewDesc("bridge_bridges", "Number of bridge listener supervisors.", nil, nil),
		listeners:       prometheus.NewDesc("bridge_listeners", "Number of bridge listeners currently bound.", nil, nil),
		activeTotal:     prometheus.NewDesc("bridge_connections_active_total", "Number of active relay connections across all bridges.", nil, nil),
		acceptedTotal:   prometheus.NewDesc("bridge_connections_accepted_total", "Accepted client connections.", nil, nil),
		rejectedTotal:   prometheus.NewDesc("bridge_connections_rejected_total", "Rejected client connections by reason.", []string{"reason"}, nil),
		dialsTotal:      prometheus.NewDesc("bridge_dials_total", "Target dial attempts by result.", []string{"result"}, nil),
		relayBytesTotal: prometheus.NewDesc("bridge_relay_bytes_total", "Bytes relayed by direction.", []string{"direction"}, nil),
		listenerErrors:  prometheus.NewDesc("bridge_listener_errors_total", "Listener errors by stage.", []string{"stage"}, nil),
		goroutines:      prometheus.NewDesc("bridge_runtime_goroutines", "Current number of Go goroutines.", nil, nil),
		heapAlloc:       prometheus.NewDesc("bridge_runtime_heap_alloc_bytes", "Current heap allocation in bytes.", nil, nil),
		sysBytes:        prometheus.NewDesc("bridge_runtime_sys_bytes", "Bytes obtained from the operating system.", nil, nil),
		numGC:           prometheus.NewDesc("bridge_runtime_gc_total", "Completed garbage collections.", nil, nil),
		maxConns:        prometheus.NewDesc("bridge_connection_limit", "Process-wide connection limit; zero means unlimited.", nil, nil),
		maxConnsPort:    prometheus.NewDesc("bridge_connection_limit_per_port", "Per-bridge connection limit; zero means unlimited.", nil, nil),

		listenerUp:         prometheus.NewDesc("bridge_listener_up", "Whether a bridge listener is currently bound.", bridgeLabels, nil),
		listenerActive:     prometheus.NewDesc("bridge_listener_connections_active", "Active relay connections for one bridge.", bridgeLabels, nil),
		listenerAccepted:   prometheus.NewDesc("bridge_listener_connections_accepted_total", "Accepted connections for one bridge.", bridgeLabels, nil),
		listenerRejected:   prometheus.NewDesc("bridge_listener_connections_rejected_total", "Rejected connections for one bridge.", append(bridgeLabels, "reason"), nil),
		listenerDials:      prometheus.NewDesc("bridge_listener_dials_total", "Target dials for one bridge.", append(bridgeLabels, "result"), nil),
		listenerRelayBytes: prometheus.NewDesc("bridge_listener_relay_bytes_total", "Relayed bytes for one bridge.", append(bridgeLabels, "direction"), nil),
		listenerErrorsBy:   prometheus.NewDesc("bridge_listener_errors_by_stage_total", "Listener errors for one bridge.", append(bridgeLabels, "stage"), nil),
		dialDuration:       prometheus.NewDesc("bridge_dial_duration_seconds", "Target dial duration for one bridge.", bridgeLabels, nil),
	}
}

var bridgeLabels = []string{"bridge_id", "bridge_port", "proxy_addr"}

func (c *bridgeCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		c.configured, c.bridges, c.listeners, c.activeTotal, c.acceptedTotal,
		c.rejectedTotal, c.dialsTotal, c.relayBytesTotal, c.listenerErrors,
		c.goroutines, c.heapAlloc, c.sysBytes, c.numGC, c.maxConns, c.maxConnsPort,
		c.listenerUp, c.listenerActive, c.listenerAccepted, c.listenerRejected,
		c.listenerDials, c.listenerRelayBytes, c.listenerErrorsBy, c.dialDuration,
	} {
		ch <- desc
	}
}

func (c *bridgeCollector) Collect(ch chan<- prometheus.Metric) {
	initPrometheusMetrics()
	runtimeSnapshot := collectRuntimeSnapshot()
	snapshots := runtimeSnapshot.BridgeSnapshots
	configured := 0
	if cf != nil {
		configured = len(cf.All())
	}
	ch <- prometheus.MustNewConstMetric(c.configured, prometheus.GaugeValue, float64(configured))
	ch <- prometheus.MustNewConstMetric(c.bridges, prometheus.GaugeValue, float64(runtimeSnapshot.BridgeStats.Bridges))
	ch <- prometheus.MustNewConstMetric(c.listeners, prometheus.GaugeValue, float64(runtimeSnapshot.BridgeStats.Listening))
	ch <- prometheus.MustNewConstMetric(c.activeTotal, prometheus.GaugeValue, float64(runtimeSnapshot.BridgeStats.Conns))
	ch <- prometheus.MustNewConstMetric(c.acceptedTotal, prometheus.CounterValue, float64(metricTotals.Accepted.Load()))
	ch <- prometheus.MustNewConstMetric(c.rejectedTotal, prometheus.CounterValue, float64(metricTotals.RejectedGlobal.Load()), "global_limit")
	ch <- prometheus.MustNewConstMetric(c.rejectedTotal, prometheus.CounterValue, float64(metricTotals.RejectedPort.Load()), "port_limit")
	ch <- prometheus.MustNewConstMetric(c.dialsTotal, prometheus.CounterValue, float64(metricTotals.DialOK.Load()), "success")
	ch <- prometheus.MustNewConstMetric(c.dialsTotal, prometheus.CounterValue, float64(metricTotals.DialFail.Load()), "failure")
	ch <- prometheus.MustNewConstMetric(c.relayBytesTotal, prometheus.CounterValue, float64(metricTotals.RelayUpBytes.Load()), "up")
	ch <- prometheus.MustNewConstMetric(c.relayBytesTotal, prometheus.CounterValue, float64(metricTotals.RelayDownBytes.Load()), "down")
	ch <- prometheus.MustNewConstMetric(c.listenerErrors, prometheus.CounterValue, float64(metricTotals.BindErrors.Load()), "bind")
	ch <- prometheus.MustNewConstMetric(c.listenerErrors, prometheus.CounterValue, float64(metricTotals.AcceptErrors.Load()), "accept")
	ch <- prometheus.MustNewConstMetric(c.goroutines, prometheus.GaugeValue, float64(runtimeSnapshot.Goroutines))
	ch <- prometheus.MustNewConstMetric(c.heapAlloc, prometheus.GaugeValue, float64(runtimeSnapshot.MemStats.HeapAlloc))
	ch <- prometheus.MustNewConstMetric(c.sysBytes, prometheus.GaugeValue, float64(runtimeSnapshot.MemStats.Sys))
	ch <- prometheus.MustNewConstMetric(c.numGC, prometheus.GaugeValue, float64(runtimeSnapshot.MemStats.NumGC))
	ch <- prometheus.MustNewConstMetric(c.maxConns, prometheus.GaugeValue, float64(config.Cfg.MaxConns))
	ch <- prometheus.MustNewConstMetric(c.maxConnsPort, prometheus.GaugeValue, float64(config.Cfg.MaxConnsPerPort))

	bridgeID := strconv.FormatUint(uint64(config.Cfg.BridgeId), 10)
	for _, snapshot := range snapshots {
		labels := []string{bridgeID, strconv.Itoa(int(snapshot.BridgePort)), snapshot.ProxyAddr}
		ch <- prometheus.MustNewConstMetric(c.listenerUp, prometheus.GaugeValue, boolFloat(snapshot.Listening), labels...)
		ch <- prometheus.MustNewConstMetric(c.listenerActive, prometheus.GaugeValue, float64(snapshot.Conns), labels...)
		ch <- prometheus.MustNewConstMetric(c.listenerAccepted, prometheus.CounterValue, float64(snapshot.Accepted), labels...)
		ch <- prometheus.MustNewConstMetric(c.listenerRejected, prometheus.CounterValue, float64(snapshot.RejectedGlobal), append(labels, "global_limit")...)
		ch <- prometheus.MustNewConstMetric(c.listenerRejected, prometheus.CounterValue, float64(snapshot.RejectedPort), append(labels, "port_limit")...)
		ch <- prometheus.MustNewConstMetric(c.listenerDials, prometheus.CounterValue, float64(snapshot.DialOK), append(labels, "success")...)
		ch <- prometheus.MustNewConstMetric(c.listenerDials, prometheus.CounterValue, float64(snapshot.DialFail), append(labels, "failure")...)
		ch <- prometheus.MustNewConstMetric(c.listenerRelayBytes, prometheus.CounterValue, float64(snapshot.RelayUpBytes), append(labels, "up")...)
		ch <- prometheus.MustNewConstMetric(c.listenerRelayBytes, prometheus.CounterValue, float64(snapshot.RelayDownBytes), append(labels, "down")...)
		ch <- prometheus.MustNewConstMetric(c.listenerErrorsBy, prometheus.CounterValue, float64(snapshot.BindErrors), append(labels, "bind")...)
		ch <- prometheus.MustNewConstMetric(c.listenerErrorsBy, prometheus.CounterValue, float64(snapshot.AcceptErrors), append(labels, "accept")...)
		buckets := make(map[float64]uint64, len(dialDurationBounds))
		cumulative := uint64(0)
		for i, upper := range dialDurationBounds {
			cumulative += snapshot.DialDurationBuckets[i]
			buckets[upper] = cumulative
		}
		cumulative += snapshot.DialDurationBuckets[len(dialDurationBounds)]
		buckets[math.Inf(1)] = cumulative
		ch <- prometheus.MustNewConstHistogram(c.dialDuration, snapshot.DialCount, float64(snapshot.DialDurationNs)/1e9, buckets, labels...)
	}
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// MetricsHandler is exposed for tests and for embedding the metrics endpoint
// into another HTTP server. Production startup uses the dedicated listener.
func MetricsHandler() http.Handler {
	initPrometheusMetrics()
	return promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

func startMetrics(addr string) {
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		slog.Error("metrics addr invalid", "addr", addr, "err", err)
		return
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		slog.Warn("metrics endpoint has no authentication; bind to loopback or monitoring network", "addr", addr)
	}
	go func() {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("metrics server listen failed", "addr", addr, "err", err)
			return
		}
		mux := http.NewServeMux()
		mux.Handle("/metrics", MetricsHandler())
		srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		slog.Info("metrics server listening", "addr", addr, "path", "/metrics")
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server exited", "addr", addr, "err", err)
		}
	}()
}
