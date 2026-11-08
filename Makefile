# Common tasks. Everything here is plain `go` underneath; the Makefile is a convenience.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-bundled test test-tor check fmt lint clean hooks

build:                      ## build bin/sol (uses tor from PATH)
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/sol ./cmd/sol

build-bundled:              ## build bin/sol with tor embedded (run scripts/fetch-binaries.sh first)
	go build -trimpath -tags bundled -ldflags "$(LDFLAGS)" -o bin/sol ./cmd/sol

test:                       ## unit + in-process end-to-end tests (no Tor needed)
	go test -race ./...

test-tor:                   ## end-to-end test over the real Tor network (needs tor, takes minutes)
	SOL_TEST_TOR=1 go test ./internal/e2e -run Tor -v -timeout 20m

fmt:
	gofmt -w .

lint:
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

check:                      ## what CI runs
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	$(MAKE) lint test

hooks:                      ## run `make check` automatically before every git push (once per clone)
	git config core.hooksPath .githooks
	@echo "pre-push hook enabled: every push runs make check first"

clean:
	rm -rf bin dist
