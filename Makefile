VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: run build test test-redis race bench sweep lint docker up down

run: ## run with zero config (mock model, memory cache)
	RECURA_LOG=text RECURA_CONFIG=examples/config.compose.json go run ./cmd/recura

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/recura ./cmd/recura

test:
	go test ./...

test-redis: ## needs redis on localhost:6379
	RECURA_TEST_REDIS=redis://localhost:6379/15 go test -count=1 ./...

race:
	go test -race ./...

bench: ## replay a realistic workload through a real server
	go run ./cmd/recurabench

sweep: ## compare similarity thresholds
	go run ./cmd/recurabench -sweep

lint:
	gofmt -l . | (! grep .) && go vet ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t recura:$(VERSION) .

up:
	docker compose up --build

down:
	docker compose down
