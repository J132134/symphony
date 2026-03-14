package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	launchAgentLabel = "com.symphony.daemon"
	plistRelPath     = "Library/LaunchAgents/com.symphony.daemon.plist"
	logRelPath       = "Library/Logs/Symphony/symphony.daemon.stderr.log"
)

// runLaunchctl is overridable for testing.
var runLaunchctl = func(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return string(out), err
}

func plistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fatalf("cannot determine home directory: %v", err)
	}
	return filepath.Join(home, plistRelPath)
}

func daemonLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fatalf("cannot determine home directory: %v", err)
	}
	return filepath.Join(home, logRelPath)
}

// getDaemonPID parses `launchctl list <label>` output.
// Format: "<PID>\t<status>\t<label>" where PID is "-" if not running.
func getDaemonPID() (pid int, running bool) {
	out, err := runLaunchctl("list", launchAgentLabel)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 3 {
		return 0, false
	}
	if fields[0] == "-" {
		return 0, true // loaded but not running
	}
	pid, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, true
	}
	return pid, true
}

func isAgentLoaded() bool {
	_, err := runLaunchctl("list", launchAgentLabel)
	return err == nil
}

// -- stop --

func cmdStop(_ []string) {
	if !isAgentLoaded() {
		fmt.Println("daemon is not loaded")
		return
	}
	path := plistPath()
	if _, err := runLaunchctl("unload", path); err != nil {
		fatalf("launchctl unload: %v", err)
	}
	fmt.Println("daemon stopped (LaunchAgent unloaded)")
}

// -- start --

func cmdStart(_ []string) {
	path := plistPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fatalf("plist not found at %s\nrun: make install-launchagents", path)
	}
	if isAgentLoaded() {
		fmt.Println("daemon is already running")
		return
	}
	if _, err := runLaunchctl("load", path); err != nil {
		fatalf("launchctl load: %v", err)
	}
	fmt.Println("daemon started (LaunchAgent loaded)")
}

// -- restart --

func cmdRestart(_ []string) {
	if !isAgentLoaded() {
		fatalf("daemon is not loaded, use 'symphony start' instead")
	}
	oldPID, _ := getDaemonPID()

	if _, err := runLaunchctl("stop", launchAgentLabel); err != nil {
		fatalf("launchctl stop: %v", err)
	}
	fmt.Printf("stopping daemon (pid: %d)...\n", oldPID)

	const timeout = 30 * time.Second
	const poll = 500 * time.Millisecond
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		time.Sleep(poll)
		newPID, loaded := getDaemonPID()
		if !loaded {
			continue
		}
		if newPID > 0 && newPID != oldPID {
			fmt.Printf("daemon restarted (pid: %d -> %d)\n", oldPID, newPID)
			return
		}
	}
	fmt.Println("warning: restart may still be in progress (ThrottleInterval is 10s)")
}

// -- logs --

func cmdLogs(args []string) {
	lines := "50"
	follow := true
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--lines", "-n":
			if i+1 < len(args) {
				i++
				lines = args[i]
			}
		case "-f", "--follow":
			follow = true
		case "--no-follow":
			follow = false
		}
	}

	logPath := daemonLogPath()
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		fatalf("no log file found at %s", logPath)
	}

	tailArgs := []string{"-n", lines}
	if follow {
		tailArgs = append(tailArgs, "-f")
	}
	tailArgs = append(tailArgs, logPath)

	tailBin, err := exec.LookPath("tail")
	if err != nil {
		fatalf("tail not found: %v", err)
	}
	// Replace process with tail — clean Ctrl+C handling, no wrapper needed.
	if err := syscall.Exec(tailBin, append([]string{"tail"}, tailArgs...), os.Environ()); err != nil {
		fatalf("exec tail: %v", err)
	}
}
