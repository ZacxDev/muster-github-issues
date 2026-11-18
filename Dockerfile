# syntax=docker/dockerfile:1

# muster-github-issues: GitHub issues to muster tasks, as a standalone image.
# The build stage runs on the BUILD platform and cross-compiles for TARGETARCH,
# so `docker buildx build --platform linux/amd64,linux/arm64` emulates nothing.

FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY internal/ ./internal/
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w" -o /muster-github-issues . && mkdir /state

# distroless/static: CA certificates for GitHub's and muster's HTTPS, no shell,
# non-root. Mount a volume at /state: it holds the outbox and the cursors.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /muster-github-issues /muster-github-issues
# Owned by distroless's nonroot user, so a named volume mounted here is writable.
COPY --from=build --chown=65532:65532 /state /state
ENV STATE_DIR=/state
VOLUME ["/state"]
ENTRYPOINT ["/muster-github-issues"]
