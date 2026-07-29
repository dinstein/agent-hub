# AgentHub — the CLI, the checks that gate a push, and the conveniences around
# them. `make help` lists every target with one line each.
#
# WHAT IS NOT HERE. GUI packaging (.app, universal merge, DMG) lives in
# Taskfile.yml and the release artifacts in scripts/; both are reachable from
# the release section below, and nothing in this file depends on either
# existing. `make bin` must keep working if Taskfile.yml is deleted — "the GUI
# is optional" is a compile-time constraint (AGENTS.md), not a preference.

# Recipes run under bash, not /bin/sh. `set -o pipefail` is load-bearing in
# every target that decides pass/fail from text it piped through tee — the
# depguard proof and the landing check — and dash, which is /bin/sh on Linux
# runners and on most Linux developer machines, does not have it. Without this
# those targets do not fail on Linux, they fail to RUN, and that is the worse
# outcome: a developer who cannot execute the check reads the error as "the
# Makefile is broken here" and moves on without it. Declared once, so a recipe
# added later cannot quietly get the weaker shell.
SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

# Bare `make` prints the target list. It deliberately does NOT build: the
# obvious default (`all`) runs npm install and links a webview, which is a
# surprising amount of machinery to trigger by typing four letters.
.DEFAULT_GOAL := help

GO ?= go
GOLANGCI_LINT ?= golangci-lint
NPM ?= npm

# One lint cache per checkout, not one per user.
#
# golangci-lint's default cache is shared across everything the user builds and
# is keyed by module path — which is the same string in every worktree of this
# repository. Lint run here then gets served results computed in a sibling
# worktree, against files this checkout never had; when that sibling has been
# removed, the issues arrive carrying its absolute paths and `make lint` fails
# on code that does not exist. The rule is fine, the tree is fine, the cache is
# lying. Keeping it inside the checkout makes "per worktree" structural rather
# than something to remember.
#
# internal/depguardtest sets the same variable for the golangci-lint it spawns
# itself, for the same reason — it does not inherit this one.
export GOLANGCI_LINT_CACHE ?= $(CURDIR)/.lintcache

# Scratch for the targets that read their own output back. Inside the checkout
# for the same reason as the lint cache: several worktrees are normally in
# flight at once, and a fixed /tmp path means two concurrent runs grade each
# other's log. Gitignored.
LOGDIR := $(CURDIR)/.make

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

# The variable CI's Linux runner sets and a macOS runner does not. Setting it
# must NOT move the run directory — AGENTHUB_DATA_DIR owns that decision on
# every platform — and `make e2e-ci` is the regression test for exactly that
# rule (AGENTS.md). The class of "only happens on CI" failure that once took
# four rounds to diagnose was rooted here, so the reproduction gets a target
# instead of a line to retype correctly.
E2E_XDG ?= /tmp/fake-xdg-e2e

# Fuzzing is opt-in and stays out of CI; `make test` already runs these targets'
# seed corpora on every run, which is the fast half. The list is the inventory
# of paths by which bytes we did not write arrive, and each entry carries its
# package because they do not all live in internal/mcp:
#
#   FuzzParseMessage      downstream JSON-RPC frames
#   FuzzSSEScanner        remote SSE streams — a hand-written line scanner,
#                         the least trustworthy of the five
#   FuzzScanAuthParam     remote WWW-Authenticate, hand-written index scanning
#   FuzzEncodeJSON        downstream tool results, on the response path
#   FuzzScanTOMLServers   another application's config file, hand-written
#
# Touching one of those parsers means running its target — `make fuzz
# FUZZ=FuzzSSEScanner` — not necessarily the whole set.
FUZZTIME ?= 60s
FUZZ_TARGETS := \
	./internal/mcp:FuzzParseMessage \
	./internal/mcp/transport:FuzzSSEScanner \
	./internal/oauthflow:FuzzScanAuthParam \
	./internal/shaping/toonenc:FuzzEncodeJSON \
	./internal/clients:FuzzScanTOMLServers \
	./internal/clients:FuzzBlankJSONC \
	./internal/clients:FuzzSpliceEntryKeepsEverythingElse
