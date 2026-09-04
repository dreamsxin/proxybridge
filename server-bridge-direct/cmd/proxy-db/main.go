// Command proxy-db converts a proxy URL list into bridge.db entries.
//
// The input contains complete proxy URLs, while bridge.db only stores the
// local bridge port and the TCP endpoint of the proxy. A companion bridge.csv
// keeps the proxy scheme and credentials for later testing or auditing.
package main

import (
	"bufio"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/baowk/bridge-direct/cachef"
)

var (
	flagProxyFile       = flag.String("proxy-file", "", "proxy list file: one URL per line or CSV first column")
	flagOutput          = flag.String("output", "bridge.db", "output bridge.db path")
	flagCSVOutput       = flag.String("csv-output", "", "companion CSV path; default is output path with .csv extension")
	flagBridgePortStart = flag.Int("bridge-port-start", 10000, "first bridge port assigned to the first proxy")
	flagDedupe          = flag.Bool("dedupe", false, "drop duplicate proxy host:port entries")
	flagVerbose         = flag.Bool("verbose", false, "print every generated bridge entry")
)

type proxyEntry struct {
	Raw      string `json:"raw"`
	Scheme   string `json:"scheme"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func (p proxyEntry) endpoint() string {
	return net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

func main() {
	flag.Parse()
	if *flagProxyFile == "" {
		fatalf("-proxy-file is required")
	}
	if *flagBridgePortStart < 1 || *flagBridgePortStart > 65535 {
		fatalf("-bridge-port-start must be between 1 and 65535")
	}
	entries, err := loadProxyFile(*flagProxyFile)
	if err != nil {
		fatalf("load proxy file: %v", err)
	}
	if *flagDedupe {
		entries, err = dedupeProxyEntries(entries)
		if err != nil {
			fatalf("dedupe proxy file: %v", err)
		}
	}
	bridges, err := buildBridges(entries, *flagBridgePortStart)
	if err != nil {
		fatalf("assign bridge ports: %v", err)
	}
	if err := writeBridgeDB(*flagOutput, bridges); err != nil {
		fatalf("write bridge db: %v", err)
	}
	csvOutput := *flagCSVOutput
	if csvOutput == "" {
		csvOutput = companionCSVPath(*flagOutput)
	}
	if strings.EqualFold(filepath.Clean(csvOutput), filepath.Clean(*flagOutput)) {
		fatalf("-csv-output must be different from -output")
	}
	if err := writeBridgeCSV(csvOutput, entries, bridges); err != nil {
		fatalf("write bridge csv: %v", err)
	}

	fmt.Printf("generated %d bridge entries from %s\n", len(bridges), *flagProxyFile)
	fmt.Printf("output=%s bridgePorts=%d-%d\n", *flagOutput, bridges[0].Port, bridges[len(bridges)-1].Port)
	fmt.Printf("credentials CSV=%s\n", csvOutput)
	if *flagVerbose {
		for i, bridge := range bridges {
			fmt.Printf("%d/%d %d,%s\n", i+1, len(bridges), bridge.Port, bridge.ProxyAddr)
		}
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "proxy-db: "+format+"\n", args...)
	os.Exit(2)
}

func loadProxyFile(filename string) ([]proxyEntry, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var raws []string
	if strings.EqualFold(filepath.Ext(filename), ".csv") {
		r := csv.NewReader(f)
		r.FieldsPerRecord = -1
		for line := 1; ; line++ {
			record, err := r.Read()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, fmt.Errorf("CSV line %d: %w", line, err)
			}
			if len(record) == 0 {
				continue
			}
			raw := cleanProxyText(record[0])
			if raw == "" || isProxyHeader(raw) {
				continue
			}
			raws = append(raws, raw)
		}
	} else {
		s := bufio.NewScanner(f)
		for s.Scan() {
			raw := cleanProxyText(s.Text())
			if raw == "" || strings.HasPrefix(raw, "#") || isProxyHeader(raw) {
				continue
			}
			raws = append(raws, raw)
		}
		if err := s.Err(); err != nil {
			return nil, err
		}
	}

	entries := make([]proxyEntry, 0, len(raws))
	for i, raw := range raws {
		entry, err := parseProxy(raw)
		if err != nil {
			return nil, fmt.Errorf("proxy entry %d %q: %w", i+1, maskProxy(raw), err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func cleanProxyText(value string) string {
	return strings.TrimSpace(strings.TrimPrefix(value, "\ufeff"))
}

func isProxyHeader(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "proxy" || value == "proxy_url" || value == "proxyurl" || strings.Contains(value, "代理")
}

func parseProxy(raw string) (proxyEntry, error) {
	entry := proxyEntry{Raw: raw}
	value := raw
	if !strings.Contains(value, "://") {
		value = "socks5://" + value
	}
	u, err := url.Parse(value)
	if err != nil {
		return entry, err
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "socks5" && scheme != "socks5h" && scheme != "http" {
		return entry, fmt.Errorf("unsupported scheme %q, want socks5, socks5h or http", u.Scheme)
	}
	if u.Hostname() == "" {
		return entry, errors.New("proxy host is empty")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return entry, fmt.Errorf("invalid proxy port %q", u.Port())
	}
	entry.Scheme, entry.Host, entry.Port = scheme, u.Hostname(), port
	if u.User != nil {
		entry.Username = u.User.Username()
		entry.Password, _ = u.User.Password()
	}
	return entry, nil
}

func (p proxyEntry) fullURL() string {
	u := url.URL{Scheme: p.Scheme, Host: p.endpoint()}
	if p.Username != "" || p.Password != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u.String()
}

func dedupeProxyEntries(entries []proxyEntry) ([]proxyEntry, error) {
	seen := make(map[string]struct{}, len(entries))
	result := make([]proxyEntry, 0, len(entries))
	for _, entry := range entries {
		key := entry.endpoint()
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, entry)
	}
	return result, nil
}

func buildBridges(entries []proxyEntry, firstPort int) ([]cachef.Bridge, error) {
	if len(entries) == 0 {
		return nil, errors.New("proxy file contains no valid entries")
	}
	lastPort := int64(firstPort) + int64(len(entries)) - 1
	if lastPort > 65535 {
		return nil, fmt.Errorf("%d entries exceed bridge port range starting at %d", len(entries), firstPort)
	}
	bridges := make([]cachef.Bridge, 0, len(entries))
	for i, entry := range entries {
		bridges = append(bridges, cachef.Bridge{
			Port:      uint16(firstPort + i),
			ProxyAddr: entry.endpoint(),
		})
	}
	return bridges, nil
}

func writeBridgeDB(filename string, bridges []cachef.Bridge) error {
	cf, err := cachef.New(filename)
	if err != nil {
		return err
	}
	defer cf.Close()
	return cf.Replace(bridges)
}

func companionCSVPath(output string) string {
	ext := filepath.Ext(output)
	if strings.EqualFold(ext, ".db") {
		return strings.TrimSuffix(output, ext) + ".csv"
	}
	return output + ".csv"
}

func writeBridgeCSV(filename string, entries []proxyEntry, bridges []cachef.Bridge) error {
	if len(entries) != len(bridges) {
		return fmt.Errorf("proxy entries=%d and bridges=%d differ", len(entries), len(bridges))
	}
	if len(entries) == 0 {
		return errors.New("cannot write an empty bridge csv")
	}

	dir := filepath.Dir(filename)
	tmp, err := os.CreateTemp(dir, filepath.Base(filename)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	w := csv.NewWriter(tmp)
	if err := w.Write([]string{"bridgePort", "proxyScheme", "proxyAddr", "username", "password"}); err != nil {
		_ = tmp.Close()
		return err
	}
	for i, entry := range entries {
		bridge := bridges[i]
		if err := w.Write([]string{
			strconv.Itoa(int(bridge.Port)), entry.Scheme, entry.endpoint(), entry.Username, entry.Password,
		}); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filename); err != nil {
		return err
	}
	tmpName = ""
	return nil
}

func maskProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.User = nil
	return u.String()
}
