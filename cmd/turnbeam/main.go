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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The first signal cancels gracefully, keeping valid checkpoints. Restoring
	// default handling then lets a second Ctrl-C terminate immediately.
	context.AfterFunc(ctx, stop)
	return cli.Run(ctx, os.Args[1:], version, os.Stdout, os.Stderr)
}
