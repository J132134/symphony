package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"symphony/internal/config"
	"symphony/internal/daemon"
	"symphony/internal/menubar"
	"symphony/internal/orchestrator"
	"symphony/internal/status"
	"symphony/internal/version"
	"symphony/internal/workflow"
)

// singleSource adapts a single Orchestrator to the status.Source interface.
type singleSource struct{ o *orchestrator.Orchestrator }

func (s *singleSource) GetAllStates() map[string]*orchestrator.State {
	return map[string]*orchestrator.State{"default": s.o.GetState()}
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage()
		os.Exit(0)
	}

	switch args[0] {
	case "run":
		cmdRun(args[1:])
	case "validate":
		cmdValidate(args[1:])
	case "daemon":
		cmdDaemon(args[1:])
	case "status":
		cmdStatus(args[1:])
	case "menubar":
		cmdMenubar(args[1:])
	case "version":
		cmdVersion()
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		printUsage()
		os.Exit(1)
	}
}

// -- run --

func cmdRun(args []string) {
	opts := parseFlags(args, map[string]string{
		"--workflow":  "WORKFLOW.md",
		"--port":      "",
		"--log-level": "INFO",
	})

	initLogger(opts["--log-level"])

	workflowPath, err := filepath.Abs(opts["--workflow"])
	if err != nil {
		fatalf("resolve workflow path: %v", err)
	}

	slog.Info("symphony.starting", "workflow", workflowPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	o := orchestrator.New(workflowPath, 0, "", nil)

	if portStr := opts["--port"]; portStr != "" {
		var port int
		fmt.Sscanf(portStr, "%d", &port)
		if port > 0 {
			srv := status.New(&singleSource{o}, port, "")
			go func() {
				if err := srv.Run(ctx); err != nil {
					slog.Error("status_server.error", "error", err)
				}
			}()
		}
	}

	if err := o.Run(ctx); err != nil {
		slog.Error("orchestrator.error", "error", err)
		os.Exit(1)
	}
}

// -- validate --

func cmdValidate(args []string) {
	opts := parseFlags(args, map[string]string{
		"--workflow": "WORKFLOW.md",
	})

	workflowPath, err := filepath.Abs(opts["--workflow"])
	if err != nil {
		fatalf("resolve workflow path: %v", err)
	}

	wf, err := workflow.Load(workflowPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	cfg := config.New(wf.Config)
	if errs := cfg.Validate(); len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "Config validation errors:")
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  - %s\n", e)
		}
		os.Exit(1)
	}

	fmt.Printf("Workflow valid: %s\n", workflowPath)
	fmt.Printf("  Tracker: %s\n", cfg.TrackerKind())
	fmt.Printf("  Project: %s\n", cfg.TrackerProjectSlug())
	fmt.Printf("  Active states: %v\n", cfg.ActiveStates())
	fmt.Printf("  Terminal states: %v\n", cfg.TerminalStates())
	fmt.Printf("  Max concurrent: %d\n", cfg.MaxConcurrentAgents())
	fmt.Printf("  Agent command: %s\n", cfg.CodexCommand())
}

// -- daemon --

func cmdDaemon(args []string) {
	opts := parseFlags(args, map[string]string{
		"--config":    "",
		"--log-level": "INFO",
	})

	initLogger(opts["--log-level"])

	cfg, err := config.LoadDaemonConfig(opts["--config"])
	if err != nil {
		fatalf("load config: %v", err)
	}

	if errs := cfg.Validate(); len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "Config errors:")
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  - %s\n", e)
		}
		os.Exit(1)
	}

	names := make([]string, len(cfg.Projects))
	for i, p := range cfg.Projects {
		names[i] = p.Name
	}
	slog.Info("symphony.daemon.starting", "projects", names, "max_total_concurrent_sessions", cfg.MaxTotalConcurrentSessions())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runtime := daemon.NewRuntime(cfg.ConfigPath)
	if err := runtime.Run(ctx, cfg); err != nil {
		slog.Error("daemon.runtime_error", "error", err)
		os.Exit(1)
	}
}

// -- menubar --

func cmdMenubar(args []string) {
	opts := parseFlags(args, map[string]string{
		"--url":  "http://127.0.0.1:7777",
		"--poll": "5s",
	})

	poll, err := time.ParseDuration(opts["--poll"])
	if err != nil || poll <= 0 {
		poll = 5 * time.Second
	}

	if err := menubar.Run(menubar.Options{
		BaseURL:      opts["--url"],
		PollInterval: poll,
	}); err != nil {
		fatalf("menubar: %v", err)
	}
}

func cmdVersion() {
	v := version.Current()
	fmt.Println(v.Version)
}

// -- helpers --

func parseFlags(args []string, defaults map[string]string) map[string]string {
	result := make(map[string]string, len(defaults))
	for k, v := range defaults {
		result[k] = v
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		for k := range defaults {
			shortK := strings.TrimPrefix(k, "--")
			if arg == k || arg == "-"+shortK {
				if i+1 < len(args) {
					i++
					result[k] = args[i]
				}
			} else if strings.HasPrefix(arg, k+"=") {
				result[k] = strings.TrimPrefix(arg, k+"=")
			}
		}
	}
	return result
}

func initLogger(level string) {
	var lvl slog.Level
	switch strings.ToUpper(level) {
	case "DEBUG":
		lvl = slog.LevelDebug
	case "WARN", "WARNING":
		lvl = slog.LevelWarn
	case "ERROR":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(1)
}

func printUsage() {
	fmt.Print(`Symphony Orchestrator — poll tracker, dispatch coding agents

Usage:
  symphony run      [--workflow WORKFLOW.md] [--port PORT] [--log-level LEVEL]
  symphony validate [--workflow WORKFLOW.md]
  symphony daemon   [--config CONFIG_PATH]  [--log-level LEVEL]
  symphony status   [--config CONFIG_PATH] [--url URL] [--json]
  symphony menubar  [--url http://127.0.0.1:7777] [--poll 5s]
  symphony version
  symphony help
`)
}