FUZZ ?=

##@ Meta

.PHONY: help
help: ## List every target
	@awk 'BEGIN {FS = ":.*## "} \
		/^##@/ { printf "\n%s\n", substr($$0, 5); next } \
		/^[a-z][a-z0-9_-]*:.*## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@printf '\nVariables: CHANNEL=%s FUZZTIME=%s BIN=%s, plus ARGS for dev/release-run\n' \
		'$(CHANNEL)' '$(FUZZTIME)' '$(BIN)'

$(LOGDIR):
	@mkdir -p $@

##@ Checks

.PHONY: build test lint fmt tidy generate
# Compile check only — it produces no artifacts, because `go build` with
# several packages discards the executables. `make ci` wants exactly that.
build: ## Compile every package, writing nothing
	$(GO) build ./...

test: ## go test ./... — includes test/e2e and every fuzz seed corpus
	$(GO) test ./...

lint: ## golangci-lint run
	$(GOLANGCI_LINT) run

fmt: ## Format with golangci-lint's formatters
	$(GOLANGCI_LINT) fmt

tidy: ## go mod tidy
	$(GO) mod tidy

# The one //go:generate directive lives in cmd/agenthub-gui/main.go, the
# UNTAGGED placeholder, so a default `go generate ./...` reaches it: it
# regenerates the frontend's health.ts out of the api package. The golden test
# in internal/healthgen fails while the checked-in file is stale, so forgetting
# this surfaces in `make test` rather than in the GUI.
generate: ## Regenerate the api → TypeScript health constants
	$(GO) generate ./...

.PHONY: e2e e2e-ci
e2e: ## The end-to-end suite alone, in this machine's environment
	$(GO) test ./test/e2e/ -count=1

# A second target rather than an environment folded into `test`: the suite must
# pass both with and without XDG_RUNTIME_DIR, and running only the CI shape
# would leave the macOS-runner shape untested exactly as much as the reverse.
e2e-ci: ## The same suite with CI's Linux environment simulated
	@mkdir -p $(E2E_XDG)
	XDG_RUNTIME_DIR=$(E2E_XDG) $(GO) test ./test/e2e/ -count=1

.PHONY: ci ci-full ci-landing ci-depguard-proof fuzz
ci: build test lint ## Build + test + lint: the pure check, no GUI, no artifacts

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
# The last three are the workflow's gui job, same targets in the same order, so
# the correspondence stays checkable by eye.
ci-full: ci ci-depguard-proof gui-frontend-ci gui-go gui-vet ## Everything the CI workflow runs; use before pushing

# Landing a branch, as one command. `make ci-full` is necessary and not
# sufficient, for two reasons AGENTS.md spells out and that are easy to perform
# incorrectly by hand:
#
#   The run belongs AFTER the rebase, on code the branch has never been tested
#   against. That is the entire cost of the rebase rule, and skipping it is how
#   main goes red.
#
#   The test cache defeats the rule silently. test/e2e builds the binary under
#   test inside TestMain, so a change to cmd/agenthub on the new base is not
#   part of the key Go caches the result under: the suite reports "ok (cached)"
#   for a tree it never ran against. So the cache is dropped first and the log
#   is then checked for the "(cached)" that must no longer be there — the
#   assertion is the point, because a human scrolling a long log does not
#   notice a word that is absent.
ci-landing: | $(LOGDIR) ## ci-full with the cache dropped and CI's env; run it after the rebase
	$(GO) clean -testcache
	@mkdir -p $(E2E_XDG)
	@XDG_RUNTIME_DIR=$(E2E_XDG) $(MAKE) --no-print-directory ci-full 2>&1 | tee $(LOGDIR)/ci.log
	@cached="$$(grep -c '(cached)' $(LOGDIR)/ci.log || true)"; \
	if [ "$$cached" != 0 ]; then \
		echo "landing check: $$cached cached result(s) in $(LOGDIR)/ci.log — the tree about to land was not actually tested" >&2; \
		exit 1; \
	fi; \
	echo "landing check: nothing came from cache, every package ran"

