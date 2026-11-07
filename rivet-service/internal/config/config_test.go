package config

import (
	"strings"
	"testing"
)

// Корни клиентских сертификатов без TLS протокола runner'ов: ошибка
// конфигурации (add-runner-identity): mTLS без TLS не бывает.
func TestGRPCClientCARequiresTLS(t *testing.T) {
	t.Setenv("RIVET_GRPC_TLS_CERT", "")
	t.Setenv("RIVET_GRPC_TLS_KEY", "")
	t.Setenv("RIVET_GRPC_CLIENT_CA", "/etc/rivet/runners-ca.pem")
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "RIVET_GRPC_CLIENT_CA требует TLS") {
		t.Fatalf("корни клиентов без TLS: %v", err)
	}
	t.Setenv("RIVET_GRPC_TLS_CERT", "/etc/rivet/grpc.pem")
	t.Setenv("RIVET_GRPC_TLS_KEY", "/etc/rivet/grpc-key.pem")
	c, err := FromEnv()
	if err != nil || c.GRPCClientCA != "/etc/rivet/runners-ca.pem" {
		t.Fatalf("корни клиентов с TLS: %v %+v", err, c.GRPCClientCA)
	}
	t.Setenv("RIVET_GRPC_CLIENT_CA", "")
	if c, err := FromEnv(); err != nil || c.GRPCClientCA != "" {
		t.Fatalf("без корней клиентов: %v", err)
	}
}
