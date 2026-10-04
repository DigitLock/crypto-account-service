# Loads .env when it exists and exports its variables to every command. Its content is never printed.
-include .env
export

.PHONY: build run fmt vet test check secrets

build:
	go build -o bin/ ./cmd/...

run:
	go run ./cmd/server

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needs to run on:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test:
	go test -race -p 1 ./...

check: fmt vet build test

secrets:
	gitleaks git --redact --no-banner .
	gitleaks git --pre-commit --staged --redact --no-banner .
