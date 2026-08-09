package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

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
	case "createdb":
		if len(args) != 1 {
			return fmt.Errorf("usage: rivet createdb <имя>")
		}
		return createDB(ctx, cfg.DatabaseURL, args[0])
	default:
		return fmt.Errorf("неизвестная команда %q", cmd)
	}
}

// createDB создаёт базу с заданным именем (нужна e2e-стендам, где нет psql)
// и печатает DSN новой базы.
func createDB(ctx context.Context, baseURL, name string) error {
	cfg, err := pgx.ParseConfig(baseURL)
	if err != nil {
		return err
	}
	adminCfg := cfg.Copy()
	adminCfg.Database = "postgres"
	conn, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return err
	}
	out := *cfg
	out.Database = name
	fmt.Printf("postgres://%s:%s@%s:%d/%s?sslmode=disable\n",
		out.User, out.Password, out.Host, out.Port, name)
	return nil
}
