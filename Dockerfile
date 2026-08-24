# Build stage
FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /vedetta ./cmd/vedetta

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

ENTRYPOINT ["vedetta"]
CMD ["-config", "/config/config.yml"]
