GO ?= go
GOLANGCI_LINT ?= golangci-lint
NPM ?= npm

GUI_DIR := cmd/agenthub-gui
GUI_FRONTEND := $(GUI_DIR)/frontend
GUI_BIN ?= bin/agenthub-gui
BIN ?= bin/agenthub
# Distinct path, not a flag on the same one: a dev and a release build differ in
# data directory AND in which command groups --help shows, so "which one is
# bin/agenthub right now?" is a question with real consequences. Two paths make
# it unaskable — and let both be kept around and compared side by side.
RELEASE_BIN ?= bin/agenthub-release

# Version is x.y.z-<hash>: the release number a human chose, plus the commit
# that number was actually built from. Both halves are load-bearing. The
# number alone cannot answer "which build is this" — two builds of 0.1.0 from
# different commits are indistinguishable, and that ambiguity is exactly what
# bites when a fix is committed but the shipped binary predates it. The hash
# alone cannot answer "how does this compare to what I have installed".
#
# RELEASE_VERSION is the single source of the x.y.z half: the VERSION file at
# the repo root, read by the Makefile, the Taskfile and the release workflow
# alike, so the binary, the .app bundle and the Release title cannot disagree.
#
# -dirty is deliberate: a binary built from uncommitted work must say so.
RELEASE_VERSION ?= $(shell cat VERSION 2>/dev/null || echo 0.0.0)
# --tags is deliberately absent: once a tag exists, `git describe` prefers it
# and yields "v0.1.0-6-gd4ef95f", which pasted after RELEASE_VERSION reads
# "0.1.0-v0.1.0-6-gd4ef95f". The hash half must be a hash, nothing else.
GIT_HASH ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo unknown)$(shell git diff --quiet 2>/dev/null || echo -dirty)
VERSION ?= $(RELEASE_VERSION)-$(GIT_HASH)
LDFLAGS := -X main.version=$(VERSION)

# CHANNEL decides which data directory a build resolves: "release" uses the
# installed location, anything else uses the development sibling. The default
# is dev and the release value must be asked for, because the failure
# directions are not symmetric — see the comment on `channel` in
# cmd/agenthub/main.go.
#
# `make bin` therefore produces a DEV binary. `make bin-release` and the
# packaging targets produce release binaries; the GitHub release workflow
# passes CHANNEL=release explicitly.
CHANNEL ?= dev
LDFLAGS += -X main.channel=$(CHANNEL)

.PHONY: all build bin bin-release test lint fmt tidy ci generate gui gui-frontend gui-frontend-ci gui-go gui-clean release-macos release-clean

# Every installable artifact, for a developer who wants the working tree's code
# in runnable form. NOT a CI target and deliberately not the first rule of this
# file: `make ci` must stay the pure check, and `make gui` needs GTK/WebKit that
# a Linux runner does not have (see the GUI section below).
#
# Stale binaries are the reason this exists. bin/agenthub is what client MCP
# configurations execute, so a fix that is committed but not rebuilt is a fix
# that is not running — and the symptom shows up as a downstream failure, which
# points anywhere but at the build. One command that rebuilds everything is
# cheaper than diagnosing that again.
all: bin gui

# Compile check only — it produces no artifacts, because `go build` with
# several packages discards the executables. `make ci` wants exactly that.
build:
	$(GO) build ./...

# The installable CLI. Kept separate from `build` so `make ci` stays a pure
# check and never writes into the working tree.
#
# This is a DEVELOPMENT build: it resolves ~/Library/Application Support/
# AgentHubDev, so running it cannot disturb an installed release. Use
# `make bin-release` for a binary that behaves like a shipped one.
bin:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/agenthub

# The same binary a release would ship: real data directory, no "(dev)" in
# --version, and no governance command groups in --help. Useful for reproducing
# something that only happens against real data — and for exactly that reason it
# is not the default.
#
# Writes $(RELEASE_BIN), never $(BIN): the two flavours must be able to sit next
# to each other, or "test them side by side" means rebuilding between every
# comparison and trusting that you remember which one is on disk.
bin-release:
	$(MAKE) bin CHANNEL=release BIN=$(RELEASE_BIN)

test:
	$(GO) test ./...

lint:
	$(GOLANGCI_LINT) run

fmt:
	$(GOLANGCI_LINT) fmt

tidy:
	$(GO) mod tidy

generate:
	$(GO) generate ./...

ci: build test lint

