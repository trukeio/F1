STATICCHECK_VERSION := v0.7.0
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
FUZZTIME ?= 30s

.PHONY: all build test vet staticcheck fmt-check smoke bench fuzz ci clean

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o bin/f1d ./cmd/f1d

# The race pass scales down large single-goroutine tests; the second pass
# runs them at full size.
test:
	go test -race ./...
	go test ./...

vet:
	go vet ./...

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

smoke: build
	scripts/smoke-serve.sh bin/f1d

bench:
	go test -run '^$$' -bench . -benchtime 3x ./...

fuzz:
	go test ./internal/object -run '^$$' -fuzz FuzzDecode -fuzztime $(FUZZTIME)
	go test ./internal/pack -run '^$$' -fuzz FuzzPack -fuzztime $(FUZZTIME)
	go test ./internal/auth -run '^$$' -fuzz FuzzSigV4 -fuzztime $(FUZZTIME)
	go test ./internal/api/s3 -run '^$$' -fuzz FuzzChunked -fuzztime $(FUZZTIME)
	go test ./internal/api/git -run '^$$' -fuzz FuzzObjects -fuzztime $(FUZZTIME)
	go test ./internal/api/git -run '^$$' -fuzz FuzzPktLine -fuzztime $(FUZZTIME)
	go test ./internal/api/git -run '^$$' -fuzz FuzzReadPack -fuzztime $(FUZZTIME)

ci: fmt-check vet staticcheck test smoke

clean:
	rm -rf bin
