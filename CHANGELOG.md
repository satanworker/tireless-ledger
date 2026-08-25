# Changelog

## Unreleased

### Exact vector serving (2026-08-25)

- Vector and hybrid queries now default to Lance's exhaustive flat scan via
  `PI_MEMORYD_EXACT_VECTOR_SEARCH=true`. At roughly 116,000 searchable chunks,
  this was faster over R2 than probing every partition of IVF_FLAT and provides
  exact nearest-neighbor recall.
- Production's derived 170 MiB IVF index was dropped; source vectors, indexed
  text, canonical messages, and raw session files were not removed. The index
  remains recreatable with `--create-vector-index` if future corpus growth and
  measured recall justify approximate search.
- The first post-restart vector query fell from roughly 11 seconds to 2.04
  seconds. Idle production measured 1.11–1.48 seconds for vector and 1.15–1.55
  seconds for hybrid, with occasional 5–6 second R2 outliers still observed.

### Decisions (2026-08-18) — session recall

**Goal**
- Recall only. New pi session asks the server and pulls relevant context from all sessions (any harness, host, folder).
- Not building session resume/replay (`pi`/`codex` --resume from original jsonl). Deferred.

**Store**
- Machines are disposable. Recall source of truth is OpenData Vector on S3/B2, not the Mac.
- OpenData Vector is the engine: ANN (fuzzy) + BM25 on `Text` fields. Hybrid = two queries, client-side merge. Docs: https://www.opendata.dev/docs/vector
- Every Vector record still requires a dense vector (collection dims, default 384). No BM25-only insert.
- One OpenData Vector writer per S3 folder (VPS). Clients HTTP only. Two writers on the same folder is invalid.
- Same embed model and dims for ingest and query.

**Document shape**
- Not one Vector row per whole session file (files are 16–169 MB). Split into documents (turn or chunk).
- First ingest slice: user + assistant turns, with indexed `session_id` (plus host/harness) so dig-deeper is filter/get-by-id over HTTP.
- Summaries-only rejected (cannot dig into the thread).
- Tool results optional later. token_count / reasoning not in the first index.
- Unlimited recall is not one ANN query (ANN is top-k). Round 2+ walks neighbors in that session.

**Before v0 (context, not the target)**
- This repo is `pi-memoryd` (tireless-ledger): ingest/query HTTP funnel. Does not embed. Query is vector + scope + project_name only.
- Queue 10000 non-blocking (full drops). Flush 500 or 300s. Dedup file_path→file_hash local. ID max 64 bytes.
- B2 is not capped at 800 MB; that number was an index-size estimate for user+assistant+tools.

**Measured this Mac (2026-08-18)**
- pi `~/.pi/agent/sessions/`: ~709 jsonl, ~357 MB, ~5k user / 33k assistant / 37k toolResult
- Codex `~/.codex/sessions/`: ~895 jsonl, ~932 MB; one file 169 MB; function_call_output ~345 MB
- Combined JSONL ~1.3 GB, ~390k events. `logs_2.sqlite` ~495 MB is not the transcript.

### Implemented (2026-08-18) — tireless-mac v0

**Daemon (`main.go`)**
- `ingest` metadata now accepts optional `session_id`, `host`, `harness`, `role` — stored as indexed String fields.
- `forward_content` schema type changed from String to Text (BM25).
- Query accepts `query_vector` and/or `query_text`; optional filters `scope`, `project_name`, `session_id`, `host`, `harness`.
- Both vector and text present → two OpenData searches, RRF merge (k=60) in the daemon. Vector-only or BM25-only also work.
- Filters are optional (empty filter if none supplied).
- OpenData Vector stores one database in one folder in the B2 bucket (today that folder is `vector-index/`). That folder already has a schema: `forward_content` is a plain String. Word search (BM25) only works if that field is type Text. You cannot change a field's type in an existing folder. Point the daemon at a **new empty folder** (example: `session-recall/`) via `PI_MEMORYD_S3_PREFIX`, start Vector once, and ingest there. If `vector-index/` was never written to, you can keep using it — there is no old schema yet. Do not run two Vector writers against the same folder.

**Mac indexer (`mac/`)**
- `parse.py` extracts user+assistant turns from `~/.pi/agent/sessions` and `~/.codex/sessions`.
- Skips toolResult, Codex `event_msg` dupes, AGENTS.md dumps, thinking parts, short text.
- Model: BAAI/bge-small-en-v1.5 (384-d), query prefix on query only.
- `cli.py` commands: `parse`, `ingest`, `query`.

**Tests**
- `go test` (RRF + filter); `python3 mac/test_parse.py`.

**Still not done**: no embedder on server, no jsonl archive, no auth, no tool ingest, no resume.

**Deferred**
- jsonl archive next to the index (only needed for resume or reindex-from-source)
- indexer/embedder — v0 landed; polish later
- BM25 + hybrid + session_id query — v0 landed; polish later
- code_memory / codebase indexing
- auth on :8090 (none today; do not expose publicly)

### Implemented (2026-08-18) — local smoke

- `memory://` storage: daemon uses an in-RAM index (no B2, no OpenData binary). For Mac testing only.
- GET /v1/memory/session?session_id=… lists turns in that session (time order in RAM; BM25+filter against OpenData).
- mac/cli.py: ingest streams in batches of 100; `session` command added.
- `make recall-local` starts the RAM daemon on :8090. `make test` runs go tests + parse self-check.
