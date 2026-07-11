package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cc-hchi/browser-control/internal/bridgeauth"
	"github.com/cc-hchi/browser-control/internal/config"
	"github.com/cc-hchi/browser-control/internal/nativemsg"
)

func main() {
	if err := run(); err != nil {
		// stdout is reserved exclusively for Native Messaging frames.
		fmt.Fprintf(os.Stderr, "browser-native-host: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("browser-native-host", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	bridgeSocket := cfg.BridgeSocketPath
	bridgeTokenFile := cfg.BridgeTokenPath
	connectTimeout := 10 * time.Second
	flags.StringVar(&bridgeSocket, "bridge-socket", bridgeSocket, "trusted browserd bridge socket")
	flags.StringVar(&bridgeTokenFile, "bridge-token-file", bridgeTokenFile, "private browserd bridge authentication token file")
	flags.DurationVar(&connectTimeout, "connect-timeout", connectTimeout, "daemon connection timeout")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if err := validateInvocationOrigin(flags.Args()); err != nil {
		return err
	}
	token, err := bridgeauth.LoadToken(bridgeTokenFile)
	if err != nil {
		return fmt.Errorf("load bridge transport token: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", bridgeSocket)
	if err != nil {
		return fmt.Errorf("connect to browserd bridge: %w", err)
	}
	defer conn.Close()
	if err := bridgeauth.Authenticate(conn, token, connectTimeout); err != nil {
		return err
	}
	if err := nativemsg.Relay(ctx, os.Stdin, os.Stdout, conn, nativemsg.DefaultChunkSize); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func validateInvocationOrigin(args []string) error {
	if len(args) != 1 || args[0] != bridgeauth.StableExtensionOrigin {
		return fmt.Errorf("native host invocation origin is not authorized")
	}
	return nil
}
