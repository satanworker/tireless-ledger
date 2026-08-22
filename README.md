# pi-memoryd

`pi-memoryd` is the single recall service for indexed Pi and Codex sessions. Production is one Go process linked to LanceDB through CGO. Macs only copy their untouched session JSONL files to R2; the VPS extracts, embeds, and indexes them.

```text
Mac JSONL -> R2 raw prefix -> pi-memoryd -> VPS llama.cpp -> R2 Lance table `turns`
```

The live source of truth defaults to `s3://<bucket>/session-recall-lance-payload-id-v3/`. Set `PI_MEMORYD_S3_PREFIX` to select a different prefix for rollback. The daemon does not copy the table into RAM or run a Python/HTTP storage sidecar.

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

`llama-embed` runs next to the daemon and is used only by the VPS worker:

```bash
docker compose up -d llama-embed
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
- `GET /v1/raw/status`

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

## Raw session ingestion

The raw object layout is deliberately the only contract:

```text
<host>/pi/<any subdirectories>/<file>.jsonl
<host>/codex/<any subdirectories>/<file>.jsonl
```

Objects are never modified or deleted by the daemon. It polls the raw prefix, downloads changed objects, extracts user/assistant messages, embeds them through the local llama.cpp container, and merge-inserts stable message IDs. An object is recorded as indexed only after every batch is persisted. On a crash, the object is retried; already persisted messages are harmlessly skipped or upserted. Failed objects retry with bounded exponential backoff. Parser-version changes automatically reprocess the derived data.

The durable registry is `/data/raw_registry.json`. Inspect progress with:

```bash
curl -sS http://100.127.82.49:8090/v1/raw/status
```

On a Mac, copy `config/raw-upload.env.example` to `~/.config/tireless-ledger/raw-upload.env`, insert the R2 credentials, and run:

```bash
scripts/upload-raw-sessions.sh
make install-raw-uploader
```

The launch agent repeats the small `tireless-upload` sync every minute. It compares object sizes and uploads only new or growing JSONL files. There is no local parsing, formatting, deduplication, or embedding.

The VPS uses the same uploader under a user systemd timer:

```bash
make install-upload-timer
```

It uploads the VPS's untouched session trees every minute; extraction and embeddings still run only in `pi-memoryd` and `llama-embed`.

## Production environment

| Variable | Purpose |
|---|---|
| `PI_MEMORYD_STORAGE_URL` | Lance database URI, composed as `s3://<bucket>/<PI_MEMORYD_S3_PREFIX>` by Docker Compose |
| `PI_MEMORYD_S3_PREFIX` | Optional Compose prefix override; defaults to `session-recall-lance-payload-id-v3` |
| `PI_MEMORYD_VECTOR_NPROBES` | IVF partitions scanned per dense query; defaults to all 64 for exhaustive coverage |
| `PI_MEMORYD_S3_BUCKET` | Compose bucket interpolation |
| `PI_MEMORYD_S3_ENDPOINT` | R2 S3 endpoint; passed as Lance `aws_endpoint` |
| `PI_MEMORYD_KEY_ID` | R2 access key ID |
| `PI_MEMORYD_APPLICATION_KEY` | R2 secret key |
| `AWS_REGION` | R2 region (`auto` is supported) |
| `PI_MEMORYD_DIMENSIONS` | Vector size, default `384` |
| `PI_MEMORYD_STATE` | Local dedup registry path |
| `PI_MEMORYD_RAW_URL` | Raw session prefix, e.g. `s3://bucket/session-recall-raw-v1` |
| `PI_MEMORYD_RAW_POLL_INTERVAL` | Raw object scan interval, default `30s` |
| `PI_MEMORYD_RAW_MAX_OBJECT_BYTES` | Per-object safety limit, default `128 MiB` |
| `PI_MEMORYD_EMBED_URL` | VPS embedding service, default `http://llama-embed:8091` |

Encrypted values live in `secrets/pi-memoryd.sops.env`; `.env` is generated locally and must not be committed.
