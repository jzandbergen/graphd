// Command graphd is a local-only work graph: tasks as nodes, blockers as
// labeled directed edges, auto-layout on demand, and a computed ready-frontier.
// One binary, two subcommands, one SQLite file. No daemon is required.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"graphd/internal/api"
	"graphd/internal/mcp"
	"graphd/internal/store"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "mcp":
		err = runMCP(args)
	case "version", "-v", "--version":
		fmt.Println("graphd " + version)
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "graphd: unknown subcommand %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "graphd: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `graphd — a local work graph.

usage:
  graphd serve [--db PATH] [--listen ADDR] [--open] [--log-level LEVEL]
               [--allow-remote] [--seed-fixture]
  graphd mcp   [--db PATH] [--log-level LEVEL]
  graphd version

env:
  GRAPHD_DB, GRAPHD_LISTEN, GRAPHD_PROJECT, GRAPHD_LOG_LEVEL

flags override env, env overrides defaults.
`)
}

// defaultDBPath is $XDG_DATA_HOME/graphd/graphd.db, falling back to
// ~/.local/share/graphd/graphd.db.
func defaultDBPath() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "graphd", "graphd.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "graphd.db"
	}
	return filepath.Join(home, ".local", "share", "graphd", "graphd.db")
}

// envOr returns the env var value or a default.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func setupLogging(level string) {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	// All logging goes to stderr. For the mcp subcommand this is not a
	// preference: stdout is the JSON-RPC protocol channel and a single stray
	// byte there corrupts the session (SPEC §9.5).
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})))
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dbPath := fs.String("db", envOr("GRAPHD_DB", defaultDBPath()), "path to the SQLite database")
	listen := fs.String("listen", envOr("GRAPHD_LISTEN", "127.0.0.1:7331"), "address to listen on")
	open := fs.Bool("open", false, "open the browser on start")
	logLevel := fs.String("log-level", envOr("GRAPHD_LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	allowRemote := fs.Bool("allow-remote", false, "allow binding a non-loopback address (dangerous: there is no auth)")
	seedFixture := fs.Bool("seed-fixture", false, "seed the deterministic fixture project before serving")
	if err := fs.Parse(args); err != nil {
		return err
	}
	setupLogging(*logLevel)

	// Bind guard (SPEC §10). There is no authentication of any kind, so a
	// non-loopback bind is a real vulnerability, not a warning.
	if !api.IsLoopback(*listen) && !*allowRemote {
		return fmt.Errorf("refusing to bind %q: not a loopback address and --allow-remote was not passed.\n"+
			"graphd has no authentication; exposing it on a network interface would let anyone read and\n"+
			"write your work graph. Pass --allow-remote if you really mean it.", *listen)
	}
	if !api.IsLoopback(*listen) {
		slog.Warn("binding a non-loopback address without authentication", "listen", *listen)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	slog.Info("database open", "path", st.Path())

	if *seedFixture {
		p, err := st.SeedFixture(context.Background())
		if err != nil {
			return fmt.Errorf("seed fixture: %w", err)
		}
		slog.Info("seeded fixture", "project", p.Name, "id", p.ID)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := api.New(st)
	srv.StartRevisionPoller(ctx)

	ln, err := srv.Listen(*listen)
	if err != nil {
		return err
	}
	base := "http://" + ln.Addr().String()
	slog.Info("listening", "addr", base)
	if *open {
		go openBrowser(base + "/canvas")
	}
	if err := srv.Serve(ctx, ln); err != nil {
		return err
	}
	slog.Info("shutdown complete")
	return nil
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		slog.Warn("could not open browser", "err", err)
	}
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	dbPath := fs.String("db", envOr("GRAPHD_DB", defaultDBPath()), "path to the SQLite database")
	logLevel := fs.String("log-level", envOr("GRAPHD_LOG_LEVEL", "warn"), "log level: debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	setupLogging(*logLevel)

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	srv := mcp.New(st)
	return srv.Serve(context.Background(), os.Stdin, os.Stdout)
}
