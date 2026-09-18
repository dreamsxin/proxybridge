//go:build linux

package server

import "os"

// processRSSBytes 返回当前进程的 RSS。读失败时返回 0，调用方按「未知」处理——
// 采样内存指标不该让业务路径出错。
func processRSSBytes() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	kb, ok := parseVmRSSKB(string(data))
	if !ok {
		return 0
	}
	return kb * 1024
}
