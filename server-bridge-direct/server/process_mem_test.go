package server

import (
	"strings"
	"testing"
)

// /proc/self/status 的真实片段。解析器单独测是因为非 Linux 平台上没有这个文件，
// 而生产只跑在 Linux 上——不能靠「跑起来看看」来验证这段。
func TestParseVmRSSKB(t *testing.T) {
	sample := strings.Join([]string{
		"Name:\tbridge-direct",
		"Umask:\t0022",
		"State:\tS (sleeping)",
		"VmPeak:\t 1355228 kB",
		"VmSize:\t 1289692 kB",
		"VmLck:\t       0 kB",
		"VmPin:\t       0 kB",
		"VmHWM:\t  183624 kB",
		"VmRSS:\t  176492 kB",
		"RssAnon:\t  168904 kB",
		"Threads:\t18",
		"",
	}, "\n")

	kb, ok := parseVmRSSKB(sample)
	if !ok {
		t.Fatal("parseVmRSSKB reported failure on a valid /proc/self/status sample")
	}
	// 必须取 VmRSS 而不是 VmHWM/VmPeak/RssAnon——它们都以 Vm/Rss 开头，很容易抓错
	if kb != 176492 {
		t.Fatalf("VmRSS = %d kB, want 176492", kb)
	}
}

func TestParseVmRSSKBRejectsUnusableInput(t *testing.T) {
	cases := map[string]string{
		"没有 VmRSS 行": "Name:\tbridge-direct\nThreads:\t18\n",
		"VmRSS 值缺失":  "VmRSS:\t\n",
		"VmRSS 值非数字": "VmRSS:\tunknown kB\n",
		"空内容":        "",
	}
	for name, content := range cases {
		if kb, ok := parseVmRSSKB(content); ok {
			t.Errorf("%s: parseVmRSSKB returned ok with %d, want failure", name, kb)
		}
	}
}

// 内存字段之间的关系是恒定的，用它兜住「字段接错」这类错误：
// heapSys 一定不小于 heapAlloc，sys 一定不小于 heapSys
func TestCollectRuntimeStatsMemoryFieldsAreConsistent(t *testing.T) {
	// 撑起一块存活的堆，避免所有值都截断成 0 导致断言失去意义
	ballast := make([]byte, 48<<20)
	for i := 0; i < len(ballast); i += 4096 {
		ballast[i] = 1
	}

	stats := CollectRuntimeStats()

	if stats.HeapAllocMB < 32 {
		t.Fatalf("heapAllocMB = %d, want >= 32 with a 48MB live ballast", stats.HeapAllocMB)
	}
	if stats.HeapSysMB < stats.HeapAllocMB {
		t.Errorf("heapSysMB=%d < heapAllocMB=%d", stats.HeapSysMB, stats.HeapAllocMB)
	}
	if stats.SysMB < stats.HeapSysMB {
		t.Errorf("sysMB=%d < heapSysMB=%d", stats.SysMB, stats.HeapSysMB)
	}
	if stats.Goroutines < 1 {
		t.Errorf("goroutines = %d, want >= 1", stats.Goroutines)
	}
	// 保持 ballast 存活到采样之后，否则 GC 可能已经把它回收掉
	if ballast[0] != 1 {
		t.Fatal("ballast was modified")
	}
}
