VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test bench interop
build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/holdfast ./cmd/holdfast

install:
	go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/holdfast

test:
	go test ./...

bench: build
	nice -n 19 ./bin/holdfast bench

interop:
	scripts/interop.sh
