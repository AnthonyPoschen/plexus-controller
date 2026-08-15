package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/AnthonyPoschen/plexus-controller/internal/supervisor"
	factorioruntime "github.com/AnthonyPoschen/plexus-controller/internal/supervisor/factorio"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	runner := supervisor.Supervisor{
		Adapter:              factorioruntime.Adapter{},
		MaxProcessRecoveries: supervisor.DefaultMaxProcessRecoveries,
	}
	if err := runner.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
