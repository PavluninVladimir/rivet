// e2e-register — тестовый gRPC-клиент e2e (rivet-e2e, сценарий отказа
// стороннего агента, add-native-agent): регистрирует runner с агентом не
// из каталога и печатает ответ control plane строкой JSON. Настоящий
// rivet-runner сторонний агент не запускает вовсе (отказ на старте),
// поэтому отказ регистрации на стороне control plane и событие
// runner.rejected проверяются этим клиентом. Им же e2e проверяет отказ по
// токену, выпущенному для другого runner'а (add-runner-identity). Только
// тестовая инфраструктура стенда, в сборку продукта не входит.
//
// Код выхода 0 — control plane ответил (принял или отказал, итог в JSON),
// 1 — ответа нет: неверные флаги, нет токена, control plane недоступен.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

type result struct {
	Accepted bool   `json:"accepted"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

func main() {
	addr := flag.String("addr", "localhost:8291", "адрес gRPC control plane стенда")
	tokenFile := flag.String("token-file", "", "файл с токеном регистрации runner'ов; пусто — $E2E_STAND_DIR/runner-token")
	id := flag.String("id", "e2e-third-party", "идентификатор runner'а")
	agent := flag.String("agent", "claude-code", "агент runner'а")
	proto := flag.String("protocol", "12", "версия протокола (protocol_version из GET /system/status)")
	flag.Parse()

	if *tokenFile == "" {
		dir := os.Getenv("E2E_STAND_DIR")
		if dir == "" {
			dir = "/tmp/rivet-e2e-stand"
		}
		*tokenFile = filepath.Join(dir, "runner-token")
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		fail("токен регистрации: %v", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		fail("токен регистрации пуст: %s", *tokenFile)
	}

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fail("подключение к %s: %v", *addr, err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	resp, err := pb.NewRunnerServiceClient(conn).Register(ctx, &pb.RegisterRequest{
		RunnerId: *id, Agent: *agent, Host: "e2e-register", Capabilities: []string{"coding"},
		ProtocolVersion: *proto, Depth: "full",
	})
	out := result{Code: codes.OK.String()}
	if err != nil {
		st := status.Convert(err)
		if st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded {
			fail("control plane недоступен: %v", err)
		}
		out.Code, out.Message = st.Code().String(), st.Message()
	} else {
		out.Accepted, out.Message = resp.GetAccepted(), resp.GetMessage()
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fail("вывод: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "e2e-register: "+format+"\n", args...)
	os.Exit(1)
}
