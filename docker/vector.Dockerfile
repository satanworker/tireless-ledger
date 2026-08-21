FROM ubuntu:24.04
ARG VERSION=0.2.2
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
  && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /tmp/v.tgz \
    "https://github.com/opendata-oss/opendata/releases/download/opendata-vector/v${VERSION}/opendata-vector-${VERSION}-aarch64-unknown-linux-gnu.tar.gz" \
  && tar -xzf /tmp/v.tgz -C /usr/local/bin \
  && chmod +x /usr/local/bin/opendata-vector \
  && rm /tmp/v.tgz
EXPOSE 8080
ENTRYPOINT ["opendata-vector"]
CMD ["--port", "8080", "vector", "--config", "/config/vector.yaml"]
