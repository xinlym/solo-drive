GO ?= go
NPM ?= npm

.PHONY: all frontend build test check run docker-build
all: build

frontend:
	cd web && $(NPM) ci && $(NPM) run build

build: frontend
	CGO_ENABLED=0 $(GO) build -trimpath -o solodrive ./cmd/solodrive

test: frontend
	$(GO) test ./...

check: frontend
	bash -n scripts/init.sh
	$(GO) vet ./...

run: build
	./solodrive

docker-build:
	docker compose build
