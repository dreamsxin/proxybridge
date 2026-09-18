//go:build windows

package server

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func describePortOwner(port int) string {
	// Get-NetTCPConnection returns the owning PID for listeners. A PID/name is
	// enough to distinguish a second bridge process from an unrelated service.
	// Failure to query is deliberately non-fatal: the original bind error is
	// still retained and returned to the caller.
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"$c=Get-NetTCPConnection -State Listen -LocalPort "+strconv.Itoa(port)+" -ErrorAction SilentlyContinue; if($c){$c | Select-Object -First 1 -ExpandProperty OwningProcess}")
	output, err := cmd.Output()
	if err != nil {
		return "port owner lookup failed: " + err.Error()
	}
	pidText := strings.TrimSpace(string(output))
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return "port owner not found by Get-NetTCPConnection"
	}
	process := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"$p=Get-Process -Id "+strconv.Itoa(pid)+" -ErrorAction SilentlyContinue; if($p){$p.ProcessName}")
	nameOutput, nameErr := process.Output()
	name := strings.TrimSpace(string(nameOutput))
	if nameErr != nil || name == "" {
		name = "unknown"
	}
	current := ""
	if currentPID := os.Getpid(); currentPID == pid {
		current = ", current process"
	}
	return fmt.Sprintf("port owner pid=%d process=%s%s", pid, name, current)
}
