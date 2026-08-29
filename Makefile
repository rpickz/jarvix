GO      ?= go
PREFIX  ?= $(HOME)/.local
BINDIR   = $(PREFIX)/bin

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -ldflags "-X github.com/rpickz/jarvix/internal/build.Version=$(VERSION)"

.PHONY: all build test ci coverage coverage-ratchet coverage-ratchet-raise qml-test vet gofmt-check lint generate install install-kokoro install-wake install-plugin install-systemd install-hyprland uninstall clean release-snapshot

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

# The coverage ratchet (issue #171). Same script CI runs, so the local answer
# is the gate's answer. `-raise` prints the line to paste into coverage.floor;
# nothing ever writes that file for you, which is the whole point of it.
coverage-ratchet:
	scripts/coverage-ratchet.sh

coverage-ratchet-raise:
	scripts/coverage-ratchet.sh --raise

# The QML behaviour suite (issue #174). Same script CI runs, so the local
# answer is the gate's answer. It is not part of `ci` above for the same reason
# `coverage-ratchet` is not: both are their own CI job, running beside the Go
# gate rather than lengthening it.
#
# It needs a Qt 6 qmltestrunner and the QtTest QML module, and it fails rather
# than skips when they are missing — the script says how to install them.
# Narrow a run while you chase something:
#
#   make qml-test QMLTEST=tst_pendingturn.qml
#
QMLTEST ?=
qml-test:
	scripts/qml-test.sh $(QMLTEST)

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

# Rebuild the checked-in artifacts generated from Go — currently the bar
# widget's state vocabulary (plugin/omarchy/BarState.js). `go test` fails if
# they have drifted, so this is the fix for that failure, not an extra step.
generate:
	$(GO) generate ./...

install: build
	install -Dm755 bin/jarvix  $(BINDIR)/jarvix
	install -Dm755 bin/jarvixd $(BINDIR)/jarvixd

install-kokoro:
	scripts/setup-kokoro.sh

install-wake:
	scripts/setup-wake.sh

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

.PHONY: fuzz fuzz-properties bench bench-engines mutate soak soak-repeat soak-constrained soak-unraced voice-corpus voice-corpus-baseline

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
	$(GO) test -run='^$$' -fuzz='^FuzzRedact$$' -fuzztime=$(FUZZTIME) ./internal/desktop

# The property targets of issue #172: the shell classifier, the
# remembered-approval matrix and the spoken-time parser, each attacked with the
# invariants that make them security controls rather than heuristics, stated as
# laws rather than as a table of examples.
#
# Deliberately NOT part of `fuzz` above, which .github/workflows/test-depth.yml
# runs for thirty seconds a target on every pull request. These are searches
# rather than regression checks and they belong on the weekly clock
# (.github/workflows/mutation.yml, five minutes a target), so the PR gate stays
# where it is. What the PR gate does keep is the committed corpora: every one
# of these targets replays its testdata on a plain `go test`, in milliseconds,
# so a case the fuzzer has already found can never come back unnoticed.
fuzz-properties:
	$(GO) test -run='^$$' -fuzz='^FuzzShellClassifier$$' -fuzztime=$(FUZZTIME) ./internal/tools
	$(GO) test -run='^$$' -fuzz='^FuzzRememberOffer$$' -fuzztime=$(FUZZTIME) ./internal/tools
	$(GO) test -run='^$$' -fuzz='^FuzzParseWhen$$' -fuzztime=$(FUZZTIME) ./internal/intent

# Latency/throughput benchmarks over our own pipeline (fakes, not engines).
# BENCHCOUNT=5 gives benchstat-able samples; CI passes it, local runs default
# to one — same command either way.
BENCHCOUNT ?= 1
bench:
	$(GO) test -run='^$$' -bench=. -benchmem -count=$(BENCHCOUNT) ./internal/session ./internal/intent ./internal/desktop

# The same latency question against the REAL local engines (ADR 0018). Not run
# by CI: it measures the machine, not the code, so it lives behind a build tag
# and is the tool for answering "is warm mode still worth it on this box?".
# Needs a 16 kHz mono WAV of a short question:
#   JARVIX_BENCH_WAV=/tmp/question.wav make bench-engines
bench-engines:
	$(GO) test -tags=engines -run='^$$' -bench='Engines' -benchtime=5x \
		-count=$(BENCHCOUNT) -v ./internal/session

# The real-voice corpus (issue #143): the user's own recordings through the
# real whisper, asserted on the intent that matched and the word that survived.
# Behind a build tag, so it is out of CI by construction — the recordings are
# personal audio and whisper is heavy. -v because an empty corpus SKIPS, and a
# skip nobody sees is indistinguishable from a pass.
#
# Recordings live in testdata/voicecorpus; JARVIX_VOICE_CORPUS points elsewhere.
# docs/voice-corpus.md explains recording, adding phrases, and the baseline.
voice-corpus:
	$(GO) test -tags voicecorpus -count=1 -v ./internal/voicecorpus

# Rewrite the committed baseline from a run. Deliberately a separate target
# with the flag spelled out: the baseline's only value is that a person agreed
# to it, so read the diff before committing it.
voice-corpus-baseline:
	$(GO) test -tags voicecorpus -count=1 -v ./internal/voicecorpus -voicecorpus.update-baseline

# Mutation testing over the security-critical packages, with a report of the
# mutants that survived (issue #172). The package set, the flags and the report
# all live in the script, so .github/workflows/mutation.yml and a local run are
# the same run — and the report is a file rather than a wall of scrolling
# output, because the previous target's output was read by nobody.
#
# The gremlins version is pinned in the script so the documented mutation score
# stays reproducible — bump it deliberately and re-triage the survivors.
# docs/mutation.md holds the current triage: every survivor either killed or
# recorded as accepted, with its reason.
#
#   make mutate                          # the defined package set
#   make mutate MUTATE_PKGS=./internal/tools
MUTATE_PKGS ?=
mutate:
	scripts/mutation-report.sh $(MUTATE_PKGS)

# --- Soak (issue #171) ------------------------------------------------------
# The commands that catch this repo's ordering failures, which the PR gate's
# `-race -count=2` does not. .github/workflows/soak.yml runs these same
# targets nightly; docs/soak.md explains what each mode is for and how to read
# a failure. SOAKPKG narrows a run to one package while you chase something:
#
#   make soak-repeat SOAKPKG=./internal/session
#
SOAKPKG ?=
soak:
	scripts/soak.sh all $(SOAKPKG)

soak-repeat:
	scripts/soak.sh repeat $(SOAKPKG)

soak-constrained:
	scripts/soak.sh constrained $(SOAKPKG)

soak-unraced:
	scripts/soak.sh unraced $(SOAKPKG)
