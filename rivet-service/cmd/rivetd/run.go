package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/PavluninVladimir/rivet/internal/api"
	"github.com/PavluninVladimir/rivet/internal/blob"
	"github.com/PavluninVladimir/rivet/internal/config"
	"github.com/PavluninVladimir/rivet/internal/infra"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/runnertls"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
	"github.com/PavluninVladimir/rivet/internal/stream"
	"github.com/PavluninVladimir/rivet/internal/webui"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

func run(ctx context.Context, cfg config.Config) error {
	// Конфиг проверяем до миграций и listener'ов, чтобы падать без side effects.
	// С опубликованными ключом и паролем rivetd не стартует (спека
	// access-policy «Безопасные умолчания установки»).
	if err := cfg.CheckServe(); err != nil {
		return err
	}
	// Ключ из окружения — запасной источник планировщика: активный провайдер
	// в базе имеет приоритет (design add-operations-management).
	env := api.EnvPlanner{Provider: cfg.LLMProvider}
	switch cfg.LLMProvider {
	case "deepseek":
		env.Key, env.Model = cfg.DeepSeekAPIKey, cfg.DeepSeekModel
	case "anthropic":
		env.Key = cfg.AnthropicAPIKey
	case "":
		// Ключей в окружении нет: декомпозиция доступна только с ключом из базы.
	default:
		return fmt.Errorf("неизвестный RIVET_LLM_PROVIDER %q (anthropic или deepseek)", cfg.LLMProvider)
	}
	if env.Provider != "" && env.Key == "" {
		// Не фатально: ключ может лежать в базе, а состояние установки
		// покажет «модель не настроена», если его нет и там.
		slog.Warn("RIVET_LLM_PROVIDER задан без ключа в окружении: запасной источник модели отключён", "provider", env.Provider)
		env = api.EnvPlanner{}
	}

	if err := store.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	// Пока rivetd работает, перешифровка ключа (rivetd -rotate-key) с этой
	// базой не идёт: он держит прежний ключ в памяти.
	// Перешифровка, прошедшая в окне без блокировки (обрыв соединения),
	// оставляет ключ в памяти устаревшим: rivetd завершается с ошибкой, его
	// перезапускают с новым ключом.
	rotated := make(chan struct{})
	var rotatedOnce sync.Once
	releaseLock, err := st.HoldServeLock(ctx, func() { rotatedOnce.Do(func() { close(rotated) }) })
	if err != nil {
		return err
	}
	defer releaseLock()

	// Опубликованный RIVET_ADMIN_PASSWORD: отказ, если он станет паролем
	// первого администратора, иначе предупреждение (спека access-policy
	// «Безопасные умолчания установки»).
	users, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	warn, err := config.AdminPasswordCheck(cfg.AdminPassword, users > 0)
	if err != nil {
		return err
	}
	if warn != "" {
		slog.Warn(warn)
	}
	// Fail-fast до listener'ов: без владельца установка не поднимается.
	if err := st.Bootstrap(ctx, cfg.AdminLogin, cfg.AdminPassword); err != nil {
		return err
	}
	// Администраторы с паролем из документации (установки по старому
	// docker-compose.yml с admin-local) при следующем входе меняют пароль.
	flagged, err := st.FlagPublishedPasswords(ctx)
	if err != nil {
		return err
	}
	for _, login := range flagged {
		slog.Warn("у администратора пароль из документации Rivet: при следующем входе консоль потребует сменить пароль", "login", login)
	}

	bl, err := blob.New(cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3UseSSL)
	if err != nil {
		return err
	}
	if err := bl.EnsureBucket(ctx); err != nil {
		slog.Warn("S3-хранилище недоступно, транскрипты не будут сохраняться", "err", err)
		bl = nil
	}

	reg := stream.NewRegistry()
	hub := stream.NewHub()
	var adapter scm.Adapter = scm.NewGitHub(cfg.GitHubToken)
	if cfg.SCM == "fake" {
		slog.Warn("SCM-провайдер fake: PR и merge фиктивные (режим e2e-стенда)")
		adapter = scm.NewFake()
	}
	// Ключ шифрования учётных данных хостингов; без него подключение
	// репозиториев с токеном отключено (fail-closed), установка работает
	// на глобальном токене.
	box, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		return err
	}
	if !box.Enabled() {
		slog.Warn("RIVET_SECRET_KEY не задан: подключение репозиториев с токеном отключено")
	}
	// Движок политик: решения точек принуждения. В режиме external адрес
	// обязателен — иначе установка не поднимается (fail-fast до listener'ов).
	polEngine, err := policy.NewEngine(policy.Config{
		Mode: cfg.PolicyMode, URL: cfg.PolicyURL, Timeout: cfg.PolicyTimeout,
	})
	if err != nil {
		return err
	}
	slog.Info("движок политик", "mode", polEngine.Mode())

	engine := orchestrator.New(st, adapter, bl, reg, cfg.RunnerHeartbeatTimeout)
	engine.Box = box
	engine.LoadLimits = cfg.LoadLimits
	engine.Policy = polEngine
	urlPolicy, err := orchestrator.ParseURLPolicy(cfg.AllowPrivateURLs)
	if err != nil {
		return err
	}
	engine.URLPolicy = urlPolicy
	// Провайдеры хостов окружений (add-environment-provisioning).
	engine.Infra = &infra.Factory{RUVDSBaseURL: cfg.RUVDSAPIURL}
	// RIVET_SCM=fake — установка без настоящих хостингов (e2e-стенд).
	engine.Adapters.Force = cfg.SCM == "fake"

	// Протокол runner'ов: регистрация аутентифицируется токеном на обоих
	// RPC, токен привязан к идентификаторам runner'ов; TLS включается парой
	// PEM-файлов, mTLS включают корни клиентских сертификатов (спека runners).
	rs := &stream.Server{
		St: st, Engine: engine, Reg: reg, Hub: hub,
		HeartbeatInterval:  cfg.RunnerHeartbeatTimeout / 3,
		ClientCertRequired: cfg.GRPCClientCA != "",
	}
	grpcOpts := []grpc.ServerOption{
		grpc.UnaryInterceptor(rs.UnaryAuth()),
		grpc.StreamInterceptor(rs.StreamAuth()),
	}
	if cfg.GRPCTLSCert != "" {
		tlsCfg, err := runnertls.ServerConfig(cfg.GRPCTLSCert, cfg.GRPCTLSKey, cfg.GRPCClientCA)
		if err != nil {
			return fmt.Errorf("TLS протокола runner'ов: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(runnertls.LogHandshakeFailures(credentials.NewTLS(tlsCfg))))
		if cfg.GRPCClientCA != "" {
			slog.Info("протокол runner'ов требует клиентский сертификат (mTLS): CN или DNS SAN сверяется с идентификатором runner'а")
		}
	}
	for _, w := range cfg.ListenWarnings() {
		slog.Warn(w)
	}
	if cfg.GRPCClientCA == "" && !config.IsLoopbackAddr(cfg.GRPCAddr) {
		slog.Warn("клиентские сертификаты runner'ов не проверяются (RIVET_GRPC_CLIENT_CA не задан), а протокол слушает не только loopback: "+
			"runner'ы различаются только токенами регистрации", "addr", cfg.GRPCAddr)
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	pb.RegisterRunnerServiceServer(grpcSrv, rs)
	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}

	apiSrv := &api.Server{
		St: st, Engine: engine, Hub: hub, Blob: bl,
		EnvPlanner: env, Version: version, ProtocolVersion: stream.ProtocolVersion,
		About:     api.NewAbout(version, commit, sourceURL, cfg.SourceURL),
		StartedAt: time.Now(), GRPCAddr: cfg.GRPCAddr, GRPCTLS: cfg.GRPCTLSCert != "", GRPCClientCert: cfg.GRPCClientCA != "",
		Secrets: box, PublicURL: cfg.PublicURL, Policy: polEngine,
		WebhookSecret: cfg.GitHubWebhookSecret,
		TrustProxy:    cfg.TrustProxy,
	}
	if browser, err := exec.LookPath(cfg.DesignChromiumPath); err == nil {
		apiSrv.DesignScreenshot = api.NewDesignScreenshotRenderer(browser)
	} else {
		slog.Warn("дизайн: Chromium недоступен, передача снимка вернёт screenshot_unavailable")
	}
	go apiSrv.RunAssistant(ctx)
	if err := apiSrv.ReloadPlanner(ctx); err != nil {
		return err
	}
	// Анализы интервью и декомпозиции бэклога, прерванные прошлым
	// остановом, завершаются failed с причиной (add-product-discovery).
	if err := apiSrv.RecoverDiscovery(ctx); err != nil {
		return fmt.Errorf("восстановление исследования: %w", err)
	}
	engine.IssueWatch = func(jobCtx context.Context, projectID, agentID string) error {
		jobCtx, cancel := context.WithCancel(jobCtx)
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		defer cancel()
		return apiSrv.PollProjectIssues(jobCtx, projectID, agentID)
	}
	root := http.NewServeMux()
	root.Handle("/api/", apiSrv.Handler())
	root.Handle("/", webui.Handler())
	httpSrv := &http.Server{
		Addr: cfg.HTTPAddr,
		// Общие заголовки безопасности и на ответах самого ServeMux
		// (редиректы путей), CSP — у консоли и API (спека access-policy
		// «Заголовки безопасности HTTP»).
		Handler:           api.BaseHeaders(root),
		ReadHeaderTimeout: 5 * time.Second,
	}

	if err := config.CheckSourceURL(sourceURL); err != nil {
		slog.Warn("адрес исходников из сборки (-X main.sourceURL) отброшен, ссылка ведёт на публичный репозиторий Rivet", "err", err)
	}
	logListen(cfg)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		select {
		case <-rotated:
			return errors.New("ключ шифрования сменили, пока rivetd работал: перезапустите rivetd с новым RIVET_SECRET_KEY")
		case <-ctx.Done():
			return nil
		}
	})
	g.Go(func() error { return grpcSrv.Serve(grpcLis) })
	g.Go(func() error {
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error { engine.Run(ctx); return nil })
	g.Go(func() error { apiSrv.RunIssueIntake(ctx); return nil })
	g.Go(func() error { apiSrv.RunEpicAssessments(ctx); return nil })
	// После старта в фоне: переподписка webhook'ов на push и сверка SHA уже
	// построенных снимков архитектуры, чтобы не потерять обновление,
	// отложенное до рестарта (add-architecture-c4). Старт хостинга не ждёт.
	// Две независимые фоновые задачи: переподписка ходит на хостинг с
	// паузами и не должна задерживать сверку снимков.
	g.Go(func() error { apiSrv.ResubscribeWebhooks(ctx); return nil })
	g.Go(func() error { engine.ResumeArchitecture(ctx); return nil })
	// Задачи, которые ждут на review или merge без PR, получают его сами
	// (fix-pr-without-test-step).
	g.Go(func() error { engine.ResumePendingPRs(ctx); return nil })
	g.Go(func() error {
		<-ctx.Done()
		grpcSrv.GracefulStop()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shCtx)
	})
	return g.Wait()
}

// logListen пишет, где слушает rivetd. На loopback подсказывает, как открыть
// адрес наружу: новые умолчания закрывают установку, запущенную без
// настроек на сервере.
func logListen(cfg config.Config) {
	slog.Info("rivetd started", "http", cfg.HTTPAddr, "grpc", cfg.GRPCAddr, "version", version)
	if config.IsLoopbackAddr(cfg.HTTPAddr) {
		slog.Info("API и консоль доступны только с этой машины. Чтобы открыть их в сеть, задайте RIVET_HTTP_ADDR, например 0.0.0.0:8080")
	}
	if config.IsLoopbackAddr(cfg.GRPCAddr) {
		slog.Info("протокол runner'ов доступен только с этой машины. Для runner'ов на других хостах задайте RIVET_GRPC_ADDR и TLS (RIVET_GRPC_TLS_CERT, RIVET_GRPC_TLS_KEY)")
	}
}