# The proof must RUN, not skip. Mirrors the workflow step exactly.
ci-depguard-proof: | $(LOGDIR) ## Prove the depguard rules fire; fail if the proof skipped
	@$(GO) test ./internal/depguardtest/ -run TestDepguardRulesActuallyFire -count=1 -v \
		| tee $(LOGDIR)/depguard-proof.log
	@if grep -q -- "--- SKIP" $(LOGDIR)/depguard-proof.log; then \
		echo "depguard proof was SKIPPED: install golangci-lint v2.12.2, a skipped proof is no proof" >&2; \
		exit 1; \
	fi

fuzz: ## Fuzz the untrusted-input parsers (FUZZ=<name> for one, FUZZTIME=60s)
	@matched=0; \
	for entry in $(FUZZ_TARGETS); do \
		pkg="$${entry%%:*}"; target="$${entry##*:}"; \
		case "$$target" in *"$(FUZZ)"*) ;; *) continue ;; esac; \
		matched=1; \
		echo "==> $$target  $$pkg  ($(FUZZTIME))"; \
		$(GO) test "$$pkg" -run xxx -fuzz "^$$target\$$" -fuzztime $(FUZZTIME); \
	done; \
	if [ "$$matched" = 0 ]; then \
		echo "no fuzz target matches FUZZ=$(FUZZ); known targets:" >&2; \
		for entry in $(FUZZ_TARGETS); do echo "  $${entry##*:}" >&2; done; \
		exit 2; \
	fi

##@ Binaries

.PHONY: all bin bin-release
# Every installable artifact, for a developer who wants the working tree's code
# in runnable form. Not a CI target, and not the default goal.
#
# Stale binaries are the reason this exists. bin/agenthub is what client MCP
# configurations execute, so a fix that is committed but not rebuilt is a fix
# that is not running — and the symptom shows up as a downstream failure, which
# points anywhere but at the build. One command that rebuilds everything is
# cheaper than diagnosing that again.
all: bin gui ## Build everything installable: the CLI and the GUI

# The installable CLI. Kept separate from `build` so `make ci` stays a pure
# check and never writes into the working tree.
#
# This is a DEVELOPMENT build: it resolves ~/Library/Application Support/
# AgentHubDev, so running it cannot disturb an installed release. Use
# `make bin-release` for a binary that behaves like a shipped one.
bin: ## Build the CLI on the dev channel → bin/agenthub
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/agenthub

# The same binary a release would ship: real data directory, no "(dev)" in
# --version, and no governance command groups in --help. Useful for reproducing
# something that only happens against real data — and for exactly that reason it
# is not the default.
#
# Writes $(RELEASE_BIN), never $(BIN): the two flavours must be able to sit next
# to each other, or "test them side by side" means rebuilding between every
# comparison and trusting that you remember which one is on disk.
bin-release: ## Build the CLI as a release ships it → bin/agenthub-release
	$(MAKE) bin CHANNEL=release BIN=$(RELEASE_BIN)

##@ GUI (optional by construction)

# The GUI is deliberately NOT part of `make build`: linking a webview needs
# GTK/WebKit development packages that a Linux CI runner does not have. Its Go
# code sits behind the `wails` build tag, so `go build ./...` and
# `golangci-lint run` never reach it (docs/canonical.md §7 item 3).
#
# gui-frontend-ci, gui-go and gui-vet are what the separate `gui` CI job runs
# (.github/workflows/ci.yml). They stay out of `make ci` so the GUI can never
# become a prerequisite of the default build.

.PHONY: gui gui-frontend gui-frontend-ci gui-go gui-vet gui-clean
gui: gui-frontend gui-go ## Build the GUI: frontend bundle + wails-tagged binary

# Builds the Vite bundle that gets embedded into the binary.
gui-frontend: ## Install deps and build the frontend bundle
	cd $(GUI_FRONTEND) && $(NPM) install && $(NPM) run build

