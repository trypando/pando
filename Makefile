.DEFAULT_GOAL := help
SHELL := /bin/bash

GO      ?= go
BIN     ?= bin/pando
PKG     := ./...

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Build and test
# ---------------------------------------------------------------------------

.PHONY: build
build: ## Build the pando binary
	$(GO) build -o $(BIN) ./cmd/pando

.PHONY: test
test: ## Run unit tests
	$(GO) test -race -count=1 $(PKG)

# COVERPROFILE is set by CI so the integration run's coverage can be uploaded
# under its own flag. Empty locally, where writing a profile nobody reads is
# just a slower test run.
#
# -coverpkg=./... is what makes the integration profile mean what it says. By
# default a package's coverage counts only its own tests, so an API test that
# drives the state store, the planner and the audit writer credits none of
# them — and those packages read as untested because the tests that exercise
# them live one package over.
COVERPROFILE ?=
COVERFLAGS := $(if $(COVERPROFILE),-coverpkg=./... -coverprofile=$(COVERPROFILE) -covermode=atomic,)

# Every integration package except the detection corpus, which has its own
# target and its own workflow below.
#
# The corpus clones ten real repositories over the network and took 425 of the
# 597 seconds this job spent — 71% of it, waiting on one package, on every
# push. Measured against the rest of the suite it covers twenty statements
# nothing else reaches, all of them in internal/detect. That is not a gate
# worth putting in front of every change; it is a gate worth running nightly.
#
# `-tags=integration` on the `go list` is load-bearing, not decoration. Both
# packages under test/ contain only integration-tagged files, so an untagged
# `go list ./...` does not report them as packages at all — and a list built
# without the tag would have quietly dropped test/acceptance, which is the four
# sequences design 07 calls the acceptance criteria for v1. Excluding the
# corpus must not exclude those.
#
# Lazy `=` rather than `:=` so the `go list` runs when a corpus-excluding
# target is invoked, and not on every make.
INTEGRATION_PKGS = $(shell $(GO) list -tags=integration ./... | grep -v '/test/corpus$$')

.PHONY: test-integration
test-integration: ## Run integration tests, minus the corpus (real Postgres + Docker)
	@# One Ryuk reaper serves every package in a run (testcontainers keys it on
	@# the go test session). By default it shuts down 10s after its last client
	@# leaves, and a package that looks it up while it is shutting down finds the
	@# dying container, waits for a port it will never map, and fails after 60s
	@# with "wait for reaper … context deadline exceeded" — which happened when
	@# the audit package started a few seconds after cmd/pando finished. Keeping
	@# the reaper for as long as a run may last closes that window; it still
	@# removes everything once the run is over.
	TESTCONTAINERS_RYUK_RECONNECTION_TIMEOUT=15m \
		$(GO) test -race -count=1 -timeout=15m -tags=integration $(COVERFLAGS) $(INTEGRATION_PKGS)

# Several replicas against one Postgres, behind a load balancer (issue #72).
# Its own project and ports, so it never touches a stack somebody is using:
# brought up, changed under test, the design 07 sequences run through it, and
# taken down with its volumes whether or not anything failed.
REPLICAS_PROJECT  ?= pando-replicas
# The acceptance suite's own setup password: test/replicas claims the fresh
# install with it through POST /setup, and the sequences then sign in with it.
REPLICAS_PASSWORD ?= pando-acceptance-suite-admin
# Shared by the stack and the tests: the ports, and the compressed retry
# schedule test/acceptance/README.md describes, which the crash-loop test
# sizes its deadlines from. A test that recreates the server reads the ports
# from here too, or `docker compose up` would fall back to 8080.
#
# Small per-app reservations, so the suite's apps fit a laptop's Docker: at the
# shipped 1 CPU each, a four-CPU daemon refuses every deploy after the fourth
# (R-242, no oversubscription).
REPLICAS_ENV = PANDO_PORT=18080 PANDO_APP_PORT_START=19000 PANDO_APP_PORT_END=19019 \
	PANDO_RECONCILER_BACKOFF=0s,1s,2s,3s,4s PANDO_RECONCILER_FAILURE_WINDOW=2m \
	PANDO_APPS_CPU_MILLIS=100 PANDO_APPS_MEMORY_BYTES=134217728
