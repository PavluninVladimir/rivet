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
	fs.StringVar(&cfg.Token, "token", os.Getenv("RIVET_RUNNER_TOKEN"), "токен регистрации runner'а (обязателен)")
	fs.BoolVar(&cfg.TLS, "tls", os.Getenv("RIVET_PLANE_TLS") == "1", "подключаться к control plane по TLS")
	fs.StringVar(&cfg.TLSCA, "tls-ca", os.Getenv("RIVET_PLANE_TLS_CA"), "корневой сертификат control plane (пусто — системные корни)")
	fs.StringVar(&cfg.ID, "id", envDef("RIVET_RUNNER_ID", hostDefault()), "идентификатор runner'а")
	fs.StringVar(&cfg.Agent, "agent", envDef("RIVET_AGENT", "claude-code"), "название агента")
	fs.StringVar(&cfg.Model, "model", os.Getenv("RIVET_MODEL"), "модель агента")
	fs.StringVar(&cfg.AgentCmd, "cmd", envDef("RIVET_AGENT_CMD", runner.DefaultAgentCmd),
		"команда агента для обёртки; путь к промпту приходит в $RIVET_PROMPT_FILE")
	fs.StringVar(&cfg.Adapter, "adapter", os.Getenv("RIVET_ADAPTER"),
		"адаптер агента: claude-code (нативный), wrap (обёртка) или external (своя программа по контракту pkg/adapter); пусто — по агенту")
	fs.StringVar(&cfg.AdapterCmd, "adapter-cmd", os.Getenv("RIVET_ADAPTER_CMD"),
		"команда внешнего адаптера: задание стадии приходит ему на stdin, события — построчным JSON на stdout")
	fs.StringVar(&cfg.AdapterDepth, "adapter-depth", envDef("RIVET_ADAPTER_DEPTH", "minimal"),
		"глубина данных внешнего адаптера: full, partial или minimal")
	fs.BoolVar(&cfg.AdapterContext, "adapter-context", os.Getenv("RIVET_ADAPTER_CONTEXT") == "1",
		"внешний адаптер принимает контекст от Rivet (обратный канал)")
	fs.StringVar(&cfg.ClaudeBin, "claude-bin", envDef("RIVET_CLAUDE_BIN", "claude"),
		"бинарник Claude Code для нативного адаптера")
	caps := fs.String("caps", envDef("RIVET_CAPS", "coding"), "capabilities через запятую")
	fs.StringVar(&cfg.Workdir, "workdir", envDef("RIVET_WORKDIR", os.ExpandEnv("$HOME/.rivet-runner")), "рабочий каталог")
	fs.StringVar(&cfg.GitBase, "git-base", envDef("RIVET_GIT_BASE", "https://github.com/"), "префикс URL клонирования репозиториев")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Capabilities = strings.Split(*caps, ",")
	switch cfg.Adapter {
	case "":
		cfg.Adapter = runner.DefaultAdapter(cfg.Agent, cfg.AgentCmd)
	case runner.AdapterWrap, runner.AdapterClaudeCode:
	case runner.AdapterExternal:
		if strings.TrimSpace(cfg.AdapterCmd) == "" {
			return fmt.Errorf("внешний адаптер требует -adapter-cmd")
		}
		switch cfg.AdapterDepth {
		case "full", "partial", "minimal":
		default:
			return fmt.Errorf("неизвестная глубина адаптера %q: ожидается full, partial или minimal", cfg.AdapterDepth)
		}
	default:
		return fmt.Errorf("неизвестный адаптер %q: ожидается %s, %s или %s",
			cfg.Adapter, runner.AdapterClaudeCode, runner.AdapterWrap, runner.AdapterExternal)
	}

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
