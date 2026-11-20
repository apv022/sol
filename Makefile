.PHONY: build build-bundled fetch-tor check test fmt browser-test ui-e2e

build:                ## bin/sol
	go build -trimpath -o bin/sol ./cmd/sol

fetch-tor:            ## download + verify the bundled tor (Tor Browser expert bundles) into internal/runtime/bin
	sh scripts/fetch-tor.sh

build-bundled:        ## bin/sol with tor embedded (fetches it first when this platform's folder is missing)
	@[ -d internal/runtime/bin/$$(go env GOOS)-$$(go env GOARCH) ] || sh scripts/fetch-tor.sh
	go build -trimpath -tags bundled -o bin/sol ./cmd/sol

test:
	go test -race ./...

check:                ## must pass before a ticket is done
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; echo "gofmt needed"; exit 1; fi
	go vet ./...
	go test -race ./...

fmt:
	gofmt -w .

browser-test:         ## browser JS suites in headless Firefox (no Node)
	go run ./cmd/browsertest

ui-e2e:               ## the Sol app against two real daemons in headless Firefox (SOL_UI_SHOTS=dir saves screenshots)
	SOL_UI_E2E=1 go test -count=1 -v ./test/uie2e
