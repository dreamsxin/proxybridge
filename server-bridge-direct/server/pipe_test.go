package server

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TCP↔TCP 是线上唯一的主路径，必须命中内核快路径（Linux 上是 splice），
// 否则每个方向会白占 32KB 池化缓冲——线上 31432 个方向就是约 1GB
func TestHasKernelCopyFastPath(t *testing.T) {
	left, right := tcpConnPair(t)

	if !hasKernelCopyFastPath(left, right) {
		t.Error("TCP 两端必须命中快路径")
	}

	// net.Pipe 既没有 WriteTo 也没有 ReadFrom，必须落回缓冲路径
	pipeA, pipeB := net.Pipe()
	t.Cleanup(func() {
		pipeA.Close()
		pipeB.Close()
	})
	if hasKernelCopyFastPath(pipeA, pipeB) {
		t.Error("net.Pipe 不该被判成快路径")
	}

	// idleTimeoutConn 内嵌 net.Conn 接口，WriteTo/ReadFrom 不会被提升。
	// 这一条是回归保护：哪天有人把内嵌类型改成 *net.TCPConn，
	// 快路径判定会变成 true，而 deadline 逻辑会被 splice 绕过失效
	wrapped := &idleTimeoutConn{Conn: left, idle: time.Minute}
	wrappedPeer := &idleTimeoutConn{Conn: right, idle: time.Minute}
	if hasKernelCopyFastPath(wrappedPeer, wrapped) {
		t.Error("包装过的连接不该被判成快路径")
	}
	// 一端包装、另一端是裸 TCPConn 时，dst 的 ReadFrom 仍然可用
	if !hasKernelCopyFastPath(right, wrapped) {
		t.Error("dst 是 TCPConn 时应当仍然命中 ReadFrom 快路径")
	}
}

// 两条路径都要能把数据原样搬完，不能因为省缓冲而改变行为
func TestPipeTransfersDataOnBothPaths(t *testing.T) {
	payload := bytes.Repeat([]byte("bridge-direct payload "), 4096) // 约 88KB，跨多次读写

	t.Run("tcp fast path", func(t *testing.T) {
		src, srcPeer := tcpConnPair(t)
		dst, dstPeer := tcpConnPair(t)

		go func() {
			srcPeer.Write(payload)
			srcPeer.(*net.TCPConn).CloseWrite()
		}()

		received := make(chan []byte, 1)
		go func() {
			data, _ := io.ReadAll(dstPeer)
			received <- data
		}()

		n, err := pipe(dst, src)
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		dst.(*net.TCPConn).CloseWrite()
		if n != int64(len(payload)) {
			t.Fatalf("copied %d bytes, want %d", n, len(payload))
		}
		if got := <-received; !bytes.Equal(got, payload) {
			t.Fatalf("payload mismatch: got %d bytes", len(got))
		}
	})

	t.Run("buffered fallback", func(t *testing.T) {
		src, srcPeer := net.Pipe()
		dst, dstPeer := net.Pipe()
		t.Cleanup(func() {
			src.Close()
			dst.Close()
		})

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			srcPeer.Write(payload)
			srcPeer.Close()
		}()

		received := make(chan []byte, 1)
		go func() {
			data, _ := io.ReadAll(dstPeer)
			received <- data
		}()

		n, err := pipe(dst, src)
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		dst.Close()
		wg.Wait()
		if n != int64(len(payload)) {
			t.Fatalf("copied %d bytes, want %d", n, len(payload))
		}
		if got := <-received; !bytes.Equal(got, payload) {
			t.Fatalf("payload mismatch: got %d bytes", len(got))
		}
	})
}

// tcpConnPair 返回一对已建立的 TCP 连接
func tcpConnPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type result struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		accepted <- result{conn, err}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server := <-accepted
	if server.err != nil {
		client.Close()
		t.Fatalf("accept: %v", server.err)
	}
	t.Cleanup(func() {
		client.Close()
		server.conn.Close()
	})
	return client, server.conn
}
