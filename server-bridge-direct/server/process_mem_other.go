//go:build !linux

package server

// 非 Linux 平台不采 RSS，返回 0 表示「未知」。生产只跑在 Linux 上，
// 这里保留一个空实现是为了 Windows 本地开发和交叉编译能过。
func processRSSBytes() uint64 {
	return 0
}
