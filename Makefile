.PHONY: build test lint vet clean compose-up compose-down mock

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMPOSE ?= $(shell command -v docker-compose 2>/dev/null || echo "docker compose")

build:
	CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(VERSION)" -o bin/agent ./cmd/agent
	CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/mockbackend ./cmd/mockbackend

test:
	go test ./... -v -count=1

vet:
	go vet ./...

lint: vet

clean:
	rm -rf bin/

compose-up:
	$(COMPOSE) up -d --build

compose-down:
	$(COMPOSE) down

mock: build
	@echo "Starting mock backend on :9999..."
	@echo "Set BACKEND_URL=http://localhost:9999 in .env"
	./bin/mockbackend