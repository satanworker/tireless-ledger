# pi-memoryd

`pi-memoryd` is the single recall service for indexed Pi, Codex, OMP, Claude Code, Grok, and Grok Bot sessions and Telegram chats. Production is one Go process linked to LanceDB through CGO. Uploaders only copy untouched JSONL files to R2; the VPS extracts, embeds, and indexes them.

The same code runs as separate deployments (personal and Valantis), each with its own VPS, R2 bucket, and set of uploaders. Nothing in the code distinguishes them.

```text
Mac launchd uploader -------\
                             > R2 raw prefix -> VPS pi-memoryd + llama.cpp -> R2 Lance
VPS systemd uploader -------/                         `chunks` + `messages`
```

There are two installations of the same small `tireless-upload` binary, plus
the central ingester:

| Component | Runs on | Reads | Responsibility |
|---|---|---|---|
| Mac uploader (`launchd`) | Each Mac | That Mac's Pi, Codex, OMP, Claude Code, and Grok session directories | Copies untouched JSONL to R2 every minute |
| Server uploader (`systemd --user`) | VPS | The VPS's Pi, Codex, OMP, Claude Code, and Grok session directories, plus Grok Bot chat exports when `TIRELESS_GROKBOT_SESSIONS` is set | Copies untouched JSONL to R2 every minute |
| Central ingester (`pi-memoryd` in Docker) | VPS | All raw JSONL already uploaded to R2 | Parses, embeds, and writes the shared Lance recall index |

Uploaders never parse or index sessions. `pi-memoryd` does not discover local
session directories; it only ingests objects that an uploader has copied to R2.

## Nix packages

This repository is the single source for the clients. Its flake exports `packages.<system>.tireless` (the recall CLI used by the omp `tireless_recall` tools and grok-box) and `packages.<system>.tireless-upload` for Linux and macOS. Every machine takes them from the `tireless-ledger` flake input; there are no copies in host repos. Roll out a client change on every machine with `nix flake update tireless-ledger` and a rebuild. `pi-memoryd` itself links the patched Lance library, so it ships as the `release-linux` binary. When `go.mod`/`go.sum` change, update `vendorHash` in `flake.nix`.

The live derived recall store defaults to `s3://<bucket>/session-recall-lance-generic-v2/`. Untouched raw JSONL in R2 remains the authoritative source. Set `PI_MEMORYD_S3_PREFIX` to select a different derived prefix for rollback. The daemon does not copy the table into RAM or run a Python/HTTP storage sidecar.

## Record schema

Every source writes the same generic columns, so a new source needs a parser and no schema change. `messages` holds one row per message; `chunks` holds the embedded text plus the same columns for filtering.

| Column | Agent session | Telegram |
|---|---|---|
| `id` | `<host>/<source>/<session>/<turn>` (`…/<session>/<kind>/<turn>` for titles, todos, tasks) | `<host>/telegram/<container_id>/<msg_id>` |
| `source` | `pi`, `codex`, `omp`, `claude`, `grok`, `grokbot` | `telegram` |
| `host` | uploading machine (first raw key segment) | exporting account/machine |
| `container_id`, `container_kind` | session id, `agent_session` | chat id (`<chat>:<topic>` in forum topics), `dm`/`group`/`topic`/`channel` |
| `container_title` | empty | chat title |
| `project` | repo from the session `cwd` | empty |
| `author_id`, `author_name` | `user`/`assistant`/…, empty | sender id, display name |
| `kind` | `message`, `title`, `todo`, `task` | `message` |
| `reply_to` | empty | id of the replied-to message |
| `timestamp`, `text`, `content_hash` | | |
| `attrs` | empty | JSON: `fwd_from`, `edit_date`, `media` |

Chunks add `message_id`, `chunk_index`, `chunk_count`, and `vector`; chunk ids are `<message_id>#<index>`. IDs are readable and start with the host that ingested them, so a host can re-send, overwrite, or delete (`--delete-host`) its own rows.

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

Production Linux/ARM64 builds use Docker. The image pins a `lancedb-go` commit, advances its Rust engine to stable LanceDB 0.37.1 / Lance 10, applies the checked-in 256 MiB index / 64 MiB metadata session-cache overlay and bounded disk range-cache overlay, and links the resulting `liblancedb_go.a` (AWS support) into the Go 1.24 daemon.

