//go:build linux

package server

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func describePortOwner(port int) string {
	inodes, err := listeningSocketInodes(port)
	if err != nil {
		return "port owner lookup failed: " + err.Error()
	}
	if len(inodes) == 0 {
		return "port owner not found in /proc/net/tcp"
	}

	owners := findSocketOwners(inodes)
	if len(owners) == 0 {
		return "port owner PID not found in /proc"
	}
	sort.Ints(owners)
	pid := owners[0]
	name := "unknown"
	if data, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm")); readErr == nil {
		if value := strings.TrimSpace(string(data)); value != "" {
			name = value
		}
	}
	current := ""
	if os.Getpid() == pid {
		current = ", current process"
	}
	return fmt.Sprintf("port owner pid=%d process=%s%s", pid, name, current)
}

func listeningSocketInodes(port int) (map[string]struct{}, error) {
	wantPort := strings.ToUpper(fmt.Sprintf("%04X", port))
	inodes := make(map[string]struct{})
	for _, name := range []string{"tcp", "tcp6"} {
		file, err := os.Open(filepath.Join("/proc/net", name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			local := strings.SplitN(fields[1], ":", 2)
			if len(local) != 2 || strings.ToUpper(local[1]) != wantPort {
				continue
			}
			inodes[fields[9]] = struct{}{}
		}
		closeErr := file.Close()
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return inodes, nil
}

func findSocketOwners(inodes map[string]struct{}) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	owners := make([]int, 0, 1)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", entry.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join("/proc", entry.Name(), "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(target, "socket:[") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			if _, ok := inodes[inode]; ok {
				owners = append(owners, pid)
				break
			}
		}
	}
	return owners
}
