# Every check this repository runs has its entry point here. CI installs the
# toolchain and calls `make check`; no other file repeats these commands.
#
# The module set is derived from go.work, the repository's only machine-readable
# module roster, so adding a module to the workspace registers it everywhere.

SHELL := /bin/bash
.DEFAULT_GOAL := help

# A use directive has two legal spellings -- a parenthesised block and a
# bare `use ./dir` line -- so the roster is parsed by the go toolchain
# itself rather than by a matcher here that would know only one of them
# and silently come up empty on the other.
MODULES := $(shell go work edit -json go.work | sed -n 's/^[[:space:]]*"DiskPath": "\(.*\)".*/\1/p')

# Every tools/check_*.py is a gate over this tree: it takes no arguments, it
# reports against the current directory, and it is run by `make repo-check`.
# A script under tools/ that is not a gate carries another prefix.
GATES := $(wildcard tools/check_*.py)

PYTHON ?= python3

.PHONY: help modules build test test-race fmt lint tidy tidy-check repo-check tools-test check python-version modules-present

help: ## List the entry points
	@awk 'BEGIN{FS=":.*## "} /^[a-z][a-z-]*:.*## /{printf "  %-11s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

modules: modules-present ## Print the module directories derived from go.work
	@for m in $(MODULES); do echo $$m; done

build: modules-present ## Build every module, both in the workspace and standalone
	@for m in $(MODULES); do echo "==> build $$m"; go -C $$m build ./... || exit 1; done
	@for m in $(MODULES); do echo "==> build standalone $$m"; GOWORK=off go -C $$m build ./... || exit 1; done

test: modules-present ## Test every module
	@for m in $(MODULES); do echo "==> test $$m"; go -C $$m test ./... || exit 1; done

test-race: modules-present ## Test every module under the race detector
	@for m in $(MODULES); do echo "==> test -race $$m"; go -C $$m test -race ./... || exit 1; done

fmt: modules-present ## Format every module in place
	@for m in $(MODULES); do echo "==> fmt $$m"; env -C $$m golangci-lint fmt ./... || exit 1; done

lint: modules-present ## Report formatting drift and static-analysis findings
	@for m in $(MODULES); do echo "==> lint $$m"; env -C $$m golangci-lint fmt --diff ./... || exit 1; env -C $$m golangci-lint run ./... || exit 1; done

tidy: modules-present ## Tidy every module's go.mod and go.sum in place
	@for m in $(MODULES); do echo "==> tidy $$m"; GOWORK=off go -C $$m mod tidy || exit 1; done

# `go mod tidy -diff` reports what tidying would change without writing
# anything, so the judgement is the dependency files themselves. Judging
# them by the working tree's git status instead both misses an uncommitted
# untidy file (tidying restores the committed bytes, leaving the tree
# clean) and fails on any unrelated edit to a go.mod.
tidy-check: modules-present ## Verify the dependency files are tidy, leaving the tree unchanged
	@for m in $(MODULES); do echo "==> tidy-check $$m"; GOWORK=off go -C $$m mod tidy -diff || exit 1; done

repo-check: python-version ## Run the gates in tools/ over this tree
	@for g in $(GATES); do echo "==> $$g"; $(PYTHON) $$g || exit 1; done

tools-test: python-version ## Run the test suites of the scripts in tools/
	@$(PYTHON) -m unittest discover -s tools -t tools -p 'test_*.py'

# Every leg above loops over $(MODULES), and a loop over an empty list is
# a no-op that exits 0 -- an empty roster would take the whole of `make
# check` through to success without running a single check. No leg runs
# before this says the roster is non-empty.
modules-present:
	@if [ -z "$(strip $(MODULES))" ]; then \
	  echo "no modules: go.work lists nothing this build can read." >&2; \
	  echo "Every check loops over that roster, so an empty one would pass without checking anything." >&2; \
	  exit 1; \
	fi

# The scripts parse TOML with tomllib, which arrived in Python 3.11.
python-version:
	@$(PYTHON) -c 'import sys; sys.exit(0 if sys.version_info >= (3, 11) else 1)' || { \
	  echo "tools/ needs Python 3.11 or newer (tomllib); $(PYTHON) is $$($(PYTHON) -V 2>&1)." >&2; \
	  echo "Install the version .mise.toml pins, or pass PYTHON=<interpreter>." >&2; \
	  exit 1; \
	}

check: ## Run everything CI runs
	@$(MAKE) build
	@$(MAKE) test
	@$(MAKE) test-race
	@$(MAKE) lint
	@$(MAKE) tidy-check
	@$(MAKE) repo-check
	@$(MAKE) tools-test
