BINARY      := git-remote-oci
COVERPROFILE := coverage.out
# Where the raw coverage data lands before it is merged into COVERPROFILE.
COVERDIR     := .coverdata
GO          ?= go

# How long each fuzz target runs. The default is a smoke test: long enough to
# catch a target that no longer builds or whose seed corpus now fails. The
# scheduled campaign in .github/workflows/fuzz.yml overrides it.
FUZZTIME    ?= 15s

# Minimum statement coverage, as a percentage. Coverage was measured and
# uploaded for a long time without anything ever looking at it, which is not
# reporting. The floor sits a little under the current figure so that ordinary
# work does not trip it, and a real regression does. Raise it as coverage
# improves; lowering it should take an argument.
COVER_MIN   ?= 77

# Pinned tool versions. `make lint` and the CI lint job read the same value, so
# the two cannot drift; bump it here and nowhere else. govulncheck is pinned for
# the same reason a linter is: a floating `@latest` can change what a run
# reports without anything in the repository changing.
GOLANGCI_LINT_VERSION ?= v2.13.2
GOVULNCHECK_VERSION   ?= v1.8.0

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary
	$(GO) build -trimpath -o $(BINARY) .

.PHONY: fmt
fmt: ## Format all Go files
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go file is unformatted
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "These files are not gofmt-clean:"; echo "$$out"; exit 1; \
	fi

.PHONY: tidy
tidy: ## Run go mod tidy
	$(GO) mod tidy

.PHONY: tidy-check
tidy-check: ## Fail if go.mod/go.sum are not tidy
	@cp go.mod go.mod.tidycheck && cp go.sum go.sum.tidycheck
	@$(GO) mod tidy; status=$$?; \
	if [ $$status -ne 0 ]; then \
		mv go.mod.tidycheck go.mod; mv go.sum.tidycheck go.sum; exit $$status; \
	fi; \
	if ! diff -q go.mod.tidycheck go.mod >/dev/null || ! diff -q go.sum.tidycheck go.sum >/dev/null; then \
		echo "go.mod/go.sum are not tidy; run 'make tidy' and commit the result."; \
		diff -u go.mod.tidycheck go.mod || true; \
		diff -u go.sum.tidycheck go.sum || true; \
		rm -f go.mod.tidycheck go.sum.tidycheck; exit 1; \
	fi; \
	rm -f go.mod.tidycheck go.sum.tidycheck

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint at GOLANGCI_LINT_VERSION (config: .golangci.yml)
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

# For CI to read the pin, so that .github/workflows/ci.yml does not carry a
# second copy of it. Deliberately not in `make help`.
.PHONY: golangci-lint-version
golangci-lint-version:
	@echo $(GOLANGCI_LINT_VERSION)

.PHONY: test
test: ## Run unit tests with the race detector
	$(GO) test -race ./pkg/...

.PHONY: cover
# Coverage comes from two places and both have to be counted.
#
# -coverpkg attributes coverage to the package that *owns* the code rather than
# the package whose test ran it. Without it, everything pkg/gc's tests exercise
# in pkg/oci counts as uncovered, and the reported figure understates reality by
# about five points while listing exercised functions at 0%.
#
# The other place is the helper binary itself. Protocol v2 is exercised by
# spawning it and driving real `git` against it, which is the only way to know
# whether git accepts the byte stream — but Go attributes coverage to the
# process that produced it, so a thousand tested lines counted as zero and took
# the total from 80% to below the floor. GRO_COVERDIR tells those tests to build
# an instrumented binary and collect what it records; covdata merges the two
# sets before anything is measured.
cover: ## Run unit tests with coverage and fail below COVER_MIN (default 77%)
	rm -rf $(COVERDIR)
	mkdir -p $(COVERDIR)/unit $(COVERDIR)/subprocess $(COVERDIR)/merged
	GRO_COVERDIR=$(CURDIR)/$(COVERDIR)/subprocess \
		$(GO) test -race -coverpkg=./pkg/... -covermode=atomic ./pkg/... \
		-args -test.gocoverdir=$(CURDIR)/$(COVERDIR)/unit
	$(GO) tool covdata merge -i=$(COVERDIR)/unit,$(COVERDIR)/subprocess -o=$(COVERDIR)/merged
	$(GO) tool covdata textfmt -i=$(COVERDIR)/merged -o=$(COVERPROFILE)
	@$(GO) tool cover -func=$(COVERPROFILE) | tail -1
	@total=$$($(GO) tool cover -func=$(COVERPROFILE) | tail -1 | grep -oE '[0-9]+\.[0-9]+' | tail -1); \
	LC_ALL=C awk -v got="$$total" -v min="$(COVER_MIN)" 'BEGIN { \
		if (got+0 < min+0) { \
			printf "coverage %.1f%% is below the %s%% floor\n", got, min; exit 1 \
		} \
		printf "coverage %.1f%% (floor %s%%)\n", got, min \
	}'

.PHONY: fuzz
fuzz: ## Run each fuzz target for FUZZTIME (default 15s)
	@for pkg in $$($(GO) list ./pkg/...); do \
		for target in $$($(GO) test -list '^Fuzz' $$pkg 2>/dev/null | grep '^Fuzz' || true); do \
			echo "==> $$pkg $$target ($(FUZZTIME))"; \
			$(GO) test $$pkg -run '^$$' -fuzz "^$$target$$" -fuzztime=$(FUZZTIME) || exit 1; \
		done; \
	done

.PHONY: vulncheck
vulncheck: ## Report known vulnerabilities reachable from this code
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: e2e
e2e: ## End-to-end tests against a real registry:3 container (needs Docker)
	$(GO) test -race -timeout 20m ./test/...

.PHONY: e2e-ghcr
e2e-ghcr: ## End-to-end tests against a real hosted registry (needs E2E_GHCR_REPO and credentials)
	@test -n "$$E2E_GHCR_REPO" || { echo "set E2E_GHCR_REPO, e.g. ghcr.io/<owner>/<name>/e2e"; exit 1; }
	$(GO) test -timeout 15m -run 'TestGHCR' -v ./test/

.PHONY: bench
# -v is not optional here: the suite reports its timings with t.Logf, and
# `go test` discards those for a passing test. Without it the benchmark runs for
# the better part of an hour and prints nothing at all.
bench: ## Large-scale benchmark suite (needs Docker; slow)
	$(GO) test -v -timeout 45m ./benchmark

.PHONY: check
check: fmt-check tidy-check vet lint test vulncheck ## Everything CI runs on a pull request

.PHONY: clean
clean: ## Remove build and coverage artifacts
	rm -f $(BINARY) $(COVERPROFILE)
	rm -rf dist/ $(COVERDIR)
