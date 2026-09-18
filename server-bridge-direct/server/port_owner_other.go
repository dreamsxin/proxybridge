//go:build !windows && !linux

package server

func describePortOwner(_ int) string {
	return "port owner lookup is unavailable on this platform"
}
