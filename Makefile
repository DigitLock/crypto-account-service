# Loads .env when it exists and exports its variables to every command. Its content is never printed.
-include .env
export

BUF_VERSION := 1.73.0
PROTOC_GEN_GO_VERSION := v1.36.12
PROTOC_GEN_GO_GRPC_VERSION := v1.6.2
FROZEN_IMAGE := proto/frozen/cas_v1.json
MIGRATE_VERSION := 4.20.1
SQLC_VERSION := 1.31.1
GETH_VERSION := v1.17.7
OASDIFF_VERSION := v1.33.0
OPENAPI := api/openapi/card-auth.yaml
FROZEN_OPENAPI := api/openapi/frozen/card-auth.yaml

.PHONY: build run casctl fmt vet test check secrets tools proto proto-check proto-freeze crs-proto crs-proto-check buf-version plugins \
	migrate-tool migrate-url migrate-up migrate-down migrate-version sqlc-tool sqlc-generate sqlc-check \
	bindings bindings-check openapi-check openapi-freeze fixtures-record

build:
	go build -o bin/ ./cmd/...

run:
	go run ./cmd/server

# casctl on CASCTL_DATABASE_URL (owner role): make casctl ARGS="tenant list"
casctl:
	go run ./cmd/casctl $(ARGS)

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needs to run on:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test:
	go test -race -count=1 -p 1 ./...

check: fmt vet build test

# Records the committed RPC fixtures of testdata/fixtures/evm/ from a local Anvil: the head (internal/rpcfixture)
# and the scenarios of the shared connector suite (internal/connector/evm). Only this target writes them; a normal
# test run records into a temporary directory. The error_*.json files are written by hand. Anvil and forge must
# be on PATH.
fixtures-record:
	go test -count=1 -run '^TestT107_RecordFromAnvil$$' ./internal/rpcfixture -record
	go test -count=1 -run '^TestT420_RecordScenarios$$' ./internal/connector/evm -record

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
	@out="$$(git status --porcelain -- internal/grpc/pb)"; if [ -n "$$out" ]; then \
		echo "Generated code in internal/grpc/pb is not current or not committed:"; echo "$$out"; exit 1; fi
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
		buf build --exclude-source-info -o "$$tmp/cas_v1.json" && \
		cmp "$$tmp/cas_v1.json" $(FROZEN_IMAGE) || \
		{ echo "The contract differs from $(FROZEN_IMAGE). It is frozen: a change needs the owner's decision (make proto-freeze)."; exit 1; }
	buf breaking --against $(FROZEN_IMAGE)

# Writes the frozen contract image. Run only by the owner's decision: it accepts the current contract.
proto-freeze: buf-version
	@mkdir -p $(dir $(FROZEN_IMAGE))
	buf build --exclude-source-info -o $(FROZEN_IMAGE)

# Go code of the CRS contract, vendored unchanged in third_party/proto (README there): a separate template, so the
# cas.v1 module, its lint and its frozen image are not touched.

crs-proto: buf-version plugins
	PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.crs.yaml

# Generates into a temporary directory and compares, as bindings-check: the tree is not touched.
crs-proto-check: buf-version plugins
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
		PATH="$(CURDIR)/bin:$$PATH" buf generate --template buf.gen.crs.yaml -o "$$tmp" && \
		diff -r "$$tmp/internal/crs/pb" internal/crs/pb >/dev/null || \
		{ echo "Generated code in internal/crs/pb is not current: run make crs-proto"; exit 1; }

# Contract of the processor API of card-auth. It is frozen like the proto: the source must equal the frozen copy,
# and oasdiff of OASDIFF_VERSION reports any breaking change of the source against the frozen copy.

