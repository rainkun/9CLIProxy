//go:build windows

package management

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func configureTunnelCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func inspectTunnelProcess(pid int) (string, string, bool) {
	if pid <= 0 {
		return "", "", false
	}
	script := fmt.Sprintf(
		`$p=Get-CimInstance Win32_Process -Filter "ProcessId=%d" -ErrorAction SilentlyContinue; if ($null -ne $p) { [Console]::WriteLine($p.Name); [Console]::WriteLine($p.CommandLine) }`,
		pid,
	)
	output, errRun := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-Command",
		script,
	).Output()
	if errRun != nil {
		return "", "", false
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return "", "", false
	}
	name := strings.TrimSpace(lines[0])
	commandLine := ""
	if len(lines) > 1 {
		commandLine = strings.TrimSpace(strings.Join(lines[1:], " "))
	}
	return name, commandLine, true
}

func findTunnelProcessIDs(port int) []int {
	if port <= 0 {
		return nil
	}
	script := fmt.Sprintf(
		`Get-CimInstance Win32_Process -Filter "Name='cloudflared.exe'" -ErrorAction SilentlyContinue | Where-Object { $_.CommandLine -match ':%d(\D|$)' } | ForEach-Object { [Console]::WriteLine($_.ProcessId) }`,
		port,
	)
	output, errRun := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-Command",
		script,
	).Output()
	if errRun != nil {
		return nil
	}
	var ids []int
	for _, line := range strings.Split(string(output), "\n") {
		pid, errParse := strconv.Atoi(strings.TrimSpace(line))
		if errParse == nil && pid > 0 {
			ids = append(ids, pid)
		}
	}
	return ids
}

func killTunnelProcessPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}