The patched Rust library is built once and published as a GitHub release
asset; the Dockerfile `lance-lib` stage downloads it by URL and SHA-256, so
normal builds only compile and link Go. Rebuild it only when
`LANCEDB_GO_COMMIT` or `docker/lancedb-go-*` change:

```bash
make lance-lib   # slow Rust build -> bin/lance/liblancedb_go.a + sha256
# upload as a new release asset, then update the lance-lib stage URL/checksum
```

Release binaries for the home-satan Nix package (`pi-memoryd-linux-arm64`,
`tireless-upload-linux-arm64`) come from `make release-linux`. Before a fresh
library is published, link against the local copy with
`make release-linux LANCE_LIB_DIR=bin/lance`.

```bash
make secrets-decrypt
make docker-build
make up
```

On the VPS, install the user startup service once so Compose is retried only
after the Tailscale address is available:

```bash
make install-startup-service
```

The unit waits for `tailscale` before running `docker compose up`, avoiding a
boot race when the service ports are bound directly to the Tailscale address.

All Docker targets use the persistent `tireless-limited` BuildKit builder,
capped at 1 CPU, 3 GiB RAM, and 4 GiB including swap, and stopped afterwards.
Its named volume keeps Go module and build caches across stops and reboots;
losing it costs only a Go rebuild, not a Rust one.

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

Production runs IVF-Flat with 512 partitions and searches 128 of them
(`PI_MEMORYD_VECTOR_NPROBES=128`). At 326,000 chunks a flat scan decodes all
501 MB of vectors per query; 512/128 kept recall@5 at 0.99 against numpy brute
force (30 queries) while reading a quarter of the index. 16 of 512 probes
dropped recall@5 to 0.53. `PI_MEMORYD_EXACT_VECTOR_SEARCH=true` does not
bypass an existing index, so drop the index for exact scans. The index is
derived and reversible:

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
- Vector (single-partition IVF-PQ), BM25, and combined hybrid search with LanceDB RRF.
- Search reads only the `chunks` table; container walking reads only `messages`.
- Container walk filters in Lance, applies the `(after_ts, after_id)` cursor, and returns `(timestamp, id)` order.
- Chunks use vector, `text` FTS, and scalar `id` indexes; messages use scalar `container_id` and `id` indexes.
- One-row scan and dummy FTS warmup during startup.
- Every immutable read (data, index, and deletion files) goes through a
  persistent local block cache: aligned 256 KiB blocks plus a per-object
  metadata record, so any range inside an already-fetched region is served
  from disk. The first read of a table copies its index files into the cache
  in the background, then its data files while indexes plus data fit in half
  the cap; larger tables fetch result rows on demand. Compose enables a 2 GiB
  cap at `/data/lance-cache` (home-satan uses 6 GiB); set
  `PI_MEMORYD_LANCE_CACHE_BYTES=0` to disable it or provide another byte cap.
  Manifests, listings, conditional reads, and writes always go to R2, and
  mutations invalidate any cached blocks for their object.
- Lance's in-memory index cache is 1 GiB, enough to keep the vector index resident.
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
- `POST /v1/memory/ingest` — `{records:[{id, vector, text, metadata:{source, host, container_id, timestamp, …}}]}`; one chunk `<id>#0` per record
- `POST /v1/memory/query` — filters `source`, `host`, `container_id`, `container_kind`, `project`, `author_id`, `kind`
- `GET /v1/memory/container?container_id=...&host=&source=&after_ts=...&after_id=...[&include=todos|tasks|titles|state]`
- `GET /v1/raw/status`

Example hybrid query:

```json
{
  "query_vector": [0.1, 0.2],
  "query_text": "wallet activation",
  "source": "telegram",
  "container_kind": "group",
  "limit": 5
}
```

Results are collapsed per message: `id` is the message id (for Telegram, the first message of the matched window), `text` the matched chunk, and `metadata.matched_chunk_id` the chunk.

