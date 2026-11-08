package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/teapot2/turnbeam/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signalContext()
	defer stop()
	return cli.Run(ctx, os.Args[1:], version, os.Stdout, os.Stderr)
}

// signalContext is canceled by the first Ctrl-C or SIGTERM, which stops work
// gracefully and keeps valid checkpoints. It then restores default signal
// handling, so a second Ctrl-C terminates immediately.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}
