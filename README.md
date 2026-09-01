# pi-memoryd

`pi-memoryd` is the single recall service for indexed Pi, Codex, and OMP sessions. Production is one Go process linked to LanceDB through CGO. Macs only copy their untouched session JSONL files to R2; the VPS extracts, embeds, and indexes them.

```text
Mac JSONL -> R2 raw prefix -> pi-memoryd -> VPS llama.cpp -> R2 Lance `chunks` + `messages`
```

The live derived recall store defaults to `s3://<bucket>/session-recall-lance-token-chunks-v4/`. Untouched raw JSONL in R2 remains the authoritative source. Set `PI_MEMORYD_S3_PREFIX` to select a different derived prefix for rollback. The daemon does not copy the table into RAM or run a Python/HTTP storage sidecar.

## Current production state

Production was cut over to split-table reads on 2026-08-25. Search reads
`chunks`, session reconstruction reads `messages`, and dual-write was disabled
after validation. Normal production does not open or write the retired legacy
`turns` table.

The cutover migration did not re-embed or truncate data. It copied the existing
rows and vectors from a pinned Lance snapshot, then compared source and
destination ID sets. Snapshot version 3828 contained 55,774 messages and
115,490 chunks (171,264 rows); both destination tables had zero missing IDs.
A later live audit contained 171,517 rows, 171,517 unique IDs, and zero
duplicates. A representative complete session response was byte-identical on
legacy and split reads. Original JSONL objects remain in R2 independently of
all derived Lance tables.

One known raw object is stored but deliberately not indexed:
`mbp14/codex/2026/02/27/rollout-2026-02-27T00-36-04-019c9c4f-3d7a-7451-bceb-52a1be460c9f.jsonl`
is 168,898,859 bytes, above the configured 134,217,728-byte safety limit. Its
original R2 object is intact; increasing the limit and replaying it is a future
choice, not data recovery.

## Builds

Production Linux/ARM64 builds use Docker. The image pins `lancedb-go` main, advances its Rust engine to stable LanceDB 0.37.1 / Lance 10, builds `liblancedb_go.a` with AWS support, applies the checked-in 256 MiB index / 64 MiB metadata session-cache overlay, and links it into the Go 1.24 daemon.

```bash
make secrets-decrypt
make docker-build
make up
```

`make docker-build` always uses the persistent `tireless-limited` BuildKit
builder. Its named Docker volume retains the Cargo registry, compiled Lance
target, Go modules, and Go build cache across builder stops and VPS reboots.
The builder is capped at 1 CPU, 3 GiB RAM, and 4 GiB including swap, then
stopped after loading `pi-memoryd:local` into Docker. Normal deployment uses
that saved image and does not rebuild it; run `make docker-build` only after
source or dependency changes. Do not prune the builder cache volume unless a
deliberate cold rebuild is acceptable.

After a bulk ingest, run one offline maintenance pass to compact the final
fragment tail and fold newly written rows into the existing FTS and scalar
indexes:

```bash
docker compose run --rm --no-deps pi-memoryd --optimize
```

On the VPS this is automated by a persistent user timer. It checks every four
hours (at `00,04,08,12,16,20:30` UTC, with up to 15 minutes of randomized
delay) and catches up after downtime. The check is cheap and skips maintenance
unless an active table has more than 20 fragments. Install or refresh it with:

```bash
make install-optimize-timer
```

The timer uses a non-blocking runtime lock, so delayed timer invocations cannot
overlap an optimization already in progress.

Exact flat vector search is the production default. An IVF-Flat index is an
optional, reversible mode for a materially larger corpus, but enable it only
after measuring representative recall and latency with fewer than all 64
partitions. Searching all partitions was slower than a flat scan over R2 at
116,000 rows.

```sh
docker compose run --rm --no-deps pi-memoryd --create-vector-index
# Then set PI_MEMORYD_EXACT_VECTOR_SEARCH=false and tune PI_MEMORYD_VECTOR_NPROBES.
# Return to exhaustive vector scans and remove the derived index:
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

- Merge-insert on stable `id`; an ingest response is successful only after persistence.
- Exact vector, BM25, and combined hybrid search with LanceDB RRF. At the current corpus size, exact flat vector scans outperform all-partition IVF_FLAT queries over R2.
- Search reads only the `chunks` table; session walking reads only `messages`.
- Session walk filters in Lance, applies the `(after_ts, after_id)` cursor, and returns `(timestamp, id)` order.
- Searchable chunks use FTS and scalar `id` indexes. The production IVF vector index is absent while exact search is enabled; messages use scalar `session_id` and `id` indexes.
- One-row scan and dummy FTS warmup during startup.
- Concurrent native searches are bounded and timed out at the HTTP boundary; a timed-out CGO call keeps its slot until native work actually returns, preventing orphan-query pileups.
- Writes never run compaction or index refresh inline. Offline maintenance
  compacts fragments, refreshes indexes, and prunes obsolete versions after
  bulk ingestion and through the scheduled threshold-based optimizer.

Before exact mode, 64-probe IVF measured 5.94 seconds median for vector search,
6.28 seconds median for hybrid search, and about 11 seconds on the first vector
request from a fresh process. After exact mode was deployed, the first
post-restart vector request took 2.04 seconds; idle production samples were
1.11–1.48 seconds for vector and 1.15–1.55 seconds for hybrid. Repeated R2 scans
still showed occasional 5–6 second network/storage outliers, so these are
operational baselines rather than a latency guarantee.

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
<host>/omp/<any subdirectories>/<file>.jsonl
```

