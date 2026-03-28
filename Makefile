.PHONY: build test lint run docker-build

build:
	go build -o bin/llm-proxy ./cmd/llm-proxy

test:
	go test ./... -v -race -count=1

test-short:
	go test ./... -short

lint:
	go vet ./...

run:
	CONFIG_PATH=config.example.yaml go run ./cmd/llm-proxy

docker-build:
	docker build -t llm-proxy .
