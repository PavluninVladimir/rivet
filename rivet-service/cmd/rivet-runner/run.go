package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/PavluninVladimir/rivet/internal/runner"
	"github.com/PavluninVladimir/rivet/internal/runner/native"
)

// run разбирает флаги и запускает runner. secrets — секреты, забранные
// при старте из окружения и файлов (prepareSecrets): токен регистрации и
// ключи подключений.
func run(args []string, secrets map[string]string) error {
	fs := flag.NewFlagSet("rivet-runner", flag.ContinueOnError)
	var cfg runner.Config
	fs.StringVar(&cfg.PlaneAddr, "plane", envDef("RIVET_PLANE_ADDR", "localhost:8090"), "адрес gRPC control plane")
	fs.StringVar(&cfg.Token, "token", secrets[tokenEnv],
		"устарел: токен регистрации виден в списке процессов; передавай его файлом 0600 в "+tokenFileEnv+" (или в "+tokenEnv+")")
	fs.BoolVar(&cfg.TLS, "tls", os.Getenv("RIVET_PLANE_TLS") == "1", "подключаться к control plane по TLS")
	fs.StringVar(&cfg.TLSCA, "tls-ca", os.Getenv("RIVET_PLANE_TLS_CA"), "корневой сертификат control plane (пусто — системные корни)")
	fs.StringVar(&cfg.ID, "id", envDef("RIVET_RUNNER_ID", hostDefault()), "идентификатор runner'а")
	fs.StringVar(&cfg.Name, "name", os.Getenv("RIVET_RUNNER_NAME"),
		"отображаемое имя runner'а в консоли (роль + имя: «Разработчик Макс»); имя из консоли важнее")
	fs.StringVar(&cfg.Agent, "agent", envDef("RIVET_AGENT", runner.AgentRivet),
		"агент runner'а: rivet (агентский цикл Rivet); у runner'а выкатки (-caps deploy) агента нет")
	fs.StringVar(&cfg.Model, "model", os.Getenv("RIVET_MODEL"), "модель агента по умолчанию (если назначение её не задаёт)")
	models := fs.String("models", os.Getenv("RIVET_MODELS"),
		"поддерживаемые модели через запятую: модель сессии приходит в назначении; пусто — модели каталога подключений")
	caps := fs.String("caps", envDef("RIVET_CAPS", "coding"), "capabilities через запятую")
	fs.StringVar(&cfg.Workdir, "workdir", envDef("RIVET_WORKDIR", os.ExpandEnv("$HOME/.rivet-runner")), "рабочий каталог")
	fs.StringVar(&cfg.GitBase, "git-base", envDef("RIVET_GIT_BASE", "https://github.com/"), "префикс URL клонирования репозиториев")
	unsandboxed := fs.Bool("unsandboxed", os.Getenv("RIVET_RUNNER_UNSANDBOXED") == "1",
		"разрешить агента Rivet и проверки проекта без песочницы (macOS, Linux без user namespaces): команды агента и проверки идут с правами runner'а и видят его файлы, включая файл токена и ключи — только для доверенной модели или отладки")
	fs.BoolVar(&cfg.ChecksNetwork, "checks-network", os.Getenv("RIVET_RUNNER_CHECKS_NETWORK") == "1",
		"проверки проекта идут в песочнице с сетью хоста ($HOME runner'а и файл токена по-прежнему скрыты, команды агента остаются без сети): "+
			"для проверок, которым нужны скачивание зависимостей или сервисы хоста")
	readOnly := &pathList{}
	if v := os.Getenv("RIVET_RUNNER_SANDBOX_RO"); v != "" {
		readOnly.paths, readOnly.fromEnv = filepath.SplitList(v), true
	}
	fs.Var(readOnly, "sandbox-ro",
		"каталог, видимый в песочнице только на чтение проверкам и командам агента (~/.nvm, /opt/toolchains); флаг повторяется, "+
			"в RIVET_RUNNER_SANDBOX_RO — список через "+string(os.PathListSeparator)+". Нельзя $HOME и каталоги выше, файл токена и его каталог, рабочий каталог runner'а")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Unsandboxed = *unsandboxed
	cfg.Secrets = map[string]string{}
	for k, v := range secrets {
		if k != tokenEnv {
			cfg.Secrets[k] = v
		}
	}
	// Файл токена скрывается от команд агента в песочнице (он под тем же
	// uid, что и runner).
	if tf := tokenFilePath(); tf != "" {
		cfg.SecretFiles = append(cfg.SecretFiles, tf)
	}
	// Каталоги -sandbox-ro проверяются до подключения: каталог, который
	// открыл бы песочнице $HOME runner'а, файл токена или рабочие копии
	// других задач, — ошибка запуска.
	home, _ := os.UserHomeDir()
	ro, err := runner.CheckSandboxReadOnly(readOnly.paths, home, cfg.Workdir, cfg.SecretFiles)
	if err != nil {
		return err
	}
	cfg.SandboxReadOnly = ro
	cfg.Capabilities = withArchitectureCapability(strings.Split(*caps, ","))
	// Проверка архитектуры на шаге test и задание Архитектора зовут этот же
	// бинарь (add-architecture-c4): путь едет в RIVET_RUNNER_BIN проверок и
	// команд агента, а в песочнице файл виден только на чтение.
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		cfg.RunnerBin = exe
	}
	for _, mdl := range strings.Split(*models, ",") {
		if mdl = strings.TrimSpace(mdl); mdl != "" {
			cfg.Models = append(cfg.Models, mdl)
		}
	}
	// Runner выкатки (единственная capability deploy) работает без агента:
	// он исполняет только задания выкатки.
	deployOnly := len(cfg.Capabilities) == 1 && strings.TrimSpace(cfg.Capabilities[0]) == "deploy"
	switch {
	case deployOnly && (cfg.Agent == "" || cfg.Agent == runner.AgentRivet):
		cfg.Agent, cfg.Adapter = "", ""
	case cfg.Agent == runner.AgentRivet:
		cfg.Adapter = runner.AdapterNative
	default:
		return fmt.Errorf("агентом может быть только агент Rivet: запустите rivet-runner -agent %s (указан %q)", runner.AgentRivet, cfg.Agent)
	}
	if cfg.Adapter == runner.AdapterNative {
		sandbox := native.NetworkIsolationAvailable()
		// Действующие настройки песочницы — в журнал при каждом старте.
		slog.Info("песочница команд агента и проверок проекта",
			"sandbox", sandbox, "unsandboxed", cfg.Unsandboxed,
			"checks_network", cfg.ChecksNetwork, "sandbox_ro", cfg.SandboxReadOnly)
		if sandbox {
			slog.Info("команды агента выполняются в песочнице без сети, файлы runner'а скрыты")
			if cfg.ChecksNetwork {
				slog.Info("проверки проекта идут в песочнице с сетью хоста (-checks-network): $HOME runner'а и файл токена скрыты, команды агента без сети")
			}
		} else if *unsandboxed {
			slog.Warn("ВНИМАНИЕ: песочница недоступна, команды агента идут с правами runner'а",
				"причина", native.SandboxUnavailableReason(),
				"риск", "недоверенная модель может прочитать файл токена runner'а, ключи подключений и другие его файлы и выполнить произвольный код под этим пользователем",
				"рекомендация", "для недоверенной модели запускай runner в отдельной VM или контейнере либо на Linux с user namespaces")
		} else {
			return fmt.Errorf("песочница для команд агента недоступна (%s): команды пошли бы с правами runner'а и могли бы прочитать его файл токена и ключи; "+
				"запусти на Linux с user namespaces, в отдельной VM или контейнере, либо, если это осознанно (доверенная модель или отладка), укажи -unsandboxed",
				native.SandboxUnavailableReason())
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runner.Run(ctx, cfg)
}

// pathList — повторяемый флаг -sandbox-ro. Значение из окружения —
// умолчание: первый флаг в командной строке его заменяет.
type pathList struct {
	paths   []string
	fromEnv bool
}

func (p *pathList) String() string {
	if p == nil {
		return ""
	}
	return strings.Join(p.paths, string(os.PathListSeparator))
}

func (p *pathList) Set(v string) error {
	if p.fromEnv {
		p.paths, p.fromEnv = nil, false
	}
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("пустой путь")
	}
	p.paths = append(p.paths, v)
	return nil
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
