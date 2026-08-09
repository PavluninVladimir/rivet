.PHONY: build test lint up down migrate proto

build:
	go build ./...

test:
	go test ./...

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
