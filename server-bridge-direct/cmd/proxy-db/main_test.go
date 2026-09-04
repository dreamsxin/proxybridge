package main

import (
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baowk/bridge-direct/cachef"
)

func TestLoadProxyFileCSVAndBuildBridges(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "proxies.csv")
	content := "proxy,source\n\"socks5://user:pass@198.51.100.10:1080\",a\nhttp://198.51.100.11:8080,b\n"
	if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := loadProxyFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Username != "user" || entries[0].Password != "pass" {
		t.Fatalf("credentials were not parsed: %+v", entries[0])
	}
	bridges, err := buildBridges(entries, 30000)
	if err != nil {
		t.Fatal(err)
	}
	want := []cachef.Bridge{
		{Port: 30000, ProxyAddr: "198.51.100.10:1080"},
		{Port: 30001, ProxyAddr: "198.51.100.11:8080"},
	}
	if len(bridges) != len(want) {
		t.Fatalf("bridges = %+v, want %+v", bridges, want)
	}
	for i := range want {
		if bridges[i] != want[i] {
			t.Fatalf("bridge %d = %+v, want %+v", i, bridges[i], want[i])
		}
	}
}

func TestLoadProxyFileTextAndDedupe(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "proxies.txt")
	content := "\ufeff# comment\nsocks5://198.51.100.10:1080\n198.51.100.10:1080\n"
	if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := loadProxyFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	entries, err = dedupeProxyEntries(entries)
	if err != nil || len(entries) != 1 {
		t.Fatalf("deduped entries = %+v, err=%v", entries, err)
	}
}

func TestBuildBridgesRejectsPortOverflow(t *testing.T) {
	entries := []proxyEntry{{Host: "198.51.100.10", Port: 1080}, {Host: "198.51.100.11", Port: 1080}}
	if _, err := buildBridges(entries, 65535); err == nil {
		t.Fatal("expected bridge port overflow error")
	}
}

func TestWriteBridgeDB(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "bridge.db")
	bridges := []cachef.Bridge{{Port: 30000, ProxyAddr: "198.51.100.10:1080"}}
	if err := writeBridgeDB(filename, bridges); err != nil {
		t.Fatal(err)
	}
	cf, err := cachef.New(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer cf.Close()
	if got := cf.Get(30000); got == nil || got.ProxyAddr != bridges[0].ProxyAddr {
		t.Fatalf("loaded bridge = %+v, want %+v", got, bridges[0])
	}
}

func TestWriteBridgeCSVIncludesCredentials(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "bridge.csv")
	entries := []proxyEntry{{
		Scheme: "socks5", Host: "198.51.100.10", Port: 1080,
		Username: "user", Password: "p,ass",
	}}
	bridges := []cachef.Bridge{{Port: 30000, ProxyAddr: "198.51.100.10:1080"}}
	if err := writeBridgeCSV(filename, entries, bridges); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := []string{"bridgePort", "proxyScheme", "proxyAddr", "username", "password"}
	if strings.Join(header, "|") != strings.Join(wantHeader, "|") {
		t.Fatalf("CSV header = %v, want %v", header, wantHeader)
	}
	record, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"30000", "socks5", "198.51.100.10:1080", "user", "p,ass"}
	if strings.Join(record, "|") != strings.Join(want, "|") {
		t.Fatalf("CSV record = %v, want %v", record, want)
	}
	if _, err := r.Read(); err != io.EOF {
		t.Fatalf("CSV has unexpected extra record, err=%v", err)
	}
}

func TestCompanionCSVPath(t *testing.T) {
	if got := companionCSVPath("D:/tmp/bridge.db"); got != "D:/tmp/bridge.csv" {
		t.Fatalf("companion path = %q", got)
	}
	if got := companionCSVPath("D:/tmp/bridges"); got != "D:/tmp/bridges.csv" {
		t.Fatalf("companion path without extension = %q", got)
	}
}
