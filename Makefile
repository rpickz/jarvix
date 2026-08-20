GO      ?= go
PREFIX  ?= $(HOME)/.local
BINDIR   = $(PREFIX)/bin

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -ldflags "-X github.com/rpickz/jarvix/internal/build.Version=$(VERSION)"

.PHONY: all build test vet lint install install-plugin install-systemd install-hyprland uninstall clean

all: build

build:
	$(GO) build $(LDFLAGS) -o bin/jarvix ./cmd/jarvix
	$(GO) build $(LDFLAGS) -o bin/jarvixd ./cmd/jarvixd

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint: vet
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed; ran go vet only"

install: build
	install -Dm755 bin/jarvix  $(BINDIR)/jarvix
	install -Dm755 bin/jarvixd $(BINDIR)/jarvixd

install-plugin:
	scripts/install-plugin.sh

install-systemd:
	install -Dm644 systemd/jarvixd.service $(HOME)/.config/systemd/user/jarvixd.service
	systemctl --user daemon-reload
	@echo "Enable with: systemctl --user enable --now jarvixd"

install-hyprland:
	scripts/install-hyprland-bindings.sh

uninstall:
	rm -f $(BINDIR)/jarvix $(BINDIR)/jarvixd
	rm -f $(HOME)/.config/systemd/user/jarvixd.service

clean:
	rm -rf bin
