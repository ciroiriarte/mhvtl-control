GO ?= $(HOME)/.local/go/bin/go
MODDIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

.PHONY: all build test vet fmt fmt-check

all: build

build:
	cd $(MODDIR) && $(GO) build -o bin/mhvtld ./cmd/mhvtld

test:
	cd $(MODDIR) && $(GO) test ./...

vet:
	cd $(MODDIR) && $(GO) vet ./...

fmt:
	cd $(MODDIR) && $(GO) fmt ./...

fmt-check:
	cd $(MODDIR) && gofmt -l .
