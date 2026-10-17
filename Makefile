GO ?= go
NODE ?= node
MODDIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

VERSION ?= $(or $(shell cd $(MODDIR) && git describe --tags --always --dirty 2>/dev/null | sed 's/^v//'),dev)
# Static, reproducible: no cgo, no build paths, no VCS stamp (packages build
# from tarballs without .git).
GOBUILDFLAGS ?= -trimpath -buildvcs=false
LDFLAGS ?= -s -w

PREFIX ?= /usr
BINDIR ?= $(PREFIX)/bin
# The helper's units name /usr/libexec/mhvtl-control/mhvtl-control-apply.
LIBEXECDIR ?= $(PREFIX)/libexec
SYSCONFDIR ?= /etc
UNITDIR ?= $(PREFIX)/lib/systemd/system
TMPFILESDIR ?= $(PREFIX)/lib/tmpfiles.d
LOCALSTATEDIR ?= /var
MANDIR ?= $(PREFIX)/share/man

.PHONY: all build test vet fmt fmt-check jstest install

all: build

build:
	cd $(MODDIR) && CGO_ENABLED=0 $(GO) build $(GOBUILDFLAGS) \
		-ldflags '$(LDFLAGS) -X main.version=$(VERSION)' -o bin/mhvtl-control ./cmd/mhvtl-control
	cd $(MODDIR) && CGO_ENABLED=0 $(GO) build $(GOBUILDFLAGS) \
		-ldflags '$(LDFLAGS) -X main.version=$(VERSION)' -o bin/mhvtl-control-apply ./cmd/mhvtl-control-apply

# install stages the binary, the unit, the man page and the default options
# file. The options file may hold a token, so it is installed mode 0600 in a
# 0750 directory, and an existing one is left alone so that reinstalling keeps
# the configuration. The unit masks the two helper directories with
# InaccessiblePaths=, which fails the start if they are missing, so both are
# created here: /run's through tmpfiles.d, /var/lib's directly.
#
# The config-apply helper is installed with its socket, its per-connection
# service, its boot-recovery oneshot and the drop-ins that order mhVTL's
# instances after that oneshot. Nothing is enabled: enabling
# mhvtl-control-apply.socket (and the oneshot with it) is what turns
# configuration writes on.
install:
	install -D -m 0755 $(MODDIR)bin/mhvtl-control $(DESTDIR)$(BINDIR)/mhvtl-control
	install -D -m 0755 $(MODDIR)bin/mhvtl-control-apply $(DESTDIR)$(LIBEXECDIR)/mhvtl-control/mhvtl-control-apply
	install -D -m 0644 $(MODDIR)dist/mhvtl-control.service $(DESTDIR)$(UNITDIR)/mhvtl-control.service
	install -D -m 0644 $(MODDIR)dist/mhvtl-control-apply.socket $(DESTDIR)$(UNITDIR)/mhvtl-control-apply.socket
	install -D -m 0644 $(MODDIR)dist/mhvtl-control-apply@.service $(DESTDIR)$(UNITDIR)/mhvtl-control-apply@.service
	install -D -m 0644 $(MODDIR)dist/mhvtl-control-apply-recover.service $(DESTDIR)$(UNITDIR)/mhvtl-control-apply-recover.service
	install -D -m 0644 $(MODDIR)dist/vtllibrary@.service.d/50-mhvtl-control-apply-recover.conf \
		$(DESTDIR)$(UNITDIR)/vtllibrary@.service.d/50-mhvtl-control-apply-recover.conf
	install -D -m 0644 $(MODDIR)dist/vtltape@.service.d/50-mhvtl-control-apply-recover.conf \
		$(DESTDIR)$(UNITDIR)/vtltape@.service.d/50-mhvtl-control-apply-recover.conf
	install -D -m 0644 $(MODDIR)dist/mhvtl-control.tmpfiles $(DESTDIR)$(TMPFILESDIR)/mhvtl-control.conf
	install -d -m 0700 $(DESTDIR)$(LOCALSTATEDIR)/lib/mhvtl-control-apply
	install -d -m 0750 $(DESTDIR)$(SYSCONFDIR)/mhvtl-control
	test -e $(DESTDIR)$(SYSCONFDIR)/mhvtl-control/mhvtl-control.env || \
		install -m 0600 $(MODDIR)dist/mhvtl-control.env $(DESTDIR)$(SYSCONFDIR)/mhvtl-control/mhvtl-control.env
	install -D -m 0644 $(MODDIR)dist/mhvtl-control.8 $(DESTDIR)$(MANDIR)/man8/mhvtl-control.8

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