REPLICAS_COMPOSE = $(REPLICAS_ENV) docker compose -p $(REPLICAS_PROJECT) \
	-f docker-compose.yml -f test/replicas/docker-compose.replicas.yml
# COMPOSE_PROJECT_NAME and COMPOSE_FILE point the acceptance suite's own
# `docker compose exec postgres …` at this stack rather than the default one.
REPLICAS_TEST_ENV = $(REPLICAS_ENV) PANDO_REPLICAS_PROJECT=$(REPLICAS_PROJECT) \
	PANDO_TEST_URL=http://localhost:18080/api/v1 PANDO_TEST_PASSWORD=$(REPLICAS_PASSWORD) \
	COMPOSE_PROJECT_NAME=$(REPLICAS_PROJECT) \
	COMPOSE_FILE=$(CURDIR)/docker-compose.yml:$(CURDIR)/test/replicas/docker-compose.replicas.yml

.PHONY: test-replicas
test-replicas: ## Run Pando as two replicas behind a balancer: topology changes, then the design 07 sequences
	$(REPLICAS_COMPOSE) up -d --build --wait
	@status=0; \
	$(REPLICAS_TEST_ENV) $(GO) test -count=1 -timeout=30m -tags=integration ./test/replicas/ || status=1; \
	$(REPLICAS_TEST_ENV) $(GO) test -count=1 -timeout=60m -tags=integration ./test/acceptance/ || status=1; \
	$(REPLICAS_COMPOSE) down -v --remove-orphans; \
	exit $$status

# The load harness (issue #72, test/load/README.md): a scale tier seeded into
# the replicas stack, scaled to LOAD_REPLICAS, with console, API and proxy
# traffic ramped through the balancer until it holds or breaks. Its own project
# and ports, so it runs beside test-replicas or a development stack. The
# report is written to LOAD_DIR; the stack, its volumes, and the containers
# of the real apps it deployed are removed whether or not anything failed.
TIER                ?= vm
LOAD_PROJECT        ?= pando-load
LOAD_REPLICAS       ?= $(if $(filter cluster,$(TIER)),4,2)
LOAD_REAL_APPS      ?= $(if $(filter cluster,$(TIER)),20,10)
LOAD_HOLD           ?= 3m
LOAD_DIR            ?= test/load/out
LOAD_PORT           ?= 28080
LOAD_APP_PORT_START ?= 29000
LOAD_DB_PORT        ?= 25432
# Anything else for `run`, e.g. LOAD_ARGS="-keep-going -steps 0.5,1".
LOAD_ARGS           ?=
# The port range holds every real app, and at least the shipped twenty.
LOAD_APP_PORT_END = $(shell echo $$(( $(LOAD_APP_PORT_START) + ($(LOAD_REAL_APPS) > 20 ? $(LOAD_REAL_APPS) : 20) - 1 )))
# Each replica's pool (32), the leader's lock connection and the archiver,
# and the harness.
LOAD_PG_MAX_CONNECTIONS = $(shell echo $$(( $(LOAD_REPLICAS) * 40 + 60 )))
# Later assignments win, so these replace REPLICAS_ENV's ports.
LOAD_ENV = $(REPLICAS_ENV) PANDO_PORT=$(LOAD_PORT) \
	PANDO_APP_PORT_START=$(LOAD_APP_PORT_START) PANDO_APP_PORT_END=$(LOAD_APP_PORT_END) \
	LOAD_DB_PORT=$(LOAD_DB_PORT) LOAD_PG_MAX_CONNECTIONS=$(LOAD_PG_MAX_CONNECTIONS)
LOAD_COMPOSE = $(LOAD_ENV) docker compose -p $(LOAD_PROJECT) -f docker-compose.yml \
	-f test/replicas/docker-compose.replicas.yml -f test/load/docker-compose.load.yml
LOAD_FLAGS = -tier $(TIER) -url http://localhost:$(LOAD_PORT) -admin-password $(REPLICAS_PASSWORD) \
	-real-apps $(LOAD_REAL_APPS)

