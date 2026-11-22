GOARCH_LINUX ?= $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

.PHONY: build-host build-guest build-all test vet clean

build-host:
	go build -o bin/branchvm ./cmd/branchvm

build-guest:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH_LINUX) go build -ldflags="-s -w" -o bin/execd ./cmd/execd

build-all: build-host build-guest

vet:
	go vet ./...

test:
	go test -race ./...

clean:
	rm -rf bin/
