package main

import (
	"context"
	"fmt"

	"github.com/PavluninVladimir/rivet/internal/config"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func dispatch(cmd string, args []string) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch cmd {
	case "migrate":
		return store.Migrate(ctx, cfg.DatabaseURL)
	default:
		return fmt.Errorf("неизвестная команда %q", cmd)
	}
}
