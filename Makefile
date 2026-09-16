.PHONY: build test vet fmt templ run docker clean

GO ?= go
TEMPL ?= templ

# Stamped into the binary by `--version`, the same way the release
# workflow stamps the container image.
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || echo v0.0.0-dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o elpulpo ./cmd/elpulpo

templ:
	$(TEMPL) generate ./internal/dashboard

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w cmd internal acceptance

run: build
	ELPULPO_CONFIG=./elpulpo.yaml ELPULPO_DATA_DIR=./data ./elpulpo

docker:
	docker build -t elpulpo:latest .

clean:
	rm -rf ./data ./elpulpo
