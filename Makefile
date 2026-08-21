GO      ?= go
PREFIX  ?= $(HOME)/.local
BINDIR   = $(PREFIX)/bin

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -ldflags "-X github.com/rpickz/jarvix/internal/build.Version=$(VERSION)"

.PHONY: all build test ci coverage vet gofmt-check lint install install-kokoro install-plugin install-systemd install-hyprland uninstall clean release-snapshot

all: build

build:
	$(GO) build $(LDFLAGS) -o bin/jarvix ./cmd/jarvix
	$(GO) build $(LDFLAGS) -o bin/jarvixd ./cmd/jarvixd

test:
	$(GO) test ./...

# What CI runs (.github/workflows/ci.yml), in the same order: build, vet,
# gofmt, race detector twice, lint. It used to skip the build and the gofmt
# check, so `make ci` could pass on a commit the gate then rejected — keep the
# two in step (raised in review of #15).
ci: vet gofmt-check lint
	$(GO) build ./...
	$(GO) test -race -count=2 ./...

coverage:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vet:
	$(GO) vet ./...

# gofmt reports unformatted files on stdout and still exits 0, so the check
# has to inspect the output rather than the status.
gofmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi

# Availability and outcome are checked separately, and deliberately so:
# `command -v X && X run || echo "not installed"` reports a NONZERO lint exit
# as "not installed" and lets `make ci` pass with real findings. Here the
# `||` fallback is gone, so the linter's exit status is the recipe's exit
# status (raised in review of #15).
lint: vet
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	elif command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "golangci-lint/staticcheck not installed; ran go vet only"; \
	fi

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
	$(GO) test -run='^$$' -fuzz='^FuzzRouterMatch$$' -fuzztime=$(FUZZTIME) ./internal/intent

# Latency/throughput benchmarks over our own pipeline (fakes, not engines).
# BENCHCOUNT=5 gives benchstat-able samples; CI passes it, local runs default
# to one — same command either way.
BENCHCOUNT ?= 1
bench:
	$(GO) test -run='^$$' -bench=. -benchmem -count=$(BENCHCOUNT) ./internal/session ./internal/intent

# Mutation testing over the session engine (the core state machine).
# GOFLAGS=-count=1 keeps gremlins' baseline honest: a cached test run makes
# the derived per-mutant timeout near zero and everything "times out".
# The gremlins version is pinned so the documented mutation score stays
# reproducible — bump it deliberately and re-triage the survivors.
GREMLINS_VERSION ?= v0.6.0
mutate:
	GOFLAGS=-count=1 $(GO) run github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION) unleash --timeout-coefficient 3 ./internal/session
