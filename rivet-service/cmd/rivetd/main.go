// rivetd — control plane Rivet: клиентский API, планировщик, протокол runner'ов.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/PavluninVladimir/rivet/internal/config"
)

// version — версия сборки; подставляется ldflags (make build), иначе dev.
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		slog.Error("rivetd", "err", err)
		os.Exit(1)
	}
}
