package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/ww1489/seasprak/internal/web"
)

const version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	enabled := fs.Bool("web", false, "start the local HTTP service")
	printVersion := fs.Bool("version", false, "print version")
	var cfg web.Config
	fs.StringVar(&cfg.Workspace, "workspace", "", "explicit absolute workspace directory")
	fs.StringVar(&cfg.StateRoot, "state-root", "", "private state directory outside the workspace")
	fs.StringVar(&cfg.ConfigPath, "config", "", "absolute trusted model configuration file")
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "loopback IP:port")
	fs.Usage = func() {}
	help := func() {
		fmt.Fprintln(stdout, "Usage: web --web --workspace <absolute-dir> --state-root <private-dir> --config <absolute-file> [--listen 127.0.0.1:8080]")
		fmt.Fprintln(stdout, "Options: --help, --version. Serves the local page at / and the /v1 session routes.")
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			help()
			return 0
		}
		fmt.Fprintln(stderr, "invalid command-line arguments")
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}
	if *printVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if !*enabled {
		help()
		return 0
	}
	if cfg.Workspace == "" || cfg.StateRoot == "" || cfg.ConfigPath == "" {
		fmt.Fprintln(stderr, "--workspace, --state-root and --config are required")
		return 2
	}
	server, err := web.Start(ctx, cfg, nil)
	if err != nil {
		fmt.Fprintln(stderr, "web startup failed; verify explicit paths, private directory permissions, model configuration and loopback listener")
		return 1
	}
	if _, err = fmt.Fprintf(stdout, "Listening: %s\nBearer file: %s\n", server.URL(), server.TokenPath()); err != nil {
		server.Close()
		_ = server.Wait()
		return 1
	}
	if err = server.Wait(); err != nil {
		fmt.Fprintln(stderr, "web shutdown failed")
		return 1
	}
	return 0
}
