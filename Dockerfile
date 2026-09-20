# syntax=docker/dockerfile:1.7

FROM rust:1.91-bookworm@sha256:c1e5f19e773b7878c3f7a805dd00a495e747acbdc76fb2337a4ebf0418896b33 AS lance-build
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential clang cmake git libprotobuf-dev libssl-dev pkg-config protobuf-compiler \
  && rm -rf /var/lib/apt/lists/*
ARG LANCEDB_GO_COMMIT=fa14ce29c7724354f2cea630a1d3488b56bbd64b
# The overlay advances lancedb-go's stale Rust pin from v0.24.0 to v0.37.1
# (Lance 10), matching the engine used by the Python comparison sidecar.
RUN git clone https://github.com/lancedb/lancedb-go.git /opt/lancedb-go \
  && git -C /opt/lancedb-go checkout "$LANCEDB_GO_COMMIT"
COPY docker/lancedb-go-session-cache.patch /tmp/lancedb-go-session-cache.patch
COPY docker/lancedb-go-disk-cache.patch /tmp/lancedb-go-disk-cache.patch
COPY docker/lancedb-go-disk-cache.rs /tmp/disk_cache.rs
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/opt/lancedb-go/rust/target \
    git -C /opt/lancedb-go apply /tmp/lancedb-go-session-cache.patch \
  && git -C /opt/lancedb-go apply /tmp/lancedb-go-disk-cache.patch \
  && cp /tmp/disk_cache.rs /opt/lancedb-go/rust/src/disk_cache.rs \
  && CARGO_BUILD_JOBS=1 cargo build --manifest-path /opt/lancedb-go/rust/Cargo.toml --release --features aws \
  && mkdir -p /opt/lancedb-go/lib/linux_arm64 \
  && cp /opt/lancedb-go/rust/target/release/liblancedb_go.a /opt/lancedb-go/lib/linux_arm64/

FROM golang:1.24-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY --from=lance-build /opt/lancedb-go /opt/lancedb-go
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod edit -replace github.com/lancedb/lancedb-go=/opt/lancedb-go \
  && GOMAXPROCS=1 CGO_ENABLED=1 CGO_LDFLAGS='/opt/lancedb-go/lib/linux_arm64/liblancedb_go.a -lm -ldl -lpthread' \
     go build -trimpath -ldflags='-s -w' -o /out/pi-memoryd .

FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
  && rm -rf /var/lib/apt/lists/*
COPY --from=go-build /out/pi-memoryd /usr/local/bin/pi-memoryd
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/pi-memoryd"]
