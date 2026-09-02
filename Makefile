.PHONY: build test vet fmt templ run docker clean

GO ?= go
TEMPL ?= templ

build:
	$(GO) build ./...

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
