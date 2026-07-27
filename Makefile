SHELL := /bin/sh

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
DIST_DIR := dist

.PHONY: test build build-linux-amd64 build-linux-arm64 release clean

test:
	go test ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/bgdeploy .

build-linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
		-ldflags="$(LDFLAGS)" -o $(DIST_DIR)/bgdeploy-linux-amd64 .

build-linux-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath \
		-ldflags="$(LDFLAGS)" -o $(DIST_DIR)/bgdeploy-linux-arm64 .

release: test build-linux-amd64 build-linux-arm64

clean:
	rm -rf $(DIST_DIR)
