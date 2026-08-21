GO      ?= go
PREFIX  ?= $(HOME)/.local
BINDIR   = $(PREFIX)/bin

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -ldflags "-X github.com/rpickz/jarvix/internal/build.Version=$(VERSION)"

.PHONY: all build test ci coverage vet lint install install-kokoro install-plugin install-systemd install-hyprland uninstall clean release-snapshot

all: build

build:
	$(GO) build $(LDFLAGS) -o bin/jarvix ./cmd/jarvix
	$(GO) build $(LDFLAGS) -o bin/jarvixd ./cmd/jarvixd

test:
	$(GO) test ./...

# What CI runs (.github/workflows/ci.yml) — race detector, twice, plus lint.
ci: vet
	$(GO) test -race -count=2 ./...
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed; skipped"

coverage:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vet:
	$(GO) vet ./...

lint: vet
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || \
		{ command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "golangci-lint/staticcheck not installed; ran go vet only"; }

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

# Local dry run of the release packaging (.github/workflows/release.yml runs
# the same script): tarballs + SHA256SUMS into ./dist, versioned from git.
release-snapshot:
	scripts/package-release.sh

uninstall:
	rm -f $(BINDIR)/jarvix $(BINDIR)/jarvixd
	rm -f $(HOME)/.config/systemd/user/jarvixd.service

clean:
	rm -rf bin dist

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
# BENCHCOUNT=5 gives benchstat-able samples; CI passes it, local runs default
# to one — same command either way.
BENCHCOUNT ?= 1
bench:
	$(GO) test -run='^$$' -bench=. -benchmem -count=$(BENCHCOUNT) ./internal/session

# Mutation testing over the session engine (the core state machine).
# GOFLAGS=-count=1 keeps gremlins' baseline honest: a cached test run makes
# the derived per-mutant timeout near zero and everything "times out".
# The gremlins version is pinned so the documented mutation score stays
# reproducible — bump it deliberately and re-triage the survivors.
GREMLINS_VERSION ?= v0.6.0
mutate:
	GOFLAGS=-count=1 $(GO) run github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION) unleash --timeout-coefficient 3 ./internal/session
