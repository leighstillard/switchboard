.PHONY: build test test-unit test-integration test-e2e clean

build:
	go build -o switchboard ./cmd/switchboard/

test: test-unit

test-unit:
	go test ./...

test-integration:
	go test -tags integration ./test/integration/ -v -timeout 120s -count=1

test-e2e:
	go test -tags e2e ./test/e2e/ -v -timeout 180s -count=1

test-all: test-unit test-integration test-e2e

clean:
	rm -f switchboard
