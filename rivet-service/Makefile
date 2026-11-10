.PHONY: build test lint fuzz up down image migrate proto ui e2e-stand third-party

# Сведения о сборке попадают в бинарники через ldflags: версия для состояния
# установки, версия, коммит и адрес исходников для ссылки «Исходный код»
# (GET /api/v1/about). SOURCE_URL задаёт форк, пусто значит адрес по умолчанию
# из cmd/rivetd.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
SOURCE_URL ?=
LDFLAGS = -X main.version=$(VERSION) -X main.commit=$(COMMIT)
ifneq ($(SOURCE_URL),)
LDFLAGS += -X main.sourceURL=$(SOURCE_URL)
endif
GO ?= go

# Бинарники rivetd, rivet-runner и rivet (и тестовый fake-llm) в bin/ со
# сведениями о сборке. go build ./... с несколькими main-пакетами только
# проверяет сборку и выбрасывает результат.
build:
	go build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

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
	./internal/scm:FuzzGiteaParse \
	./internal/policy:FuzzProcessDocument \
	./internal/discovery:FuzzSplitInterview \
	./internal/discovery:FuzzCheckSummary \
	./internal/discovery:FuzzCheckSynthesis \
	./internal/discovery:FuzzQuoteLeak \
	./internal/ops:FuzzOpsParsers \
	./internal/infra/ruvds:FuzzRUVDSParse

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

# Образ rivetd с версией и коммитом сборки: в контекст Docker .git не
# входит, поэтому они передаются аргументами.
image:
	docker compose build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		$(if $(SOURCE_URL),--build-arg SOURCE_URL=$(SOURCE_URL)) rivetd

down:
	docker compose down

migrate:
	go run ./cmd/rivet migrate

proto:
	protoc --go_out=. --go_opt=module=github.com/PavluninVladimir/rivet \
		--go-grpc_out=. --go-grpc_opt=module=github.com/PavluninVladimir/rivet \
		pkg/protocol/rivet.proto

# После make ui выполните make third-party: раздел консоли в NOTICE и
# THIRD_PARTY берётся из встроенной сборки, CI сверяет их.
ui:
	cd ../rivet-web && npm run build
	rm -rf internal/webui/dist && cp -r ../rivet-web/dist internal/webui/dist

e2e-stand:
	sh scripts/e2e-stand.sh

# NOTICE и THIRD_PARTY в корне репозитория: лицензии Go-зависимостей
# (go-licenses) и пакетов встроенной консоли (rivet-web/dist после
# npm run build). CI сверяет файлы тем же скриптом с --check.
third-party:
	python3 scripts/third_party.py
