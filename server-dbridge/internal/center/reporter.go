package center

import (
	"bytes"
	"context"
	"log/slog"
	"time"
)

// ReportStats is sent to the central server every 30 seconds.
type ReportStats struct {
	ActiveProxies int   `json:"active_proxies"`
	TotalConns    int   `json:"total_conns"`
	BandwidthBps  int64 `json:"bandwidth_bps"` // current bytes/s
}

// StatsProvider is implemented by the upstream pool.
type StatsProvider interface {
	Stats() ReportStats
}

type Reporter struct {
	client   *Client
	provider StatsProvider
}

func NewReporter(c *Client, provider StatsProvider) *Reporter {
	return &Reporter{client: c, provider: provider}
}

func (r *Reporter) Start(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.report(ctx)
		}
	}
}

func (r *Reporter) report(ctx context.Context) {
	stats := r.provider.Stats()
	if err := r.client.Report(ctx, stats); err != nil {
		slog.Warn("report failed", "err", err)
	}
}

// jsonReader wraps a byte slice as an io.Reader (avoids bytes import in client.go).
func jsonReader(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}