# What the `ci` WORKFLOW runs, which is strictly more than `make ci`.
#
# The gap is easy to fall into and costs a red build after a green local run:
#
#   1. The depguard proof (internal/depguardtest) SKIPS itself when
#      golangci-lint is absent, and `make test` reports that skip as success.
#      CI greps the verbose output for "--- SKIP" and fails, because a skipped
#      proof is no proof (canonical.md §6). Reproduced below.
#   2. The whole `gui` job. `make ci` never touches it on purpose — "the GUI is
#      optional" is a compile-time property and must not become a prerequisite
#      of the default build — so it is opt-in here rather than folded into ci.
#   3. `make gui` is NOT the same check: it runs `npm install`, which happily
#      repairs a package-lock.json that disagrees with package.json. CI runs
#      `npm ci`, which refuses. Only gui-frontend-ci reproduces that.
#
# Not wired into `make ci` itself: that target must stay the pure check that a
# Linux runner without GTK/WebKit can complete.
.PHONY: ci-full ci-depguard-proof
ci-full: ci ci-depguard-proof gui-frontend-ci gui-go
	$(GO) vet -tags wails ./cmd/agenthub-gui/...

# The proof must RUN, not skip. Mirrors the workflow step exactly.
ci-depguard-proof:
	@set -o pipefail; \
	$(GO) test ./internal/depguardtest/ -run TestDepguardRulesActuallyFire -count=1 -v \
	  | tee /tmp/agenthub-depguard-proof.log; \
	if grep -q -- "--- SKIP" /tmp/agenthub-depguard-proof.log; then \
	  echo "depguard proof was SKIPPED: install golangci-lint v2.12.2, a skipped proof is no proof" >&2; \
	  exit 1; \
	fi

# --- optional GUI -----------------------------------------------------------
# The GUI is deliberately NOT part of `make build`: linking a webview needs
# GTK/WebKit development packages that a Linux CI runner does not have. Its Go
# code sits behind the `wails` build tag, so `go build ./...` and
# `golangci-lint run` never reach it (docs/canonical.md §7 item 3).
#
# These targets are what the separate `gui` CI job runs (.github/workflows/
# ci.yml). They stay out of `make ci` so the GUI can never become a
# prerequisite of the default build.

gui: gui-frontend gui-go

# Builds the Vite bundle that gets embedded into the binary.
gui-frontend:
	cd $(GUI_FRONTEND) && $(NPM) install && $(NPM) run build

# Same bundle, but installed from the lockfile EXACTLY: `npm ci` fails when
# package-lock.json and package.json disagree, which is the check a CI run
# wants and a developer's working copy does not. `npm run build` is
# `tsc --noEmit && vite build`, so a type error in the frontend fails here.
gui-frontend-ci:
	cd $(GUI_FRONTEND) && $(NPM) ci && $(NPM) run build

# Builds the tagged binary. gui-frontend must have run at least once: the
# embed of frontend/dist fails loudly otherwise.
gui-go:
	$(GO) build -tags wails -o $(GUI_BIN) ./$(GUI_DIR)

gui-clean:
	rm -rf $(GUI_FRONTEND)/dist $(GUI_FRONTEND)/node_modules $(GUI_BIN)

# --- release ----------------------------------------------------------------
# Packaging lives in Taskfile.yml (see its header for why it is not here).
# These targets are the discoverable entry points: someone reading the Makefile
# should not have to already know that a Taskfile exists.

# The distributable macOS DMG, universal, with the CLI inside the bundle.
release-macos:
	wails3 task darwin:package

release-clean:
	rm -rf dist

# --- development -------------------------------------------------------------
# Separation is a property of the BINARY, not of how it is invoked: a build
# without CHANNEL=release resolves the development data directory on its own,
# so there is no environment to remember and no way to forget it. These
# targets are conveniences on top of that, not the mechanism.

.PHONY: dev dev-gui dev-where release-run

# Build, then run one command: `make dev ARGS="status"`.
dev: bin
	$(BIN) $(ARGS)

dev-gui: gui
	$(GUI_BIN)

# Which directory is this build actually using? The question worth asking
# before blaming a config that turns out to live somewhere else.
dev-where: bin
	@$(BIN) doctor 2>/dev/null | grep -E "data-dir|run-dir" || true

# The release flavour of `make dev`. One target rather than a `release-where`
# sibling: the release build raises two questions, not one — which directory,
# and which commands are even visible — and `release-run ARGS="--help"` answers
# the second as readily as `ARGS="doctor"` answers the first.
release-run: bin-release
	$(RELEASE_BIN) $(ARGS)