Production uses 384-dimensional `BAAI/bge-small-en-v1.5` vectors. Query prefixes belong only on query embeddings; do not mix embeddings from different model/runtime configurations in the same table.

## Raw session ingestion

The raw object layout is deliberately the only contract:

```text
<host>/pi/<any subdirectories>/<file>.jsonl
<host>/codex/<any subdirectories>/<file>.jsonl
<host>/omp/<any subdirectories>/<file>.jsonl
<host>/claude/<any subdirectories>/<file>.jsonl
<host>/grok/<url-encoded cwd>/<session>/updates.jsonl
<host>/grokbot/<slug>-<agentid8>.jsonl
```

The `claude` tree mirrors `~/.claude/projects` (override with
`TIRELESS_CLAUDE_SESSIONS`). The file stem is the session ID, so each subagent
transcript under `<session>/subagents/agent-<id>.jsonl` is its own
`agent-<id>` session. Meta/caveat records, thinking, tool calls, and ordinary
tool results are skipped; `ai-title`/`custom-title`/`summary` records become one
title row per distinct title, `TodoWrite` inputs become todo rows, and
synchronous `Agent`/`Task` results become task rows.

The `grok` tree mirrors `~/.grok/sessions` (override with
`TIRELESS_GROK_SESSIONS`), uploading only each session's `updates.jsonl`. Grok
is also what T3 Code runs for Grok threads; T3's Claude threads already land in
`~/.claude/projects`. User and agent message chunks become messages; thoughts,
tool calls, hooks, and T3's injected `<runtime_info>` prompt suffix are skipped.

The `grokbot` tree holds Grok Bot chat histories exported on `grok-box` and
rsynced to the uploader host. It is off unless `--grokbot` /
`TIRELESS_GROKBOT_SESSIONS` names a directory; every `*.jsonl` in it is
uploaded. Each append-only file is one bot in the Pi session format: a
`{"type":"session","id":"<agent_id>","cwd":"/grok-box/<slug>"}` header, then
`message` lines with millisecond timestamps. The agent ID is the session ID and
the slug is the project. Records keep the `grokbot` harness, so their IDs never
collide with Pi or OMP rows.

The `telegram` tree holds exports written by `cmd/tireless-telegram` (Telethon; run with the `telethon-python` wrapper). It is off unless `tireless-upload --telegram <dir>` / `TIRELESS_TELEGRAM_DIR` names the export directory, uploaded as `<host>/telegram/<chat_id>/<yyyy-mm>.jsonl`. Each line is one message version:

```json
{"id":"812","chat_id":"-1001234567890","chat_type":"group","chat_title":"Valantis Core","topic_id":"","date":1791311374,"edit_date":0,"sender_id":"55501","sender_name":"Alice","reply_to":"809","fwd_from":"","media":"","text":"pushed v0.3 to staging"}
```

An edit appends a line with the same `id`; the last line wins, and an edit to empty text drops the message. The exporter keeps a per-chat cursor and exports every dialog (`--chats all`, the personal setup) or only the chat ids listed in a file (`--chats <file>`, the company setup). Edits and deletes of already-exported messages are not re-fetched.

Objects are never modified or deleted by the daemon. It polls the raw prefix, downloads changed objects, and extracts user/assistant messages plus important session state: titles, todo snapshots, and task/subagent tool results. Every logical message is stored once, in full, as a canonical `messages` row; titles, todos, and tasks keep `kind` `title`, `todo`, or `task` so normal recall can find them without reading a local session file. The daemon uses llama.cpp's tokenizer to split each searchable text into overlapping windows of at most 384 tokens (64-token overlap), embeds every chunk, and links each chunk to its message id. Short Telegram messages embed poorly one by one, so consecutive DM/group/topic messages less than 10 minutes apart share one window (`Name: text` lines, at most 1,200 characters) whose chunks hang off the window's first message; channel posts stand alone. A new or edited message re-embeds only its window. Search runs over chunks and collapses hits by message. Container walking prefers the untouched raw JSONL archive and falls back to the derived `messages` table; `include=todos`, `include=tasks`, `include=titles`, or `include=state` reads those exact records from raw when available, so reconstruction is not limited by chunk overlap, embedding truncation, or quantized search indexes.

