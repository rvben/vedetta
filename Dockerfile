# Build stage
FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# The version the image reports. Injected by `make docker-build` and by the
# release workflow so the dashboard, the API, and bug reports name a real
# release. A build that leaves it unset falls back to the VCS revision the Go
# toolchain records, never to a version it did not build.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=${VERSION}" -o /vedetta ./cmd/vedetta

# Runtime stage
FROM debian:bookworm-slim

LABEL org.opencontainers.image.source=https://github.com/rvben/vedetta
LABEL org.opencontainers.image.description="Vedetta NVR - lightweight network video recorder"
LABEL org.opencontainers.image.licenses=Apache-2.0

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

RUN groupadd -r vedetta && useradd -r -g vedetta -d /data -s /sbin/nologin vedetta

RUN mkdir -p /data/recordings /data/snapshots /config && \
    chown -R vedetta:vedetta /data /config

COPY --from=builder /vedetta /usr/local/bin/vedetta

USER vedetta
WORKDIR /data

EXPOSE 5050

VOLUME ["/data", "/config"]

# The recorder probes itself. This image carries no shell HTTP client, and the
# only endpoint that answers before authentication and before the readiness
# gate opens is /api/health/live, which the subcommand knows. It reads the same
# config the server does, so it follows a non-default port or TLS setting; pass
# a matching -config if CMD is overridden with a different path.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD ["vedetta", "healthcheck", "-config", "/config/config.yml"]

ENTRYPOINT ["vedetta"]
CMD ["-config", "/config/config.yml"]
