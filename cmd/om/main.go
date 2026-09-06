package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/sentiolabs/observational-memory/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := cli.New().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "om:", err)
		os.Exit(1)
	}
}
