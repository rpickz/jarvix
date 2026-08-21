GO      ?= go
PREFIX  ?= $(HOME)/.local
BINDIR   = $(PREFIX)/bin

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -ldflags "-X github.com/rpickz/jarvix/internal/build.Version=$(VERSION)"

.PHONY: all build test vet lint install install-kokoro install-plugin install-systemd install-hyprland uninstall clean

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

install-kokoro:
	scripts/setup-kokoro.sh

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

# --- Test-depth targets (issue #8) -----------------------------------------
# Local and CI invocations are identical: CI calls these same targets.

.PHONY: fuzz bench mutate

# Fuzz every parser that eats external input. Each target runs briefly; the
# committed seed corpora under testdata/fuzz regression-check known inputs on
# every plain `go test` run as well.
FUZZTIME ?= 30s
fuzz:
	$(GO) test -run='^$$' -fuzz='^FuzzSentencer$$' -fuzztime=$(FUZZTIME) ./internal/session
	$(GO) test -run='^$$' -fuzz='^FuzzSpeechText$$' -fuzztime=$(FUZZTIME) ./internal/session
	$(GO) test -run='^$$' -fuzz='^FuzzConfigParse$$' -fuzztime=$(FUZZTIME) ./internal/config
	$(GO) test -run='^$$' -fuzz='^FuzzWireDecode$$' -fuzztime=$(FUZZTIME) ./internal/ipc
	$(GO) test -run='^$$' -fuzz='^FuzzReadStream$$' -fuzztime=$(FUZZTIME) ./internal/ai/openaicompat

# Latency/throughput benchmarks over our own pipeline (fakes, not engines).
bench:
	$(GO) test -run='^$$' -bench=. -benchmem ./internal/session

# Mutation testing over the session engine (the core state machine).
mutate:
	$(GO) run github.com/go-gremlins/gremlins/cmd/gremlins@latest unleash ./internal/session
