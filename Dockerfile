# agent-relay image. Built by .github/workflows/release.yml for
# linux/amd64 + linux/arm64 (pushes to GHCR on every v* tag).
# Local: docker build -t agent-relay . &&
#   docker run -v agent-relay-data:/var/lib/agent-relay -p 18789:18789 agent-relay
FROM --platform=$BUILDPLATFORM golang:1.22-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/agent-relay ./cmd/agent-relay

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
