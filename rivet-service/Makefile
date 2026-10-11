.PHONY: build test lint fuzz up down migrate proto ui e2e-stand

# Версия сборки — в бинарники через ldflags (состояние установки).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -X main.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" ./...

test:
	go test ./...

# Бюджет на один fuzz-таргет; локально можно поднять: make fuzz FUZZ_TIME=10m
FUZZ_TIME ?= 30s
FUZZ_TARGETS = \
	./internal/planner:FuzzParsePlan \
	./internal/planner:FuzzValidatePlan \
	./internal/store:FuzzValidateDAG \
	./internal/orchestrator:FuzzPRNumber \
	./internal/api:FuzzWebhookSignature \
	./internal/api:FuzzWebhookPayload \
	./internal/runner:FuzzTail \
	./internal/runner:FuzzParseUsage \
	./internal/api:FuzzBearerSecret

# go test -fuzz гоняет один таргет за прогон, поэтому цикл.
fuzz:
	@for t in $(FUZZ_TARGETS); do \
		pkg=$${t%%:*}; name=$${t##*:}; \
		echo "== fuzz $$pkg $$name ($(FUZZ_TIME))"; \
		go test $$pkg -run '^$$' -fuzz "^$$name$$" -fuzztime $(FUZZ_TIME) || exit 1; \
	done

lint:
	golangci-lint run

up:
	docker compose up -d

down:
	docker compose down

migrate:
	go run ./cmd/rivet migrate

proto:
	protoc --go_out=. --go_opt=module=github.com/PavluninVladimir/rivet \
		--go-grpc_out=. --go-grpc_opt=module=github.com/PavluninVladimir/rivet \
		pkg/protocol/rivet.proto

ui:
	cd ../rivet-web && npm run build
	rm -rf internal/webui/dist && cp -r ../rivet-web/dist internal/webui/dist

e2e-stand:
	sh scripts/e2e-stand.sh
