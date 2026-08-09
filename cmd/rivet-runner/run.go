package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/PavluninVladimir/rivet/internal/runner"
)

func run(args []string) error {
	fs := flag.NewFlagSet("rivet-runner", flag.ContinueOnError)
	var cfg runner.Config
	fs.StringVar(&cfg.PlaneAddr, "plane", envDef("RIVET_PLANE_ADDR", "localhost:8090"), "адрес gRPC control plane")
	fs.StringVar(&cfg.ID, "id", envDef("RIVET_RUNNER_ID", hostDefault()), "идентификатор runner'а")
	fs.StringVar(&cfg.Agent, "agent", envDef("RIVET_AGENT", "claude-code"), "название агента")
	fs.StringVar(&cfg.Model, "model", os.Getenv("RIVET_MODEL"), "модель агента")
	fs.StringVar(&cfg.AgentCmd, "cmd", envDef("RIVET_AGENT_CMD",
		`claude -p "$(cat "$RIVET_PROMPT_FILE")" --dangerously-skip-permissions`),
		"команда агента; путь к промпту приходит в $RIVET_PROMPT_FILE")
	caps := fs.String("caps", envDef("RIVET_CAPS", "coding"), "capabilities через запятую")
	fs.StringVar(&cfg.Workdir, "workdir", envDef("RIVET_WORKDIR", os.ExpandEnv("$HOME/.rivet-runner")), "рабочий каталог")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Capabilities = strings.Split(*caps, ",")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runner.Run(ctx, cfg)
}

func envDef(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func hostDefault() string {
	h, err := os.Hostname()
	if err != nil {
		return fmt.Sprintf("runner-%d", time.Now().Unix()%10000)
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}
