GO ?= $(HOME)/.local/go/bin/go
NODE ?= node
MODDIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

.PHONY: all build test vet fmt fmt-check jstest

all: build

build:
	cd $(MODDIR) && $(GO) build -o bin/mhvtld ./cmd/mhvtld

# jstest runs the framework-free dashboard helper tests with Node's built-in
# test runner. The jstest/ dir lives outside assets/ so it is never embedded.
jstest:
	cd $(MODDIR) && $(NODE) --test internal/webui/jstest

test: jstest
	cd $(MODDIR) && $(GO) test ./...

vet:
	cd $(MODDIR) && $(GO) vet ./...

fmt:
	cd $(MODDIR) && $(GO) fmt ./...

fmt-check:
	cd $(MODDIR) && gofmt -l .
