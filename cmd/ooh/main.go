package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/keyanfayaz/git-ooh/internal/cli"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	runner := cli.New(version)
	os.Exit(runner.Execute(ctx, os.Args[1:]))
}
