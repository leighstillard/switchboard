VERSION ?= 0.1.0
GIT_COMMIT := $(shell git rev-parse --short HEAD)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.gitCommit=$(GIT_COMMIT) -X main.buildTime=$(BUILD_TIME)

.PHONY: build test test-unit test-integration test-e2e test-all install restart clean

build:
	go build -ldflags "$(LDFLAGS)" -o switchboard ./cmd/switchboard/

test: test-unit

test-unit:
	go test ./...

test-integration:
	go test -tags integration ./test/integration/ -v -timeout 120s -count=1

test-e2e:
	go test -tags e2e ./test/e2e/ -v -timeout 180s -count=1

test-all: test-unit test-integration test-e2e

install: build
	cp switchboard ~/.local/bin/switchboard

restart: install
	systemctl --user restart switchboard

clean:
	rm -f switchboard
