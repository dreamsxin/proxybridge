package main

import (
	"runtime/debug"
	"testing"
	"time"
)

func TestResolveBuildMetadataKeepsInjectedValues(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	gotTime, gotCommit := resolveBuildMetadata("2026-09-02T10:00:00Z", "abc1234", now, nil)
	if gotTime != "2026-09-02T10:00:00Z" || gotCommit != "abc1234" {
		t.Fatalf("injected metadata changed: time=%q commit=%q", gotTime, gotCommit)
	}
}

func TestResolveBuildMetadataUsesRuntimeFallbacks(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "1234567890abcdef"}}}
	gotTime, gotCommit := resolveBuildMetadata("unknown", "unknown", now, info)
	if gotTime != now.Format(time.RFC3339) {
		t.Fatalf("runtime build time = %q, want %q", gotTime, now.Format(time.RFC3339))
	}
	if gotCommit != "1234567" {
		t.Fatalf("runtime git commit = %q, want short revision", gotCommit)
	}
}

func TestShortGitCommit(t *testing.T) {
	if got := shortGitCommit("  abcdefghijkl  "); got != "abcdefg" {
		t.Fatalf("short git commit = %q", got)
	}
	if got := shortGitCommit("abc"); got != "abc" {
		t.Fatalf("short git commit for short value = %q", got)
	}
}
