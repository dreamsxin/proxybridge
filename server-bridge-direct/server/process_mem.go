package server

import (
	"strconv"
	"strings"
)

// 进程 RSS 采样。为什么需要它：runtime.MemStats 只看得见 Go runtime 自己管的内存，
// 而运维在 top/ps/监控面板上看到的是 RSS。两者对不上是排查内存问题时最常见的分歧：
//   - heapAlloc 只含存活的堆对象，不含 goroutine 栈（几万连接时栈能到几百 MB）
//   - Sys 含已经归还给 OS 的部分（HeapReleased），所以通常高于真实 RSS
//   - 内核 socket 缓冲两者都不含，但在容器里会算进 cgroup 的内存账
//
// 把 RSS 一起打进日志，才能一眼判断多出来的内存到底在不在 Go 堆里。

// parseVmRSSKB 从 /proc/self/status 的内容里取 VmRSS（单位 KB）。
// 单独抽出来是为了能用固定样本做测试——非 Linux 平台上没有这个文件。
func parseVmRSSKB(content string) (uint64, bool) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "VmRSS:"))
		if len(fields) == 0 {
			return 0, false
		}
		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return value, true
	}
	return 0, false
}