An object is recorded as indexed only after every derived row is persisted. On a crash, the object is retried; stable message and chunk IDs make replay harmless. Growing files are coalesced for five minutes before their next derived write, reducing tiny Lance fragments without delaying new files. Failed objects retry with bounded exponential backoff. A parser version change re-parses every object, but the dedup state (message id → content hash) skips messages whose text is unchanged, so nothing is re-embedded unless it changed. The raw JSONL remains the source of truth for clean reindexing.

## Generic-schema migration

`make migrate-generic` copies the `token-chunks-v4` `messages` and `chunks` tables (default prefix `session-recall-lance-token-chunks-v4`, override with `MIGRATE_FROM_PREFIX`) into the generic tables at `PI_MEMORYD_S3_PREFIX` without downloading raw files or re-embedding. The v4 `file_path` maps to the id the raw parser emits: `<host>/<harness>/<session>/<turn>` for messages (the v4 form `<session>/message/<turn>` loses its `message` segment) and `<host>/<harness>/<session>/<kind>/<turn>` for titles, todos and tasks. Where v4 stored one turn in both message forms, the `/message/` row is copied and its plain twin skipped. Chunks keep their vectors as `<id>#<chunk_index>`, and every copied message is written to the dedup state, so the raw re-scan after the cutover embeds only messages that are new or whose derivation changed. It pins both source table versions, copies 1,024-row batches with merge-insert, and saves `/data/generic_migration.json` after every batch; re-running resumes and is safe after interruption. It fails if two non-twin v4 rows map to one id.

Rollout: stop the daemon, run `make migrate-generic`, then `--create-vector-index` and start the new image (the Compose default prefix is `session-recall-lance-generic-v2`). The v4 prefix stays untouched; rollback is the previous image with `PI_MEMORYD_S3_PREFIX=session-recall-lance-token-chunks-v4`.

The durable registry is `/data/raw_registry.json`. Inspect progress with:

```bash
curl -sS http://100.127.82.49:8090/v1/raw/status
```

On a Mac, copy `config/raw-upload.env.example` to `~/.config/tireless-ledger/raw-upload.env`, insert the R2 credentials, and run:

```bash
scripts/upload-raw-sessions.sh
make install-mac-uploader
```

The Mac launchd agent repeats the small `tireless-upload` sync every minute. It
compares object sizes and uploads only new or growing JSONL files. There is no
local parsing, formatting, deduplication, or embedding.

The VPS must separately install the same uploader under a user systemd timer:

```bash
make install-server-uploader
```

That target rebuilds and installs the uploader binary before refreshing the
timer, so rerun it after pulling uploader changes. It uploads the VPS's
untouched session trees every minute; extraction and embeddings still run only
in `pi-memoryd` and `llama-embed`.

The older target names remain as compatibility aliases:
`install-raw-uploader` means Mac, and `install-upload-timer` means VPS/server.

## Production environment

| Variable | Purpose |
|---|---|
| `PI_MEMORYD_STORAGE_URL` | Lance database URI, composed as `s3://<bucket>/<PI_MEMORYD_S3_PREFIX>` by Docker Compose |
| `PI_MEMORYD_S3_PREFIX` | Optional Compose prefix override; defaults to `session-recall-lance-generic-v2` |
| `PI_MEMORYD_VECTOR_NPROBES` | IVF partitions searched per vector query (of 512); default 64, production 128 |
| `PI_MEMORYD_EXACT_VECTOR_SEARCH` | Intended to bypass IVF for an exact scan, but Lance ignores it while an index exists; code default `true`, production `false` |
| `PI_MEMORYD_MIGRATION_BATCH` | Rows per resumable `--migrate-from` copy batch; default `1024` |
| `PI_MEMORYD_MAX_CONCURRENT_QUERIES` | Maximum native Lance searches in flight; default `2` |
| `PI_MEMORYD_QUERY_TIMEOUT` | HTTP-side query deadline; default `30s` |
| `PI_MEMORYD_LANCE_CACHE_BYTES` | Persistent local block-cache cap for immutable Lance data/index files; Compose defaults to `2147483648` (2 GiB), and `0` disables it. Tables up to half the cap are warmed fully |
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