openapi-check:
	go run github.com/oasdiff/oasdiff@$(OASDIFF_VERSION) breaking --fail-on WARN $(FROZEN_OPENAPI) $(OPENAPI)
	@cmp -s $(OPENAPI) $(FROZEN_OPENAPI) || \
		{ echo "The OpenAPI contract differs from $(FROZEN_OPENAPI). It is frozen: a change needs the owner's decision (make openapi-freeze)."; exit 1; }

# Writes the frozen OpenAPI copy. Run only by the owner's decision: it accepts the current contract.
openapi-freeze:
	@mkdir -p $(dir $(FROZEN_OPENAPI))
	cp $(OPENAPI) $(FROZEN_OPENAPI)

# Database migrations with the migrate CLI on MIGRATE_DATABASE_URL (owner role). The URL is never printed:
# the commands are not echoed, and the output is filtered because migrate quotes a malformed URL.

migrate-tool:
	@v="$$(migrate -version 2>&1)"; if [ "$$v" != "v$(MIGRATE_VERSION)" ]; then \
		echo "migrate $(MIGRATE_VERSION) is required, found: $${v:-no migrate on PATH}"; exit 1; fi

migrate-url:
	@if [ -z "$$MIGRATE_DATABASE_URL" ]; then \
		echo "MIGRATE_DATABASE_URL is not set: owner role, see .env.example"; exit 1; fi

# Runs migrate with the given arguments and removes the URL and any user:password@ from its output.
MIGRATE = out="$$(migrate -path migrations -database "$$MIGRATE_DATABASE_URL" $(1) 2>&1)"; rc=$$?; \
	printf '%s\n' "$$out" | awk '{ u = ENVIRON["MIGRATE_DATABASE_URL"]; \
		while (u != "" && (i = index($$0, u)) > 0) $$0 = substr($$0, 1, i - 1) "[redacted]" substr($$0, i + length(u)); \
		gsub(/:\/\/[^:\/@ ]*:[^@ ]*@/, "://[redacted]@"); print }'; exit $$rc

migrate-up: migrate-tool migrate-url
	@$(call MIGRATE,up)

migrate-down: migrate-tool migrate-url
	@$(call MIGRATE,down 1)

migrate-version: migrate-tool migrate-url
	@$(call MIGRATE,version)

# Data access code generated by sqlc from migrations/ and internal/repository/queries/.

sqlc-tool:
	@v="$$(sqlc version 2>/dev/null)"; if [ "$$v" != "v$(SQLC_VERSION)" ]; then \
		echo "sqlc $(SQLC_VERSION) is required, found: $${v:-no sqlc on PATH}"; exit 1; fi

sqlc-generate: sqlc-tool
	sqlc generate

sqlc-check: sqlc-tool
	sqlc diff

# Go bindings of the frozen contract ABI (ABI only, no bytecode), generated by abigen of GETH_VERSION.
# The file of a contract is its name in lower case: CardSpendController -> cardspendcontroller.go.

ABIGEN := go run github.com/ethereum/go-ethereum/cmd/abigen@$(GETH_VERSION)
BINDINGS_DIR := internal/chain/bindings
CONTRACTS := CardSpendController MockUSDC

# Generates every binding into the directory $(1).
GEN_BINDINGS = for name in $(CONTRACTS); do \
		file="$$(echo $$name | tr 'A-Z' 'a-z').go"; \
		$(ABIGEN) --abi contracts/abi/$$name.json --pkg bindings --type $$name --out $(1)/$$file || exit 1; \
	done

bindings:
	@mkdir -p $(BINDINGS_DIR)
	@$(call GEN_BINDINGS,$(BINDINGS_DIR))

bindings-check:
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
		$(call GEN_BINDINGS,$$tmp); \
		for name in $(CONTRACTS); do \
			file="$$(echo $$name | tr 'A-Z' 'a-z').go"; \
			cmp -s "$$tmp/$$file" $(BINDINGS_DIR)/$$file || \
				{ echo "$(BINDINGS_DIR)/$$file is not current: run make bindings"; exit 1; }; \
		done
