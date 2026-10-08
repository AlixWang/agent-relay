# agent-relay image. Built by .github/workflows/release.yml for
# linux/amd64 + linux/arm64 (pushes to GHCR on every v* tag).
# Local: docker build -t agent-relay . &&
#   docker run -v agent-relay-data:/var/lib/agent-relay -p 18789:18789 agent-relay
FROM --platform=$BUILDPLATFORM golang:1.22-bookworm AS build
ARG TARGETOS TARGETARCH
# Release tag for the update console + asset cache-busting. CI passes
# VERSION=v*; local builds report dev (update downgrade guard skips dev).
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Receiver revision (DESIGN §8.9): the client_update predicate. CI passes the
# hash of cmd/relay-tail/**/*.go; when the build can compute it itself (full
# source present) do that, so local/docker builds still nudge correctly.
# Empty = never nudge (safe default; the console shows "dev").
ARG RECEIVER_REV=""
RUN REV="${RECEIVER_REV:-$(find cmd/relay-tail -name '*.go' -type f | LC_ALL=C sort | xargs cat | sha256sum | cut -c1-12)}" \
  && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
     -ldflags "-X github.com/AlixWang/agent-relay/internal/web.assetVersion=${VERSION} -X github.com/AlixWang/agent-relay/internal/gateway.receiverRev=${REV}" \
     -o /out/agent-relay ./cmd/agent-relay

FROM debian:bookworm-slim
RUN useradd -r -d /var/lib/agent-relay agent-relay \
  && mkdir -p /var/lib/agent-relay /etc/agent-relay \
  && chown agent-relay:agent-relay /var/lib/agent-relay
COPY --from=build /out/agent-relay /usr/local/bin/agent-relay
COPY configs/config.toml.example /etc/agent-relay/config.toml.example
VOLUME ["/var/lib/agent-relay"]
EXPOSE 18789
USER agent-relay
ENTRYPOINT ["/usr/local/bin/agent-relay", "-config", "/etc/agent-relay/config.toml"]
