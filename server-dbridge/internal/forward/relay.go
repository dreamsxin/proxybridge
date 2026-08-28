package forward

import (
	"context"
	"io"
	"net"
	"sync"

	"dbridge/internal/upstream"
)

// bufPool reuses 32KB buffers across all relay goroutines.
// Peak memory: pool size × 32KB instead of connections × 64KB.
var bufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

// Relay copies data bidirectionally between client and remote.
// Rate limiting applies to both directions (shared token bucket = total bandwidth).
func Relay(ctx context.Context, client, remote net.Conn, handle *upstream.ConnHandle) {
	defer handle.Release()
	defer client.Close()
	defer remote.Close()

	limiter := handle.Limiter()
	counter := handle.ByteCounter()

	var wg sync.WaitGroup
	wg.Add(2)

	// client → remote
	go func() {
		defer wg.Done()
		buf := bufPool.Get().(*[]byte)
		defer bufPool.Put(buf)
		lw := upstream.NewLimitedWriter(ctx, remote, limiter, counter)
		io.CopyBuffer(lw, client, *buf)
		closeWrite(remote)
	}()

	// remote → client (same limiter: shared total bandwidth)
	go func() {
		defer wg.Done()
		buf := bufPool.Get().(*[]byte)
		defer bufPool.Put(buf)
		lw := upstream.NewLimitedWriter(ctx, client, limiter, counter)
		io.CopyBuffer(lw, remote, *buf)
		closeWrite(client)
	}()

	wg.Wait()
}

func closeWrite(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
}
