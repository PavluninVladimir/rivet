package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/PavluninVladimir/rivet/internal/api"
	"github.com/PavluninVladimir/rivet/internal/blob"
	"github.com/PavluninVladimir/rivet/internal/config"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
	"github.com/PavluninVladimir/rivet/internal/stream"
	"github.com/PavluninVladimir/rivet/internal/webui"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

func run(ctx context.Context, cfg config.Config) error {
	// Конфиг проверяем до миграций и listener'ов, чтобы падать без side effects.
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

	// Fail-fast до listener'ов: без владельца установка не поднимается.
	if err := st.Bootstrap(ctx, cfg.AdminLogin, cfg.AdminPassword); err != nil {
		return err
	}

	bl, err := blob.New(cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3UseSSL)
	if err != nil {
		return err
	}
	if err := bl.EnsureBucket(ctx); err != nil {
		slog.Warn("minio недоступен — транскрипты не будут сохраняться", "err", err)
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
	engine.Policy = polEngine
	// RIVET_SCM=fake — установка без настоящих хостингов (e2e-стенд).
	engine.Adapters.Force = cfg.SCM == "fake"

	// Протокол runner'ов: регистрация аутентифицируется токеном на обоих
	// RPC; TLS включается парой PEM-файлов (спека runners).
	rs := &stream.Server{
		St: st, Engine: engine, Reg: reg, Hub: hub,
		HeartbeatInterval: cfg.RunnerHeartbeatTimeout / 3,
	}
	grpcOpts := []grpc.ServerOption{
		grpc.UnaryInterceptor(rs.UnaryAuth()),
		grpc.StreamInterceptor(rs.StreamAuth()),
	}
	if cfg.GRPCTLSCert != "" {
		creds, err := credentials.NewServerTLSFromFile(cfg.GRPCTLSCert, cfg.GRPCTLSKey)
		if err != nil {
			return fmt.Errorf("TLS протокола runner'ов: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
	} else {
		slog.Warn("протокол runner'ов без TLS: порт должен быть закрыт периметром")
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
		StartedAt: time.Now(), GRPCAddr: cfg.GRPCAddr, GRPCTLS: cfg.GRPCTLSCert != "",
		Secrets: box, PublicURL: cfg.PublicURL, Policy: polEngine,
		WebhookSecret: cfg.GitHubWebhookSecret,
		TrustProxy:    cfg.TrustProxy,
	}
	if err := apiSrv.ReloadPlanner(ctx); err != nil {
		return err
	}
	root := http.NewServeMux()
	root.Handle("/api/", apiSrv.Handler())
	root.Handle("/", webui.Handler())
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("rivetd started", "http", cfg.HTTPAddr, "grpc", cfg.GRPCAddr)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return grpcSrv.Serve(grpcLis) })
	g.Go(func() error {
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error { engine.Run(ctx); return nil })
	g.Go(func() error {
		<-ctx.Done()
		grpcSrv.GracefulStop()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shCtx)
	})
	return g.Wait()
}
