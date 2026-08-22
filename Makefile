# gitdr, build, test, lint. Release binaries are static and Linux-only.
BINARY  := gitdr
PKG     := gitdr.io/gitdr
CMD     := ./cmd/gitdr
GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG)/internal/cli.Version=$(VERSION)

# Tools run via pinned `go run` so they stay out of go.mod (keeps the dep graph minimal).
GOLANGCI_VERSION  ?= v2.12.2
GOVULN_VERSION    ?= v1.3.0
ACTIONLINT_VERSION ?= v1.7.12

.PHONY: build build-dist test test-integration test-ci fuzz lint vuln actionlint semgrep image fmt tidy ci clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(CMD)

# Static release binaries: linux/amd64 + linux/arm64. No other targets.
build-dist:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/$(BINARY)_linux_amd64 $(CMD)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/$(BINARY)_linux_arm64 $(CMD)

test:
	$(GO) test ./...

# Needs an object-lock MinIO/S3; set GITDR_TEST_S3_ENDPOINT (and AWS_* creds).
test-integration:
	$(GO) test -tags integration -count=1 ./...

# What CI runs: everything, including the integration tag, and no test may skip.
#
# Go prints nothing for a skipped test and still reports ok for the package, so an
# environment-gated test whose dependency was never wired up is invisible -- three separate
# code paths in this repo stayed unproven that way, each with a comment claiming CI ran it.
# Locally skipping is correct (no emulators); in CI a skip means something is not being
# tested and the job should say so.
test-ci:
	@log=$$(mktemp); \
	$(GO) test -tags integration -count=1 -v ./... > $$log 2>&1; status=$$?; \
	grep -E '^(ok|FAIL|\?|--- FAIL|--- SKIP)' $$log || true; \
	if [ $$status -ne 0 ]; then grep -B20 -E '^(--- FAIL|FAIL)' $$log | tail -60; rm -f $$log; exit $$status; fi; \
	if grep -q -- '--- SKIP' $$log; then \
	  echo; echo 'FAIL: tests skipped in CI. Every test must run here:'; \
	  grep -- '--- SKIP' $$log; rm -f $$log; exit 1; \
	fi; \
	rm -f $$log

# Timed fuzzing of every Fuzz target. The seed corpora already run as ordinary tests
# in `make test`, and that is the CI gate; this is the longer exploratory run, one
# target at a time because `go test -fuzz` accepts a single target per invocation.
#
# The minimization budget defaults to 60s PER INPUT, so on a short window every worker
# sits in minimization and the run reports ~0 execs/sec while doing nothing useful.
# Bound it well below FUZZTIME.
FUZZTIME         ?= 30s
FUZZMINIMIZETIME ?= 2s
FUZZFLAGS := -run '^$$' -fuzztime $(FUZZTIME) -fuzzminimizetime $(FUZZMINIMIZETIME)
fuzz:
	$(GO) test $(FUZZFLAGS) -fuzz '^FuzzVerifyManifest$$' ./internal/pipeline
	$(GO) test $(FUZZFLAGS) -fuzz '^FuzzExtractTar$$' ./internal/pipeline
	$(GO) test $(FUZZFLAGS) -fuzz '^FuzzDecrypt$$' ./internal/crypto
	$(GO) test $(FUZZFLAGS) -fuzz '^FuzzParseKeys$$' ./internal/crypto
	$(GO) test $(FUZZFLAGS) -fuzz '^FuzzSecretNeverLeaks$$' ./internal/redact
	$(GO) test $(FUZZFLAGS) -fuzz '^FuzzConfigLoad$$' ./internal/config

lint:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULN_VERSION) ./...

# The mirror's workflows. A bad one only shows up as a red run on a public main, so parse
# them here, where it costs nothing.
actionlint:
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) -color

# SAST. Requires semgrep on PATH (pip install semgrep / brew install semgrep).
semgrep:
	semgrep scan --error

IMAGE     ?= gitdr
PLATFORMS ?= linux/amd64,linux/arm64

# Hardened multi-arch image (needs Docker buildx). Append --push to publish.
image:
	docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

ci: tidy fmt lint test vuln actionlint

clean:
	rm -rf bin dist
