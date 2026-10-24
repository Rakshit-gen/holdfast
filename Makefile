VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test race fuzz bench interop
build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/holdfast ./cmd/holdfast

install:
	go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/holdfast

test:
	go test ./...

race:
	go test -race ./...

fuzz:
	go test -run XXX -fuzz FuzzRead -fuzztime 1m ./bgp

bench: build
	nice -n 19 ./bin/holdfast bench

interop:
	scripts/interop.sh
