STATICCHECK_VERSION := v0.7.0
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
FUZZTIME ?= 30s

.PHONY: all build test vet staticcheck fmt-check smoke bench bench-check fuzz ci demo clean

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o bin/f1d ./cmd/f1d
	go build -ldflags "$(LDFLAGS)" -o bin/f1 ./cmd/f1
	go build -ldflags "$(LDFLAGS)" -o bin/f1web ./cmd/f1web

# The race pass scales down large single-goroutine tests; the second pass
# runs them at full size (the million-overwrite GC test takes minutes on CI
# runners, hence the timeout).
test:
	go test -race -timeout 30m ./...
	go test -timeout 30m ./...

vet:
	go vet ./...

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

smoke: build
	scripts/smoke-serve.sh bin/f1d

# Benchmarks of doc/benchmarks.md: each run 5 times, into bench.txt.
# bench-check compares bench.txt with the recorded baseline (same hardware
# only) and fails on regressions above 10 %.
BENCH_BASELINE ?= testdata/bench/baseline.txt
bench:
	go test ./test -run '^$$' -bench . -benchtime 3s -count 5 -timeout 60m | tee bench.txt

bench-check: bench
	go run ./scripts/benchcmp $(BENCH_BASELINE) bench.txt

fuzz:
	go test ./internal/object -run '^$$' -fuzz FuzzDecode -fuzztime $(FUZZTIME)
	go test ./internal/pack -run '^$$' -fuzz FuzzPack -fuzztime $(FUZZTIME)
	go test ./internal/auth -run '^$$' -fuzz FuzzSigV4 -fuzztime $(FUZZTIME)
	go test ./internal/api/s3 -run '^$$' -fuzz FuzzChunked -fuzztime $(FUZZTIME)
	go test ./internal/api/git -run '^$$' -fuzz FuzzObjects -fuzztime $(FUZZTIME)
	go test ./internal/api/git -run '^$$' -fuzz FuzzPktLine -fuzztime $(FUZZTIME)
	go test ./internal/api/git -run '^$$' -fuzz FuzzReadPack -fuzztime $(FUZZTIME)
	go test ./internal/client/textmerge -run '^$$' -fuzz FuzzMerge -fuzztime $(FUZZTIME)
	go test ./internal/api/git -run '^$$' -fuzz FuzzDelta -fuzztime $(FUZZTIME)

ci: fmt-check vet staticcheck test smoke

# A demo F1 server and web viewer with Git repositories in it; see
# scripts/demo.sh. Pass options with DEMO_ARGS, e.g.
#   make demo DEMO_ARGS="--keep /tmp/f1demo trukeio/ogdl"
demo:
	scripts/demo.sh $(DEMO_ARGS)

clean:
	rm -rf bin bench.txt
