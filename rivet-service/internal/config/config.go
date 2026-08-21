// Package config собирает конфигурацию rivetd из переменных окружения.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	// DatabaseURL — DSN PostgreSQL, например postgres://rivet:rivet@localhost:5432/rivet.
	DatabaseURL string
	// HTTPAddr — адрес клиентского API (REST + SSE).
	HTTPAddr string
	// GRPCAddr — адрес протокола runner'ов. Регистрация аутентифицируется
	// токеном (спека runners); без TLS токен идёт открытым текстом, поэтому
	// порт без GRPCTLSCert обязан быть закрыт сетевым периметром (loopback
	// или приватная сеть, как в docker-compose).
	GRPCAddr string
	// GRPCTLSCert/GRPCTLSKey — PEM-файлы сертификата и ключа: включают TLS
	// на протоколе runner'ов. Клиентские сертификаты (mTLS) не проверяются.
	GRPCTLSCert string
	GRPCTLSKey  string

	// S3 — объектное хранилище транскриптов (MinIO в dev).
	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	S3Bucket    string
	S3UseSSL    bool

	// GitHubToken — токен бот-идентичности для SCM-адаптера.
	GitHubToken string
	// GitHubWebhookSecret — секрет подписи X-Hub-Signature-256 входящих
	// webhook'ов (спека scm-integration). Пустой секрет выключает приём
	// webhook'ов (fail-closed).
	GitHubWebhookSecret string
	// SCM — запасной провайдер хостинга для проектов без собственных
	// учётных данных: github (по умолчанию) или fake (e2e-стенды).
	SCM string
	// SecretKey — ключ шифрования учётных данных хостингов (base64, 32 байта).
	SecretKey string
	// PublicURL — внешний адрес установки; нужен для регистрации webhook.
	PublicURL string
	// LLMProvider — провайдер модели декомпозиции: anthropic или deepseek.
	// Без RIVET_LLM_PROVIDER выбирается по наличию ключа, при обоих ключах anthropic.
	LLMProvider string
	// AnthropicAPIKey — ключ модели для декомпозиции Epic.
	AnthropicAPIKey string
	// DeepSeekAPIKey и DeepSeekModel — DeepSeek API (по умолчанию deepseek-v4-flash).
	DeepSeekAPIKey string
	DeepSeekModel  string

	// AdminLogin/AdminPassword — bootstrap первого администратора при пустой
	// таблице пользователей (спека access-policy «Аутентификация пользователей»).
	AdminLogin    string
	AdminPassword string
	// TrustProxy — доверять X-Forwarded-Proto от прокси при выставлении
	// Secure-cookie (rivetd за TLS-терминирующим reverse proxy).
	TrustProxy bool

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
		GRPCTLSCert:            os.Getenv("RIVET_GRPC_TLS_CERT"),
		GRPCTLSKey:             os.Getenv("RIVET_GRPC_TLS_KEY"),
		S3Endpoint:             getenv("RIVET_S3_ENDPOINT", "localhost:9000"),
		S3AccessKey:            getenv("RIVET_S3_ACCESS_KEY", "rivet"),
		S3SecretKey:            getenv("RIVET_S3_SECRET_KEY", "rivetsecret"),
		S3Bucket:               getenv("RIVET_S3_BUCKET", "rivet"),
		S3UseSSL:               os.Getenv("RIVET_S3_SSL") == "true",
		GitHubToken:            os.Getenv("RIVET_GITHUB_TOKEN"),
		GitHubWebhookSecret:    os.Getenv("RIVET_GITHUB_WEBHOOK_SECRET"),
		SCM:                    getenv("RIVET_SCM", "github"),
		SecretKey:              os.Getenv("RIVET_SECRET_KEY"),
		PublicURL:              strings.TrimSuffix(os.Getenv("RIVET_PUBLIC_URL"), "/"),
		LLMProvider:            os.Getenv("RIVET_LLM_PROVIDER"),
		AnthropicAPIKey:        os.Getenv("ANTHROPIC_API_KEY"),
		DeepSeekAPIKey:         os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekModel:          os.Getenv("RIVET_DEEPSEEK_MODEL"),
		AdminLogin:             os.Getenv("RIVET_ADMIN_LOGIN"),
		AdminPassword:          os.Getenv("RIVET_ADMIN_PASSWORD"),
		TrustProxy:             os.Getenv("RIVET_TRUST_PROXY") == "1",
		RunnerHeartbeatTimeout: 90 * time.Second,
		DefaultAttemptLimit:    3,
	}
	if c.LLMProvider == "" {
		if c.AnthropicAPIKey != "" {
			c.LLMProvider = "anthropic"
		} else if c.DeepSeekAPIKey != "" {
			c.LLMProvider = "deepseek"
		}
	}
	if (c.GRPCTLSCert == "") != (c.GRPCTLSKey == "") {
		return c, fmt.Errorf("RIVET_GRPC_TLS_CERT и RIVET_GRPC_TLS_KEY задаются вместе")
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
