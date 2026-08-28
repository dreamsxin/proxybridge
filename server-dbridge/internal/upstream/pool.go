package upstream

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"dbridge/internal/center"
)

var (
	ErrTooManyProxies = errors.New("max active proxies reached")
	ErrTooManyConns   = errors.New("max connections for this proxy reached")
)

// entry tracks the state of one upstream proxy.
type entry struct {
	upstream   *Upstream
	limiter    *BandwidthLimiter
	connCount  atomic.Int32
	bytesTotal atomic.Int64 // lifetime bytes transferred
	bytesCur   atomic.Int64 // bytes in current reporting window
}

// Pool manages active upstream proxies and enforces limits.
type Pool struct {
	mu      sync.Mutex
	entries map[string]*entry // key → entry

	// stats for reporter
	totalConns  atomic.Int32
	curBandwidth atomic.Int64 // bytes transferred in last window (reset by reporter)
}

func NewPool() *Pool {
	return &Pool{entries: make(map[string]*entry)}
}

// Acquire reserves a connection slot on the upstream proxy.
// Returns a handle that must be released when the connection closes.
func (p *Pool) Acquire(up *Upstream, speedLimit int64) (*ConnHandle, error) {
	cfg := center.GetConfig()

	p.mu.Lock()
	e, exists := p.entries[up.Key()]
	if !exists {
		if len(p.entries) >= cfg.MaxProxies {
			p.mu.Unlock()
			return nil, ErrTooManyProxies
		}
		if speedLimit == 0 {
			speedLimit = cfg.DefaultSpeedLimit
		}
		e = &entry{
			upstream: up,
			limiter:  NewBandwidthLimiter(speedLimit),
		}
		p.entries[up.Key()] = e
	}
	p.mu.Unlock()

	cur := e.connCount.Add(1)
	if int(cur) > cfg.MaxConnsPerProxy {
		e.connCount.Add(-1)
		return nil, ErrTooManyConns
	}
	p.totalConns.Add(1)

	return &ConnHandle{pool: p, entry: e}, nil
}

// Dial connects through the upstream proxy to target, applying bandwidth limiting.
func (p *Pool) Dial(ctx context.Context, handle *ConnHandle, target string) (net.Conn, error) {
	return Dial(ctx, handle.entry.upstream, target)
}

// ConnHandle is held for the lifetime of one proxied connection.
type ConnHandle struct {
	pool  *Pool
	entry *entry
}

// Limiter returns the bandwidth limiter for this upstream proxy.
func (h *ConnHandle) Limiter() *BandwidthLimiter {
	return h.entry.limiter
}

// ByteCounter returns the shared bytes counter for stats.
func (h *ConnHandle) ByteCounter() *atomic.Int64 {
	return &h.entry.bytesCur
}

// Release decrements the connection count and removes the entry if idle.
func (h *ConnHandle) Release() {
	h.entry.connCount.Add(-1)
	h.pool.totalConns.Add(-1)

	h.pool.mu.Lock()
	if h.entry.connCount.Load() == 0 {
		delete(h.pool.entries, h.entry.upstream.Key())
	}
	h.pool.mu.Unlock()
}

// Stats returns current pool statistics (implements center.StatsProvider).
func (p *Pool) Stats() center.ReportStats {
	p.mu.Lock()
	activeProxies := len(p.entries)
	var bps int64
	for _, e := range p.entries {
		bps += e.bytesCur.Swap(0) // reset window counter
	}
	p.mu.Unlock()

	return center.ReportStats{
		ActiveProxies: activeProxies,
		TotalConns:    int(p.totalConns.Load()),
		BandwidthBps:  bps,
	}
}
