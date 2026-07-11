package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cc-hchi/browser-control/internal/config"
	"github.com/cc-hchi/browser-control/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "browserd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("browserd", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "private runtime state directory")
	flags.StringVar(&cfg.SocketPath, "socket", cfg.SocketPath, "public Unix domain socket")
	flags.StringVar(&cfg.BridgeSocketPath, "bridge-socket", cfg.BridgeSocketPath, "trusted native bridge Unix domain socket")
	flags.StringVar(&cfg.BridgeTokenPath, "bridge-token-file", cfg.BridgeTokenPath, "private native bridge authentication token file")
	flags.StringVar(&cfg.ArtifactDir, "artifact-dir", cfg.ArtifactDir, "private artifact directory")
	uploadRoots := &uploadRootFlag{target: &cfg.UploadRoots}
	flags.Var(uploadRoots, "upload-root", "absolute upload allowlist root (first flag replaces defaults/environment; repeat for more)")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}

	oldMask := syscall.Umask(0o077)
	defer syscall.Umask(oldMask)
	runtime, err := server.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer runtime.Close()
	return runtime.Serve(ctx)
}

// uploadRootFlag gives command-line configuration an explicit precedence:
// the first flag replaces the environment/default roots and later flags append
// additional roots.
type uploadRootFlag struct {
	target *[]string
	seen   bool
}

func (f *uploadRootFlag) String() string {
	if f == nil || f.target == nil {
		return ""
	}
	return strings.Join(*f.target, string(os.PathListSeparator))
}

func (f *uploadRootFlag) Set(value string) error {
	if value == "" || !filepath.IsAbs(value) {
		return fmt.Errorf("upload root must be an absolute path")
	}
	if !f.seen {
		*f.target = nil
		f.seen = true
	}
	*f.target = append(*f.target, filepath.Clean(value))
	return nil
}
