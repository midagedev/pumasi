# `make` builds ./dist/pumasi for this machine and the linux builds that
# `pumasi up` copies to the hosts. All static (CGO off).
GO      ?= go
FLAGS   := -trimpath -ldflags "-s -w"
export CGO_ENABLED = 0

.PHONY: all test chaos clean
all:
	$(GO) build $(FLAGS) -o dist/pumasi ./cmd/pumasi
	GOOS=linux GOARCH=amd64 $(GO) build $(FLAGS) -o dist/pumasi-linux-amd64 ./cmd/pumasi
	GOOS=linux GOARCH=arm64 $(GO) build $(FLAGS) -o dist/pumasi-linux-arm64 ./cmd/pumasi

test:
	$(GO) vet ./...
	$(GO) test ./...

# Provokes the failures of docs/robustness.md on real hosts; see scripts/chaos.sh.
chaos: all
	scripts/chaos.sh

clean:
	rm -rf dist
