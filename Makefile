.PHONY: build check test fmt

build:                ## bin/sol
	go build -trimpath -o bin/sol ./cmd/sol

test:
	go test -race ./...

check:                ## must pass before a ticket is done
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; echo "gofmt needed"; exit 1; fi
	go vet ./...
	go test -race ./...

fmt:
	gofmt -w .
