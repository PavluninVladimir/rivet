// Package config собирает конфигурацию rivetd из переменных окружения.
package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	// DatabaseURL — DSN PostgreSQL, например postgres://rivet:rivet@localhost:5432/rivet.
	DatabaseURL string
	// HTTPAddr — адрес клиентского API (REST + SSE).
	HTTPAddr string
	// GRPCAddr — адрес протокола runner'ов.
	GRPCAddr string

	// S3 — объектное хранилище транскриптов (MinIO в dev).
	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	S3Bucket    string
	S3UseSSL    bool

	// GitHubToken — токен бот-идентичности для SCM-адаптера.
	GitHubToken string
	// SCM — провайдер хостинга: github (по умолчанию) или fake (e2e-стенды).
	SCM string
	// AnthropicAPIKey — ключ модели для декомпозиции Epic.
	AnthropicAPIKey string

	// RunnerHeartbeatTimeout — тишина от runner'а, после которой он offline.
	RunnerHeartbeatTimeout time.Duration
	// DefaultAttemptLimit — лимит попыток задачи по умолчанию (спека orchestration).
	DefaultAttemptLimit int
}

func FromEnv() (Config, error) {
	c := Config{
		DatabaseURL:            getenv("RIVET_DATABASE_URL", "postgres://rivet:rivet@localhost:5432/rivet?sslmode=disable"),
		HTTPAddr:               getenv("RIVET_HTTP_ADDR", ":8080"),
		GRPCAddr:               getenv("RIVET_GRPC_ADDR", ":8090"),
		S3Endpoint:             getenv("RIVET_S3_ENDPOINT", "localhost:9000"),
		S3AccessKey:            getenv("RIVET_S3_ACCESS_KEY", "rivet"),
		S3SecretKey:            getenv("RIVET_S3_SECRET_KEY", "rivetsecret"),
		S3Bucket:               getenv("RIVET_S3_BUCKET", "rivet"),
		S3UseSSL:               os.Getenv("RIVET_S3_SSL") == "true",
		GitHubToken:            os.Getenv("RIVET_GITHUB_TOKEN"),
		SCM:                    getenv("RIVET_SCM", "github"),
		AnthropicAPIKey:        os.Getenv("ANTHROPIC_API_KEY"),
		RunnerHeartbeatTimeout: 90 * time.Second,
		DefaultAttemptLimit:    3,
	}
	if v := os.Getenv("RIVET_RUNNER_HEARTBEAT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("RIVET_RUNNER_HEARTBEAT_TIMEOUT: %w", err)
		}
		c.RunnerHeartbeatTimeout = d
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
