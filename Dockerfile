# syntax=docker/dockerfile:1.7

FROM rust:1.91-bookworm AS lance-build
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential clang cmake git libprotobuf-dev libssl-dev pkg-config protobuf-compiler \
  && rm -rf /var/lib/apt/lists/*
ARG LANCEDB_GO_COMMIT=fa14ce29c7724354f2cea630a1d3488b56bbd64b
# The overlay advances lancedb-go's stale Rust pin from v0.24.0 to v0.37.1
# (Lance 10), matching the engine used by the Python comparison sidecar.
RUN git clone https://github.com/lancedb/lancedb-go.git /opt/lancedb-go \
  && git -C /opt/lancedb-go checkout "$LANCEDB_GO_COMMIT"
COPY docker/lancedb-go-session-cache.patch /tmp/lancedb-go-session-cache.patch
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/opt/lancedb-go/rust/target \
    git -C /opt/lancedb-go apply /tmp/lancedb-go-session-cache.patch \
  && cargo build --manifest-path /opt/lancedb-go/rust/Cargo.toml --release --features aws \
  && mkdir -p /opt/lancedb-go/lib/linux_arm64 \
  && cp /opt/lancedb-go/rust/target/release/liblancedb_go.a /opt/lancedb-go/lib/linux_arm64/

FROM golang:1.24-bookworm AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY --from=lance-build /opt/lancedb-go /opt/lancedb-go
COPY . .
RUN go mod edit -replace github.com/lancedb/lancedb-go=/opt/lancedb-go \
  && CGO_ENABLED=1 CGO_LDFLAGS='/opt/lancedb-go/lib/linux_arm64/liblancedb_go.a -lm -ldl -lpthread' \
     go build -trimpath -ldflags='-s -w' -o /out/pi-memoryd .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
  && rm -rf /var/lib/apt/lists/*
COPY --from=go-build /out/pi-memoryd /usr/local/bin/pi-memoryd
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/pi-memoryd"]