.PHONY: load-test
load-test: ## Seed a scale tier (TIER=vm|cluster) into LOAD_REPLICAS replicas, ramp load through the balancer, write a report
	@case "$(TIER)" in vm|cluster) ;; *) echo "TIER must be vm or cluster, not '$(TIER)'"; exit 2;; esac
	mkdir -p $(LOAD_DIR)
	$(GO) build -o $(LOAD_DIR)/load ./test/load
	$(LOAD_COMPOSE) up -d --build --wait --scale pando=$(LOAD_REPLICAS)
	@status=0; \
	export LOAD_DATABASE_URL="postgres://pando:$${POSTGRES_PASSWORD:-pando}@127.0.0.1:$(LOAD_DB_PORT)/pando?sslmode=disable"; \
	$(LOAD_DIR)/load seed $(LOAD_FLAGS) -real-port-start $(LOAD_APP_PORT_START) || status=1; \
	if [ $$status = 0 ]; then \
		$(LOAD_DIR)/load run $(LOAD_FLAGS) -replicas $(LOAD_REPLICAS) -hold $(LOAD_HOLD) \
			-commit "$$(git rev-parse --short HEAD)" -out $(LOAD_DIR)/results-$(TIER).json $(LOAD_ARGS) || status=1; \
	fi; \
	if [ -f $(LOAD_DIR)/results-$(TIER).json ]; then \
		$(LOAD_DIR)/load report -in $(LOAD_DIR)/results-$(TIER).json -out $(LOAD_DIR)/report-$(TIER).md \
			&& echo "report: $(LOAD_DIR)/report-$(TIER).md" || status=1; \
	fi; \
	$(LOAD_DIR)/load cleanup $(LOAD_FLAGS) -real-apps-file $(LOAD_DIR)/real-apps-$(TIER).txt || true; \
	$(LOAD_COMPOSE) down -v --remove-orphans; \
	for app in $$(cat $(LOAD_DIR)/real-apps-$(TIER).txt 2>/dev/null); do \
		ids=$$(docker ps -aq --filter label=io.pando.bundle=$$app); \
		[ -z "$$ids" ] || docker rm -f $$ids >/dev/null; \
		nets=$$(docker network ls -q --filter label=io.pando.bundle=$$app); \
		[ -z "$$nets" ] || docker network rm $$nets >/dev/null; \
	done; \
	exit $$status

.PHONY: vet
vet: ## go vet, including the integration-tagged tests
	$(GO) vet $(PKG)
	# Behind a build tag, so `go vet ./...` never sees them and they rot in
	# silence: two of them had been referencing a renamed constant and an old
	# function signature for long enough that nobody could say when it started.
	# Vet compiles them without needing Docker or Postgres, which is the whole
	# cost of never letting that happen again.
	$(GO) vet -tags integration $(PKG)

# `go install` puts binaries in GOPATH/bin, which is not on PATH by default — so
# following the install line printed below leaves the next `make lint` still
# reporting the tool as missing. Look there as well as on PATH.
GOBIN := $(shell go env GOPATH)/bin
LINT := $(shell command -v golangci-lint 2>/dev/null || echo $(GOBIN)/golangci-lint)

.PHONY: lint
lint: ## golangci-lint, including the R-027 adapter import rule
	@test -x "$(LINT)" \
		|| { echo "golangci-lint v2 not installed:"; \
		     echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; exit 1; }
	$(LINT) run

.PHONY: check
check: vet lint test reference-check ## Everything CI runs on a pull request

# Fuzz targets, and how long each one runs. One list so that adding a target
# means adding a line here, rather than adding a line here and remembering to
# add one to the workflow as well.
FUZZ_TIME ?= 60s
FUZZ_TARGETS := \
	./internal/hash:FuzzVerifyEncodedHash \
	./internal/core/assertion:FuzzVerifyToken \
	./internal/core/backup:FuzzDecryptEnvelope \
	./internal/detect:FuzzImportCompose

