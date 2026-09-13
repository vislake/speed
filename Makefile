# Every check this repository runs has its entry point here. CI installs the
# toolchain and calls `make check`; no other file repeats these commands.
#
# The module set is derived from go.work, the repository's only machine-readable
# module roster, so adding a module to the workspace registers it everywhere.

SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULES := $(shell awk '$$1 ~ /^\.\//{print $$1}' go.work)

# The toolchain gate parses TOML with tomllib, which arrived in Python 3.11.
PYTHON ?= python3

.PHONY: help modules build test test-race fmt lint tidy tidy-check tools-test check

help: ## List the entry points
	@awk 'BEGIN{FS=":.*## "} /^[a-z][a-z-]*:.*## /{printf "  %-11s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

modules: ## Print the module directories derived from go.work
	@for m in $(MODULES); do echo $$m; done

build: ## Build every module, both in the workspace and standalone
	@for m in $(MODULES); do echo "==> build $$m"; go -C $$m build ./... || exit 1; done
	@for m in $(MODULES); do echo "==> build standalone $$m"; GOWORK=off go -C $$m build ./... || exit 1; done

test: ## Test every module
	@for m in $(MODULES); do echo "==> test $$m"; go -C $$m test ./... || exit 1; done

test-race: ## Test every module under the race detector
	@for m in $(MODULES); do echo "==> test -race $$m"; go -C $$m test -race ./... || exit 1; done

fmt: ## Format every module in place
	@for m in $(MODULES); do echo "==> fmt $$m"; env -C $$m golangci-lint fmt ./... || exit 1; done

lint: ## Report formatting drift and static-analysis findings
	@for m in $(MODULES); do echo "==> lint $$m"; env -C $$m golangci-lint fmt --diff ./... || exit 1; env -C $$m golangci-lint run ./... || exit 1; done

tidy: ## Tidy every module's go.mod and go.sum in place
	@for m in $(MODULES); do echo "==> tidy $$m"; GOWORK=off go -C $$m mod tidy || exit 1; done

tidy-check: ## Verify the dependency files are tidy, leaving the tree unchanged
	@backup=$$(mktemp -d); status=0; \
	for m in $(MODULES); do \
	  mkdir -p $$backup/$$m; cp $$m/go.mod $$backup/$$m/go.mod; \
	  if [ -f $$m/go.sum ]; then cp $$m/go.sum $$backup/$$m/go.sum; fi; \
	done; \
	for m in $(MODULES); do \
	  echo "==> tidy-check $$m"; GOWORK=off go -C $$m mod tidy || status=1; \
	done; \
	if [ $$status -eq 0 ]; then \
	  dirty=$$(git status --porcelain -- '*go.mod' '*go.sum' go.work go.work.sum); \
	  if [ -n "$$dirty" ]; then echo "dependency files are not tidy:"; echo "$$dirty"; status=1; fi; \
	fi; \
	for m in $(MODULES); do \
	  cp $$backup/$$m/go.mod $$m/go.mod; \
	  if [ -f $$backup/$$m/go.sum ]; then cp $$backup/$$m/go.sum $$m/go.sum; else rm -f $$m/go.sum; fi; \
	done; \
	rm -rf $$backup; exit $$status

tools-test: ## Run the test suites of the scripts in tools/
	@$(PYTHON) -c 'import sys; sys.exit(0 if sys.version_info >= (3, 11) else 1)' || { \
	  echo "tools-test needs Python 3.11 or newer (tomllib); $(PYTHON) is $$($(PYTHON) -V 2>&1)." >&2; \
	  echo "Install the version .mise.toml pins, or pass PYTHON=<interpreter>." >&2; \
	  exit 1; \
	}
	@$(PYTHON) -m unittest discover -s tools -t tools -p 'test_*.py'

check: ## Run everything CI runs
	@$(MAKE) build
	@$(MAKE) test
	@$(MAKE) lint
	@$(MAKE) tidy-check
	@$(MAKE) tools-test
