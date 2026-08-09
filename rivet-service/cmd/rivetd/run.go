package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/PavluninVladimir/rivet/internal/api"
	"github.com/PavluninVladimir/rivet/internal/blob"
	"github.com/PavluninVladimir/rivet/internal/config"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
	"github.com/PavluninVladimir/rivet/internal/stream"
	"github.com/PavluninVladimir/rivet/internal/webui"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

func run(ctx context.Context, cfg config.Config) error {
	if err := store.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

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
	engine := orchestrator.New(st, adapter, bl, reg, cfg.RunnerHeartbeatTimeout)

	grpcSrv := grpc.NewServer()
	pb.RegisterRunnerServiceServer(grpcSrv, &stream.Server{
		St: st, Engine: engine, Reg: reg, Hub: hub,
		HeartbeatInterval: cfg.RunnerHeartbeatTimeout / 3,
	})
	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}

	var pl *planner.Planner
	switch cfg.LLMProvider {
	case "deepseek":
		if cfg.DeepSeekAPIKey != "" {
			pl = planner.NewDeepSeek(cfg.DeepSeekAPIKey, cfg.DeepSeekModel)
		}
	case "anthropic":
		if cfg.AnthropicAPIKey != "" {
			pl = planner.New(cfg.AnthropicAPIKey)
		}
	}
	root := http.NewServeMux()
	root.Handle("/api/", (&api.Server{St: st, Engine: engine, Hub: hub, Planner: pl}).Handler())
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
