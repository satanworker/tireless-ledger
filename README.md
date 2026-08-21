# pi-memoryd

`pi-memoryd` is the single recall service for indexed Pi and Codex session windows. Production is one Go process linked to LanceDB through CGO; clients continue to use the existing HTTP API. Embeddings are produced by clients or the optional llama.cpp service and are not part of the daemon.

```text
clients -> pi-memoryd (:8090) -> lancedb-go/CGO -> R2 Lance table `turns`
```

The live source of truth is `s3://<bucket>/session-recall-lance/`. The daemon does not copy the table into RAM or run a Python/HTTP storage sidecar.

## Builds

Production Linux/ARM64 builds use Docker. The image pins `lancedb-go` main, advances its Rust engine to stable LanceDB 0.37.1 / Lance 10, builds `liblancedb_go.a` with AWS support, applies the checked-in 256 MiB index / 64 MiB metadata session-cache overlay, and links it into the Go 1.24 daemon.

```bash
make secrets-decrypt
docker compose build pi-memoryd
docker compose up -d pi-memoryd
```

After a bulk ingest, run one offline maintenance pass to compact the final
fragment tail and fold newly written rows into the existing FTS and scalar
indexes:

```bash
docker compose run --rm --no-deps pi-memoryd --optimize
```

On the VPS this is automated by a persistent user timer. It runs daily at
04:30 UTC with up to 15 minutes of randomized delay and catches up after
downtime. Install or refresh it with:

```bash
make install-optimize-timer
```

The timer uses a non-blocking runtime lock, so delayed timer invocations cannot
overlap an optimization already in progress.

For corpora above roughly 10,000 rows, an explicitly reversible IVF-Flat index can reduce dense-search latency while retaining full-precision vectors:

```sh
docker compose run --rm --no-deps pi-memoryd --create-vector-index --optimize
# Roll back to exhaustive vector scans:
docker compose run --rm --no-deps pi-memoryd --drop-vector-index
```

`llama-embed` remains an optional separate process:

```bash
docker compose --profile embed up -d llama-embed
```

Mac and portable unit tests deliberately stay pure Go and use `memory://`:

```bash
CGO_ENABLED=0 go test ./...
make recall-local
```

A CGO-disabled binary rejects S3 storage with a clear startup error.

## Lance behavior

- Merge-insert on `id`; an ingest response is successful only after persistence.
- ANN, BM25, and combined hybrid search with LanceDB RRF.
- Session walk filters in Lance, applies the `(after_ts, after_id)` cursor, and returns `(timestamp, id)` order.
- FTS index on `forward_content` and B-tree index on `session_id`.
- One-row scan and dummy FTS warmup during startup.
- Compact at 16 data fragments, then prune obsolete versions immediately.

## HTTP API

- `GET /-/ready`
- `POST /v1/memory/ingest`
- `POST /v1/memory/query`
- `GET /v1/memory/session?session_id=...&after_ts=...&after_id=...`

Example hybrid query:

```json
{
  "query_vector": [0.1, 0.2],
  "query_text": "wallet activation",
  "session_id": "session-id",
  "limit": 5
}
```

Production uses 384-dimensional `BAAI/bge-small-en-v1.5` vectors. Query prefixes belong only on query embeddings; do not mix embeddings from different model/runtime configurations in the same table.

## Production environment

| Variable | Purpose |
|---|---|
| `PI_MEMORYD_STORAGE_URL` | Lance database URI, normally `s3://<bucket>/session-recall-lance` |
| `PI_MEMORYD_VECTOR_NPROBES` | IVF partitions scanned per dense query; defaults to all 64 for exhaustive coverage |
| `PI_MEMORYD_S3_BUCKET` | Compose bucket interpolation |
| `PI_MEMORYD_S3_ENDPOINT` | R2 S3 endpoint; passed as Lance `aws_endpoint` |
| `PI_MEMORYD_KEY_ID` | R2 access key ID |
| `PI_MEMORYD_APPLICATION_KEY` | R2 secret key |
| `AWS_REGION` | R2 region (`auto` is supported) |
| `PI_MEMORYD_DIMENSIONS` | Vector size, default `384` |
| `PI_MEMORYD_STATE` | Local dedup registry path |

Encrypted values live in `secrets/pi-memoryd.sops.env`; `.env` is generated locally and must not be committed.
