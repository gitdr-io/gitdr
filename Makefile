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
ACTIONLINT_VERSION ?= v1.7.9

.PHONY: build build-dist test test-integration lint vuln actionlint semgrep image fmt tidy ci clean

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
