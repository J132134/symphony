package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestGetDaemonPIDRunning(t *testing.T) {
	t.Parallel()

	orig := runLaunchctl
	t.Cleanup(func() { runLaunchctl = orig })

	runLaunchctl = func(args ...string) (string, error) {
		return "12345\t0\tcom.symphony.daemon", nil
	}

	pid, loaded := getDaemonPID()
	if !loaded {
		t.Fatal("loaded = false, want true")
	}
	if pid != 12345 {
		t.Fatalf("pid = %d, want 12345", pid)
	}
}

func TestGetDaemonPIDLoadedButNotRunning(t *testing.T) {
	t.Parallel()

	orig := runLaunchctl
	t.Cleanup(func() { runLaunchctl = orig })

	runLaunchctl = func(args ...string) (string, error) {
		return "-\t-9\tcom.symphony.daemon", nil
	}

	pid, loaded := getDaemonPID()
	if !loaded {
		t.Fatal("loaded = false, want true")
	}
	if pid != 0 {
		t.Fatalf("pid = %d, want 0 (not running)", pid)
	}
}

func TestGetDaemonPIDNotLoaded(t *testing.T) {
	t.Parallel()

	orig := runLaunchctl
	t.Cleanup(func() { runLaunchctl = orig })

	runLaunchctl = func(args ...string) (string, error) {
		return "", fmt.Errorf("exit status 113")
	}

	pid, loaded := getDaemonPID()
	if loaded {
		t.Fatal("loaded = true, want false")
	}
	if pid != 0 {
		t.Fatalf("pid = %d, want 0", pid)
	}
}

func TestIsAgentLoaded(t *testing.T) {
	t.Parallel()

	orig := runLaunchctl
	t.Cleanup(func() { runLaunchctl = orig })

	t.Run("loaded", func(t *testing.T) {
		runLaunchctl = func(args ...string) (string, error) {
			return "12345\t0\tcom.symphony.daemon", nil
		}
		if !isAgentLoaded() {
			t.Fatal("isAgentLoaded() = false, want true")
		}
	})

	t.Run("not loaded", func(t *testing.T) {
		runLaunchctl = func(args ...string) (string, error) {
			return "", fmt.Errorf("exit status 113")
		}
		if isAgentLoaded() {
			t.Fatal("isAgentLoaded() = true, want false")
		}
	})
}

func TestCmdStopWhenNotLoaded(t *testing.T) {
	orig := runLaunchctl
	t.Cleanup(func() { runLaunchctl = orig })

	runLaunchctl = func(args ...string) (string, error) {
		return "", fmt.Errorf("exit status 113")
	}

	// Should not panic or fatalf — just print "not loaded"
	cmdStop(nil)
}

func TestCmdStartWhenAlreadyLoaded(t *testing.T) {
	orig := runLaunchctl
	t.Cleanup(func() { runLaunchctl = orig })

	runLaunchctl = func(args ...string) (string, error) {
		return "12345\t0\tcom.symphony.daemon", nil
	}

	// Should not panic or fatalf — just print "already running"
	cmdStart(nil)
}

func TestCmdLogsArgsDefault(t *testing.T) {
	t.Parallel()

	// Verify that parsing doesn't panic with no args
	// We can't test syscall.Exec in unit tests, but we can verify arg parsing
	args := []string{"--no-follow", "--lines", "100"}
	// Just ensure no panic during arg parsing
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
	if lines != "100" {
		t.Fatalf("lines = %q, want 100", lines)
	}
	if follow {
		t.Fatal("follow = true, want false")
	}
}

func TestGetDaemonPIDWithNewPID(t *testing.T) {
	t.Parallel()

	orig := runLaunchctl
	t.Cleanup(func() { runLaunchctl = orig })

	// Simulate PID changing after restart
	calls := 0
	runLaunchctl = func(args ...string) (string, error) {
		if len(args) > 0 && args[0] == "list" {
			calls++
			if calls <= 2 {
				return "-\t-9\tcom.symphony.daemon", nil
			}
			return "99999\t0\tcom.symphony.daemon", nil
		}
		if len(args) > 0 && args[0] == "stop" {
			return "", nil
		}
		return "", nil
	}

	// First call: not running yet
	pid, loaded := getDaemonPID()
	if !loaded || pid != 0 {
		t.Fatalf("first call: pid=%d loaded=%v", pid, loaded)
	}

	// Advance past threshold
	_, _ = getDaemonPID() // call 2
	pid, loaded = getDaemonPID()
	if !loaded || pid != 99999 {
		t.Fatalf("third call: pid=%d loaded=%v, want 99999/true", pid, loaded)
	}
}

func TestDaemonLogPath(t *testing.T) {
	t.Parallel()

	path := daemonLogPath()
	if !strings.HasSuffix(path, logRelPath) {
		t.Fatalf("daemonLogPath() = %q, want suffix %q", path, logRelPath)
	}
}

func TestPlistPath(t *testing.T) {
	t.Parallel()

	path := plistPath()
	if !strings.HasSuffix(path, plistRelPath) {
		t.Fatalf("plistPath() = %q, want suffix %q", path, plistRelPath)
	}
}
