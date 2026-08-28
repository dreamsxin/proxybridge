package upstream

import (
	"context"
	"io"
	"sync/atomic"

	"golang.org/x/time/rate"
)

const bufSize = 32 * 1024

// BandwidthLimiter uses a token bucket (golang.org/x/time/rate).
// No background goroutine — tokens are calculated on demand.
type BandwidthLimiter struct {
	r *rate.Limiter
}

func NewBandwidthLimiter(bytesPerSec int64) *BandwidthLimiter {
	// burst = max(bytesPerSec, bufSize) so a single Write never exceeds burst
	burst := int(bytesPerSec)
	if burst < bufSize {
		burst = bufSize
	}
	return &BandwidthLimiter{
		r: rate.NewLimiter(rate.Limit(bytesPerSec), burst),
	}
}

// wait blocks until n bytes are allowed, chunking if n > burst.
func (l *BandwidthLimiter) wait(ctx context.Context, n int) error {
	burst := l.r.Burst()
	for n > 0 {
		chunk := n
		if chunk > burst {
			chunk = burst
		}
		if err := l.r.WaitN(ctx, chunk); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

// LimitedWriter applies bandwidth limiting and byte counting on writes.
type LimitedWriter struct {
	ctx     context.Context
	w       io.Writer
	limiter *BandwidthLimiter
	counter *atomic.Int64
}

func NewLimitedWriter(ctx context.Context, w io.Writer, limiter *BandwidthLimiter, counter *atomic.Int64) *LimitedWriter {
	return &LimitedWriter{ctx: ctx, w: w, limiter: limiter, counter: counter}
}

func (lw *LimitedWriter) Write(p []byte) (int, error) {
	if err := lw.limiter.wait(lw.ctx, len(p)); err != nil {
		return 0, err
	}
	n, err := lw.w.Write(p)
	if n > 0 {
		lw.counter.Add(int64(n))
	}
	return n, err
}
