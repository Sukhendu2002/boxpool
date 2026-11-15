.DEFAULT_GOAL := help

GO ?= go
PYTHON ?= python3
NODE ?= node

.PHONY: help build check check-format fmt vet test test-scripts test-npm docs docs-check site test-site

help:
	@printf '%s\n' \
	  'Boxpool contributor commands (run from the repository root):' \
	  '  make build         Build bin/boxpool without installing or starting it' \
	  '  make check         Run formatting, vet, unit tests, build and docs checks' \
	  '  make fmt           Format Go source' \
	  '  make vet           Run go vet' \
	  '  make test          Run Go unit tests (no hypervisor required)' \
	  '  make test-scripts  Run Python tooling tests' \
	  '  make test-npm      Run npm launcher tests' \
	  '  make docs          Regenerate docs/commands.md from CLI help' \
	  '  make docs-check    Check that generated CLI documentation is current' \
	  '  make site          Build landing page and docs (npm ci --prefix site first)' \
	  '  make test-site     Check site rendering, routes and distribution endpoints' \
	  'See CONTRIBUTING.md for setup and platform integration tests.'

build:
	$(GO) build ./...
	$(GO) build -o bin/boxpool ./cmd/boxpool

check: check-format vet test test-scripts test-npm docs-check

check-format:
	@files=$$(gofmt -l .) || exit $$?; \
	  test -z "$$files" || { printf '%s\n' "$$files"; echo 'Run make fmt to format Go source.' >&2; exit 1; }

fmt:
	gofmt -w .

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

test-scripts:
	$(PYTHON) -m unittest discover -s scripts/tests -p 'test_*.py'

test-npm:
	$(NODE) --test npm/test/*.test.js

docs: build
	$(PYTHON) scripts/generate-command-reference.py --binary bin/boxpool

docs-check: build
	$(PYTHON) scripts/generate-command-reference.py --binary bin/boxpool --check

site:
	$(NODE) site/build.mjs

test-site:
	$(NODE) --test site/test/*.test.mjs
