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
FROM cgr.dev/chainguard/go:latest@sha256:746a18254ff1f65968e411ab89d15c49fc91075d9cc78e65ead081236ea15bbd AS build
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
FROM cgr.dev/chainguard/wolfi-base:latest@sha256:30f03343947c7ae3581fda727a6e2aa7b8ce7009b7bfc2ab8d5c9483ace5812f
RUN apk add --no-cache git git-lfs ca-certificates-bundle && \
    git lfs install --system
COPY --from=build /out/gitdr /usr/bin/gitdr
USER 65532:65532
ENV GIT_TERMINAL_PROMPT=0
ENTRYPOINT ["/usr/bin/gitdr"]
