// Command comporta runs the Comporta LLM gateway.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/JoaoVictorVM/comporta/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
