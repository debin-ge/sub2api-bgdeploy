SHELL := /bin/sh

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
LDFLAGS := -s -w -X github.com/debin-ge/sub2api-bgdeploy/internal/cli.Version=$(VERSION)
DIST_DIR := dist
COMMAND := ./cmd/bgdeploy
BINARY := sub2api-bgdeploy

.PHONY: test build build-linux-amd64 build-linux-arm64 release clean

test:
	go test ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY) $(COMMAND)

build-linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
		-ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-amd64 $(COMMAND)

build-linux-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath \
		-ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-arm64 $(COMMAND)

release: test build-linux-amd64 build-linux-arm64

clean:
	rm -rf $(DIST_DIR)
