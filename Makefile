.PHONY: build test lint run docker

build:
	go build -o bin/coredevs ./cmd/coredevs

test:
	go test -race ./...

lint:
	golangci-lint run --timeout=10m

run:
	go run ./cmd/coredevs --config config.yaml

docker:
	docker build -t ghcr.io/ethpandaops/coredevs:dev .
