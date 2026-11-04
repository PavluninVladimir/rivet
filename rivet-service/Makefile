.PHONY: build test lint fuzz up down migrate proto ui e2e-stand

# Версия сборки — в бинарники через ldflags (состояние установки).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -X main.version=$(VERSION)
GO ?= go

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
	./internal/api:FuzzBearerSecret \
	./internal/scm:FuzzParseRepoURL \
	./internal/policy:FuzzProcessDocument \
	./internal/discovery:FuzzSplitInterview \
	./internal/discovery:FuzzCheckSummary \
	./internal/discovery:FuzzCheckSynthesis \
	./internal/discovery:FuzzQuoteLeak \
	./internal/ops:FuzzOpsParsers

# go test -fuzz гоняет один таргет за прогон, поэтому цикл. Падение
# «context deadline exceeded» без записанного входа — гонка Go на остановке
# фаззинга (воркер ещё в вызове на дедлайне), а не находка: такой таргет
# запускается ещё раз. Настоящая находка (Failing input) валит сразу.
fuzz:
	@for t in $(FUZZ_TARGETS); do \
		pkg=$${t%%:*}; name=$${t##*:}; \
		for try in 1 2; do \
			echo "== fuzz $$pkg $$name ($(FUZZ_TIME)), попытка $$try"; \
			out=$$($(GO) test $$pkg -run '^$$' -fuzz "^$$name$$" -fuzztime $(FUZZ_TIME) 2>&1); rc=$$?; \
			echo "$$out"; \
			[ $$rc -eq 0 ] && break; \
			if [ $$try -eq 1 ] && echo "$$out" | grep -q 'context deadline exceeded' \
				&& ! echo "$$out" | grep -q 'Failing input written to'; then \
				echo "== $$name: остановка по дедлайну без находки, повтор"; continue; \
			fi; \
			exit 1; \
		done; \
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
