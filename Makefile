# Developer entry points. Everything here is local; nothing deploys anywhere.

SHELL := /bin/bash
TOOLS := $(CURDIR)/.tools/bin
ENV   := set -a && . .dev/local.env && set +a

.PHONY: help tools proto infra-up infra-down dev-setup migrate topics \
        run-ledger run-identity run-lending run-sim \
        test test-unit fmt lint vuln check bench

help:
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## /\t/'

tools: ## install pinned build and analysis tools into .tools/
	@mkdir -p $(TOOLS)
	GOBIN=$(TOOLS) go install github.com/bufbuild/buf/cmd/buf@v1.73.0
	GOBIN=$(TOOLS) go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	GOBIN=$(TOOLS) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
	GOBIN=$(TOOLS) go install honnef.co/go/tools/cmd/staticcheck@2026.2.1
	GOBIN=$(TOOLS) go install golang.org/x/vuln/cmd/govulncheck@latest

proto: ## lint protobuf and regenerate Go code
	$(TOOLS)/buf lint
	$(TOOLS)/buf generate

infra-up: ## start local PostgreSQL, Kafka and Redis
	docker compose -f deploy/docker-compose.yml up -d --wait postgres kafka redis

infra-down: ## stop local infrastructure and delete its data
	docker compose -f deploy/docker-compose.yml down -v

dev-setup: ## generate local certificates, keys and .dev/local.env
	APP_ENV=local go run ./cmd/devsetup init

migrate: ## apply migrations for all services and seed the dev funding account
	$(ENV) && go run ./cmd/ledgerd migrate && go run ./cmd/ledgerd devseed
	$(ENV) && go run ./cmd/identityd migrate
	$(ENV) && go run ./cmd/lendingd migrate

topics: ## create local Kafka topics
	$(ENV) && go run ./cmd/devsetup topics

run-ledger: ## run the ledger service
	$(ENV) && go run ./cmd/ledgerd serve

run-identity: ## run the identity service
	$(ENV) && go run ./cmd/identityd serve

run-lending: ## run the lending service
	$(ENV) && go run ./cmd/lendingd serve

run-sim: ## run the provider simulator
	$(ENV) && go run ./cmd/providersim

test-unit: ## tests that need no infrastructure
	go test -race -count=1 ./platform/money/... ./platform/bizdate/... ./platform/config/... ./platform/authn/... \
		./services/ledger/domain/... ./services/identity/domain/... ./services/identity/secrets/... \
		./services/lending/domain/... ./services/lending/config/...

test: ## all tests, with the race detector; requires `make infra-up`
	REQUIRE_INTEGRATION=1 go test -race -count=1 -timeout 20m ./...

fmt: ## fail if any file is not gofmt-formatted
	@out=$$(gofmt -l $$(find . -name '*.go' -not -path './gen/*' -not -path './.tools/*')); \
	if [ -n "$$out" ]; then echo "not formatted:"; echo "$$out"; exit 1; fi

lint: fmt ## vet, staticcheck and protobuf lint
	go vet ./...
	$(TOOLS)/staticcheck ./...
	$(TOOLS)/buf lint

vuln: ## check dependencies for known vulnerabilities
	./scripts/vulncheck.sh

check: lint vuln test ## everything CI runs

bench: ## run the loan journey benchmark against locally running services
	$(ENV) && go run ./cmd/loanbench -customers 200 -concurrency 16
