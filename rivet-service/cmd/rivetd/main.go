// rivetd — control plane Rivet: клиентский API, планировщик, протокол runner'ов.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/PavluninVladimir/rivet/internal/api"
	"github.com/PavluninVladimir/rivet/internal/config"
)

// Сведения о сборке (спека source-availability): подставляются ldflags
// (make build, Dockerfile). Без них version=dev, коммит неизвестен, а
// адрес исходников указывает на публичный репозиторий Rivet.
var (
	version   = "dev"
	commit    = ""
	sourceURL = api.DefaultSourceURL
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	fs := flag.NewFlagSet("rivetd", flag.ExitOnError)
	rotate := fs.Bool("rotate-key", false,
		"перешифровать секреты в базе новым ключом и завершиться: прежний ключ в RIVET_SECRET_KEY_OLD, новый в RIVET_SECRET_KEY. "+
			"rivetd с прежним ключом на время перешифровки остановите")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "rivetd: control plane Rivet. Настройки задаются переменными окружения RIVET_* (README).")
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *rotate {
		if err := rotateKey(ctx, cfg); err != nil {
			slog.Error("rotate-key", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(ctx, cfg); err != nil {
		slog.Error("rivetd", "err", err)
		os.Exit(1)
	}
}
