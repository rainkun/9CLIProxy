//go:build !windows

package management

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func configureTunnelCommand(_ *exec.Cmd) {}

func inspectTunnelProcess(pid int) (string, string, bool) {
	if pid <= 0 {
		return "", "", false
	}
	data, errRead := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if errRead != nil {
		return "", "", false
	}
	commandLine := strings.ReplaceAll(string(data), "\x00", " ")
	commandLine = strings.TrimSpace(commandLine)
	if commandLine == "" {
		return "", "", false
	}
	return filepath.Base(strings.Fields(commandLine)[0]), commandLine, true
}

func findTunnelProcessIDs(port int) []int {
	if port <= 0 {
		return nil
	}
	output, errRun := exec.Command("ps", "-eo", "pid=,args=").Output()
	if errRun != nil {
		return nil
	}
	var ids []int
	portMarker := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, errParse := strconv.Atoi(fields[0])
		if errParse != nil || pid <= 0 {
			continue
		}
		commandLine := strings.ToLower(strings.Join(fields[1:], " "))
		if !strings.Contains(filepath.Base(fields[1]), "cloudflared") ||
			!strings.Contains(commandLine, portMarker) {
			continue
		}
		ids = append(ids, pid)
	}
	return ids
}

func killTunnelProcessPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	process, errFind := os.FindProcess(pid)
	if errFind != nil {
		return errFind
	}
	if errKill := process.Kill(); errKill != nil && !errors.Is(errKill, os.ErrProcessDone) {
		return errKill
	}
	return nil
}