Objects are never modified or deleted by the daemon. It polls the raw prefix, downloads changed objects, and extracts user/assistant messages. Every logical message is stored once, in full, as a canonical `message` row. The daemon uses llama.cpp's tokenizer to split the same text into overlapping windows of at most 384 tokens (64-token overlap), embeds every `chunk` row, and links each chunk to its stable parent message ID. Search runs over chunks and collapses hits by parent. Session walking prefers the untouched raw JSONL archive and falls back to the derived `messages` table, so reconstruction is not limited by chunk overlap, embedding truncation, or quantized search indexes.

An object is recorded as indexed only after every derived row is persisted. On a crash, the object is retried; stable parent and chunk IDs make replay harmless. Growing files are coalesced for five minutes before their next derived write, reducing tiny Lance fragments without delaying new files. Failed objects retry with bounded exponential backoff. Parser/chunker-version changes automatically reprocess the derived data. The raw JSONL remains the source of truth for clean reindexing.

## Split-table migration

Existing `turns` rows can be copied into the split layout without downloading
raw files or recomputing embeddings. The rollout is deliberately reversible:

1. Run the new image with `PI_MEMORYD_DUAL_WRITE_SPLIT=true` and
   `PI_MEMORYD_SPLIT_TABLES=false`. Reads stay on `turns`, while new writes also
   land in `chunks` and `messages`.
2. Run `make migrate-split`. It pins one immutable source-table version, copies
   in 1,024-row batches with merge-insert on stable IDs, and saves
   `/data/split_migration.json` after every batch. Re-running the command resumes
   that exact snapshot and is safe after interruption. Before marking the
   checkpoint complete, an ID-only reconciliation copies any omissions and
   proves that no source IDs are missing from either destination.
3. Create/refresh the chunk indexes, validate the shadow service, then set
   `PI_MEMORYD_SPLIT_TABLES=true` while leaving dual-write enabled for the
   rollback observation window. Disable dual-write only after that window.

Keep the old `turns` table and previous image during the observation window.
After dual-write is disabled, that table becomes a historical snapshot and is
not a lossless rollback target for newer sessions. Untouched raw JSONL in R2 is
the authoritative recovery source. The current VPS still retains the old image
as `pi-memoryd:pre-split-20260825` (`sha256:0ce78d40f440...`) and the dormant
legacy table, but neither participates in normal runtime operation.

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
| `PI_MEMORYD_S3_PREFIX` | Optional Compose prefix override; defaults to `session-recall-lance-token-chunks-v4` |
| `PI_MEMORYD_VECTOR_NPROBES` | IVF partitions scanned when exact mode is disabled; defaults to all 64 |
| `PI_MEMORYD_EXACT_VECTOR_SEARCH` | Bypass IVF and scan all vectors exactly; defaults to `true` because this is faster over R2 at the current corpus size and guarantees 100% vector recall |
| `PI_MEMORYD_SPLIT_TABLES` | Read/write separate `chunks` and `messages` tables; production default `true` |
| `PI_MEMORYD_DUAL_WRITE_SPLIT` | Write both legacy and split layouts during migration/observation; production default `false` |
| `PI_MEMORYD_MIGRATION_BATCH` | Rows per resumable legacy-to-split copy batch; default `1024` |
| `PI_MEMORYD_MAX_CONCURRENT_QUERIES` | Maximum native Lance searches in flight; default `2` |
| `PI_MEMORYD_QUERY_TIMEOUT` | HTTP-side query deadline; default `30s` |
| `PI_MEMORYD_S3_BUCKET` | Compose bucket interpolation |
| `PI_MEMORYD_S3_ENDPOINT` | R2 S3 endpoint; passed as Lance `aws_endpoint` |
| `PI_MEMORYD_KEY_ID` | R2 access key ID |
| `PI_MEMORYD_APPLICATION_KEY` | R2 secret key |
| `AWS_REGION` | R2 region (`auto` is supported) |
| `PI_MEMORYD_DIMENSIONS` | Vector size, default `384` |
| `PI_MEMORYD_STATE` | Local dedup registry path |
| `PI_MEMORYD_RAW_URL` | Raw session prefix, e.g. `s3://bucket/session-recall-raw-v1` |
| `PI_MEMORYD_RAW_POLL_INTERVAL` | Raw object scan interval, default `30s` |
| `PI_MEMORYD_RAW_WRITE_INTERVAL` | Minimum rewrite interval for an already-indexed growing object, default `5m` |
| `PI_MEMORYD_RAW_MAX_OBJECT_BYTES` | Per-object safety limit, default `128 MiB` |
| `PI_MEMORYD_EMBED_URL` | VPS embedding service, default `http://llama-embed:8091` |

Encrypted values live in `secrets/pi-memoryd.sops.env`; `.env` is generated locally and must not be committed.
