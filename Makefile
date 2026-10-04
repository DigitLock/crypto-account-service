# Loads .env when it exists and exports its variables to every command. Its content is never printed.
-include .env
export

BUF_VERSION := 1.73.0
PROTOC_GEN_GO_VERSION := v1.36.12
PROTOC_GEN_GO_GRPC_VERSION := v1.6.2
FROZEN_IMAGE := proto/frozen/cas_v1.json

.PHONY: build run fmt vet test check secrets tools proto proto-check proto-freeze buf-version plugins

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

# Contract tooling. buf is installed separately at BUF_VERSION; the plugins go into ./bin.

buf-version:
	@v="$$(buf --version 2>/dev/null)"; if [ "$$v" != "$(BUF_VERSION)" ]; then \
		echo "buf $(BUF_VERSION) is required, found: $${v:-no buf on PATH}"; exit 1; fi

tools:
	GOBIN=$(CURDIR)/bin go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(CURDIR)/bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

# The pinned plugins must be in ./bin: buf generate would otherwise pick any version on PATH.
plugins:
	@test "$$(bin/protoc-gen-go --version 2>/dev/null)" = "protoc-gen-go $(PROTOC_GEN_GO_VERSION)" || \
		{ echo "protoc-gen-go $(PROTOC_GEN_GO_VERSION) is not in ./bin: run make tools"; exit 1; }
	@test "$$(bin/protoc-gen-go-grpc --version 2>/dev/null)" = "protoc-gen-go-grpc $(PROTOC_GEN_GO_GRPC_VERSION:v%=%)" || \
		{ echo "protoc-gen-go-grpc $(PROTOC_GEN_GO_GRPC_VERSION) is not in ./bin: run make tools"; exit 1; }

proto: buf-version plugins
	buf lint
	PATH="$(CURDIR)/bin:$$PATH" buf generate

proto-check: buf-version plugins
	buf lint
	buf format -d --exit-code
	PATH="$(CURDIR)/bin:$$PATH" buf generate
	git diff --exit-code -- internal/grpc/pb
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
		buf build --exclude-source-info -o "$$tmp/cas_v1.json" && \
		cmp "$$tmp/cas_v1.json" $(FROZEN_IMAGE) || \
		{ echo "The contract differs from $(FROZEN_IMAGE). It is frozen: a change needs the owner's decision (make proto-freeze)."; exit 1; }
	buf breaking --against $(FROZEN_IMAGE)

# Writes the frozen contract image. Run only by the owner's decision: it accepts the current contract.
proto-freeze: buf-version
	@mkdir -p $(dir $(FROZEN_IMAGE))
	buf build --exclude-source-info -o $(FROZEN_IMAGE)
