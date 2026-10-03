# syntax=docker/dockerfile:1
#
# gitdr hardened image. Two stages: a static build, then a Wolfi runtime that carries
# git + git-lfs (which gitdr shells out to). Buildx-ready for linux/amd64 + linux/arm64.
#
# Build:  docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=$(git describe --tags --always) -t gitdr .
#
# Notes for hardening (see SPEC §6):
#   - Runs non-root (uid 65532). Pair with the chart's read-only rootfs + a writable
#     /tmp emptyDir (gitdr clones/bundles into os.MkdirTemp).
#   - Pin the base images by @sha256 digest in release CI (M8). Tags are used here for
#     readability.
#   - "No shell": wolfi-base ships a minimal shell because git/git-lfs are dynamically
#     linked and need a libc runtime; a fully shell-free image needs an apko build
#     (tracked separately). This Dockerfile is the portable, buildable form.

# ---- build: fully static gitdr binary ----
FROM cgr.dev/chainguard/go:latest@sha256:e93cc0ce6c290b4a13d59603c9eec15220312a6b0474428e6d9aa6ac3ee98766 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X gitdr.io/gitdr/internal/cli.Version=${VERSION}" \
    -o /out/gitdr ./cmd/gitdr

# ---- runtime: wolfi + git + git-lfs, non-root ----
FROM cgr.dev/chainguard/wolfi-base:latest@sha256:08df5982c3d27e70a4ce1607e3bb9af09d746f8722cf135a7694afef879fc5a2
RUN apk add --no-cache git git-lfs ca-certificates-bundle && \
    git lfs install --system
# Owned by root, mode 0711: the user it runs as can execute it and not read it, so the kernel
# starts it non-dumpable and git, which runs as that same user, cannot read its environment.
COPY --from=build --chmod=0711 /out/gitdr /usr/bin/gitdr
USER 65532:65532
ENV GIT_TERMINAL_PROMPT=0
ENTRYPOINT ["/usr/bin/gitdr"]