.PHONY: fuzz
fuzz: ## Fuzz the parsers that read untrusted input (FUZZ_TIME per target)
	@# One at a time, because `go test -fuzz` takes exactly one package and
	@# because a target saturating every core tells the others nothing.
	@set -e; for entry in $(FUZZ_TARGETS); do \
		pkg=$${entry%%:*}; target=$${entry##*:}; \
		echo "==> $$target ($$pkg) for $(FUZZ_TIME)"; \
		$(GO) test -run=XXX -fuzz=$$target -fuzztime=$(FUZZ_TIME) $$pkg; \
	done

.PHONY: vulncheck
vulncheck: ## Check dependencies for known vulnerabilities (govulncheck + the allowlist)
	@# GOTOOLCHAIN=local so govulncheck is built with the Go on PATH. x/vuln's
	@# go.mod names an older toolchain, and a govulncheck built with that one
	@# refuses to analyze newer source with "package requires newer Go version".
	GOTOOLCHAIN=local $(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	$(GOBIN)/govulncheck -format=json $(PKG) > vulns.json || true
	python3 scripts/check-vulns.py vulns.json .github/govulncheck-allowlist.txt
	@rm -f vulns.json

.PHONY: fuzz-seeds
fuzz-seeds: ## Run every fuzz target's seed corpus only — what `make test` already does
	$(GO) test -run='^Fuzz' -count=1 $(PKG)

# ---------------------------------------------------------------------------
# Code generation
# ---------------------------------------------------------------------------

.PHONY: sqlc
sqlc: ## Regenerate typed queries from SQL
	sqlc generate

.PHONY: console
console: ## Build the console into assets embedded by internal/console
	cd console && npm ci && npm run build

.PHONY: console-check
console-check: ## Brand adherence and types for the console (needs npm)
	cd console && npm ci && npm run check

# ---------------------------------------------------------------------------
# Detection

# Not part of `make test-integration`, and not on the push path. It clones ten
# real repositories, so it is slow and it depends on ten third-party projects
# staying reachable — two properties that belong to a nightly run rather than
# to every pull request. The corpus workflow runs it nightly, on demand, and on
# a pull request that touches detection; `make vet` still compiles it on every
# push, so it cannot rot in silence.
.PHONY: detection-corpus
detection-corpus: ## Run detection against the corpus of real repositories (network, slow)
	$(GO) test -tags=integration -count=1 -timeout=20m -v ./test/corpus/

# ---------------------------------------------------------------------------
# Requirement traceability (design 00 §4)
# ---------------------------------------------------------------------------

.PHONY: reference
reference: ## Regenerate docs/api.md, cli.md, mcp.md and events.md from the code
	$(GO) run ./cmd/gen-reference docs

.PHONY: reference-check
reference-check: ## Fail if the generated reference is out of date
	@# Generated into a temporary directory and compared, rather than written
	@# over the checked-in files and diffed against the index: the question is
	@# whether what is on disk matches what the code produces, which is a
	@# different question from whether it has been committed yet. Comparing
	@# against the index failed in any tree where the reference had legitimately
	@# changed and not yet been committed — which is every tree that just added
	@# a route.
	@tmp=$$(mktemp -d); \
	$(GO) run ./cmd/gen-reference $$tmp > /dev/null; \
	for f in api.md cli.md mcp.md events.md; do \
		diff -u docs/$$f $$tmp/$$f > /dev/null \
			|| { echo "docs/$$f is out of date. Run 'make reference' and commit the result."; rm -rf $$tmp; exit 1; }; \
	done; \
	rm -rf $$tmp

.PHONY: requirements-index
requirements-index: ## Regenerate docs/traceability/requirements-index.md
	python3 scripts/gen-requirements-index.py

.PHONY: requirements-coverage
requirements-coverage: ## Report which R-IDs have a named acceptance test
	python3 scripts/gen-requirements-index.py --coverage

# ---------------------------------------------------------------------------
# Release
# ---------------------------------------------------------------------------

.PHONY: release-check
release-check: ## Validate .goreleaser.yaml (needs goreleaser)
	goreleaser check

.PHONY: release-snapshot
release-snapshot: ## Build the release artifacts locally, publishing nothing
	goreleaser release --snapshot --clean

# ---------------------------------------------------------------------------
# Housekeeping
# ---------------------------------------------------------------------------

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist coverage.out vulns.json
