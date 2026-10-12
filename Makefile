STATICCHECK_VERSION := v0.7.0
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build test vet staticcheck fmt-check smoke ci clean

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o bin/f1d ./cmd/f1d

test:
	go test -race ./...

vet:
	go vet ./...

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

smoke: build
	scripts/smoke-serve.sh bin/f1d

ci: fmt-check vet staticcheck test smoke

clean:
	rm -rf bin