# Same bundle, but installed from the lockfile EXACTLY: `npm ci` fails when
# package-lock.json and package.json disagree, which is the check a CI run
# wants and a developer's working copy does not. `npm run build` is
# `tsc --noEmit && vite build`, so a type error in the frontend fails here.
gui-frontend-ci: ## The frontend as CI builds it: npm ci rejects a stale lockfile
	cd $(GUI_FRONTEND) && $(NPM) ci && $(NPM) run build

# Builds the tagged binary. gui-frontend must have run at least once: the
# embed of frontend/dist fails loudly otherwise.
gui-go: ## Build the wails-tagged binary → bin/agenthub-gui
	$(GO) build -tags wails -o $(GUI_BIN) ./$(GUI_DIR)

# The tagged half is invisible to `make lint`, so vet is the only static check
# it ever gets — and wails v3 is an alpha whose API does move.
gui-vet: ## Vet the wails-tagged code, the only static check it gets
	$(GO) vet -tags wails ./$(GUI_DIR)/...

gui-clean: ## Remove the bundle, node_modules and the GUI binary
	rm -rf $(GUI_FRONTEND)/dist $(GUI_FRONTEND)/node_modules $(GUI_BIN)

##@ Release

# GUI packaging lives in Taskfile.yml and the CLI artifacts in scripts/ (each
# file's header says why). These targets are the discoverable entry points:
# someone reading the Makefile should not have to already know those exist.
#
# Cutting an actual release is `scripts/release-local.sh <vX.Y.Z> [--push]`,
# deliberately NOT wrapped here. It publishes a GitHub Release and commits to
# the Homebrew tap; both are visible to other people the moment they happen and
# neither can be quietly taken back. The dry-run-unless---push shape is the
# safety, and a make target is how a flag like that gets passed by accident.

.PHONY: release-macos release-cli
release-macos: ## Build the macOS DMG: universal, CLI inside the bundle
	wails3 task darwin:package

# The same script the release workflow runs, so the bytes do not depend on who
# built them. Writes dist/; the script refuses a dirty tree.
release-cli: ## Cross-compile the CLI release artifacts into dist/
	scripts/build-release-artifacts.sh $(RELEASE_VERSION)

##@ Development

# Separation is a property of the BINARY, not of how it is invoked: a build
# without CHANNEL=release resolves the development data directory on its own,
# so there is no environment to remember and no way to forget it. These
# targets are conveniences on top of that, not the mechanism.

.PHONY: dev dev-gui dev-where release-run
dev: bin ## Build, then run one command: make dev ARGS="status"
	$(BIN) $(ARGS)

dev-gui: gui ## Build and launch the GUI
	$(GUI_BIN)

# Which directory is this build actually using? The question worth asking
# before blaming a config that turns out to live somewhere else. It fails
# loudly when doctor cannot answer: a silently empty result reads as "there is
# no data directory", which is never what happened.
dev-where: bin ## Which data directory this build actually resolves
	@$(BIN) doctor | grep -E '(data-dir|run-dir):'

# The release flavour of `make dev`. One target rather than a `release-where`
# sibling: the release build raises two questions, not one — which directory,
# and which commands are even visible — and `release-run ARGS="--help"` answers
# the second as readily as `ARGS="doctor"` answers the first.
release-run: bin-release ## The release build's equivalent of make dev
	$(RELEASE_BIN) $(ARGS)

##@ Cleaning

.PHONY: clean clean-cache clean-all
# Named paths rather than `rm -rf bin dist`: BIN and its siblings are
# overridable, and a clean that ignores the override deletes something it was
# never pointed at.
clean: ## Remove this checkout's build output and logs
	rm -rf $(BIN) $(RELEASE_BIN) $(GUI_BIN) dist $(LOGDIR)

# The two caches that can make a stale tree look green — the same pair
# ci-landing has to defeat.
clean-cache: ## Drop the Go test cache and this checkout's lint cache
	$(GO) clean -testcache
	rm -rf $(GOLANGCI_LINT_CACHE)

clean-all: clean clean-cache gui-clean ## Everything above, plus node_modules
