---
id: single-process-go-cgo-lance-drop-python-sidecar
title: Single-process Go CGO Lance (drop Python sidecar)
emoji: ⚡
status: completed
created: 2026-08-21T11:11:40.665Z
updated: 2026-08-21T18:07:32.000Z
---
## Checklist
- [x] Wire lance.go CGO store: connect R2, merge-insert, hybrid, walk, compact-on-16
- [x] Patch lancedb-go FFI Session cache (256MiB index / 64MiB meta) in Docker so VPS does not OOM
- [x] Docker CGO build: rust liblancedb_go.a + Go 1.24, drop lance-writer from compose
- [x] Mac tests stay CGO_ENABLED=0 + memory://; no Python sidecar in product path
- [x] Deploy to home-satan, time Mac walk/hybrid vs previous sidecar hop

## Validation (2026-08-21)

- `CGO_ENABLED=0 go test ./...` and `go vet ./...` pass with Go 1.24.
- The Linux/arm64 CGO image builds, starts, and passes `go test ./...` inside its native builder.
- A localhost-only native canary connected to the live R2 `turns` table and logged the patched 256 MiB index / 64 MiB metadata cache sizes.
- Canary walk output exactly matched the sidecar output; hybrid returned the same top-five IDs; cursor ordering was strict.
- The first native build exposed a performance mismatch: `lancedb-go` main pinned Rust LanceDB 0.24 / Lance 1 while the sidecar used LanceDB 0.37.1 / Lance 10. The overlay now advances the Rust bridge to stable LanceDB 0.37.1 / Lance 10 and adapts its changed batch, schema-evolution, and index-stat APIs.
- In an alternating ten-sample benchmark after the upgrade: hybrid median was 0.334s native vs 0.403s sidecar; vector median 0.338s vs 0.345s; session walk median 0.199s vs 1.164s. BM25-only improved from 0.443s to 0.266s native but remains slower than the sidecar's 0.209s median.
- Across three fresh-process samples, the first post-ready hybrid was 1.122s native vs 0.844s Python, but median boot-to-ready was 3.205s native vs 7.456s Python; total start-to-first-hybrid was about 4.35s vs 8.37s. Cold walk itself was 0.778s native vs 1.342s Python, with about 4.24s vs 8.55s total start-to-result.
- Walk output remained byte-identical and hybrid returned the same top-five IDs. The warm upgraded canary used about 21 MiB RSS; its cache limits remained logged at 256 MiB / 64 MiB.
- A single named Lance 10 smoke record persisted to R2 (`accepted: 1`), immediately hybrid-queried and walked back, and was skipped on duplicate ingest. No bulk sync was started.
- Production was cut over at 2026-08-21 13:48 UTC to image `sha256:6ecc8f5356e5d52684024d07adb6491e5363d98b5ca711c9b4605f61b7fb0cce`. Readiness passed after the native Lance warmup, the existing Codex session walk passed, and the Python `lance-writer` container was stopped and removed. The previous daemon image remains available as `pi-memoryd:pre-cgo-20260821` for rollback.
- The explicitly requested VPS Codex-history follow-on scan found 350 JSONL files / 550 MiB and extracted 15,426 user/assistant turns in 5.19s. The client now bounds embedding input to 800 characters (while storing full text) to stay inside BGE-small's 512-token window. A 100-turn write trial completed in 10.24s; the full run started at 13:52 UTC and reached 1,700 processed / 1,600 newly accepted / 100 skipped in 3m38s (~7.8 turns/s end to end, including compaction and concurrent measurement).
- During that active write load, five-request medians were 0.683s for BM25 and 0.337s for session walk. The first BM25 request was cold at 1.837s. `pi-memoryd` used about 50 MiB RSS and the two-thread embedder about 122 MiB RSS / 195% CPU.
- The original foreground extractor ended when its tool session was collected after 3,800 unique turns; production remained healthy. Ingest resumed from parsed-turn offset 3,800 at 14:06 UTC as durable user unit `tireless-codex-ingest.service`; its first resumed 100-row batch persisted successfully.
- The resumed extraction completed with 11,724 newly accepted rows, zero skipped, in 1,739.43s. Together with the earlier rows, the live table contains 15,524 extracted Codex-history turns. Automatic fragment compaction ran 11 times without a failure.
- Immediately after bulk ingest, warm medians had regressed with corpus size and stale index tails: hybrid 1.982s, BM25 2.197s, vector 0.981s, and walk 0.511s. Ordinary compaction had not folded newly written rows into the existing FTS/B-tree indexes.
- A one-shot `--optimize` path now compacts, runs Lance `OptimizeIndex`, and prunes old versions. It reduced the table from 15 fragments to 3; the subsequent IVF build/maintenance reduced it to 1. Future automatic compactions refresh the remaining FTS/scalar indexes so the stale-tail regression does not recur.
- Historical state: the 15,524-row table used a reversible 64-partition IVF-Flat vector index and scanned all 64 partitions for complete vector coverage. At roughly 116,000 chunks this became slower than exact flat search over R2, so production now defaults to `PI_MEMORYD_EXACT_VECTOR_SEARCH=true` and the derived IVF index is absent. The full vectors remain in Lance and IVF can be recreated if later corpus growth justifies it.
- Final 64-probe ten-sample warm medians were BM25 0.211s, vector 0.211s, hybrid 0.225s, and 50-row session walk 0.221s. Relative to post-ingest/pre-maintenance, that is about 90%, 78%, 89%, and 57% lower median latency respectively. Query embedding itself was 0.016s median. The 64-probe vector/hybrid maxima in this run were 0.319s / 0.294s.
- The final image is `sha256:4ec73dba25cfcc294cefdf13be154775df21fe421ad223bf579902fd980c2009`; exact-scan rollback remains `pi-memoryd:pre-ann-20260821` (`sha256:f218d104fcc9ec1acdeada8fb0d88d658983a74a8cfc0fc0869c8cedc6c84e95`). Both services are healthy. Steady-state RSS was about 48 MiB for `pi-memoryd` and 40 MiB for the embedder, with 3.7 GiB host memory available.

## Decisions

- Product is session recall: fuzzy + keyword search, then cursor-walk the indexed thread over HTTP. Resume/replay of original jsonl is out of scope.
- Source of truth is R2 Lance table `turns` at prefix `s3://tireless-ledger/session-recall-lance/`. Do not make local disk the live table. Do not RAM-copy the whole turns table.
- Hybrid (vector + BM25) is required; production currently uses exact vector scans rather than ANN. Walk is filter scan + cursor (`after_ts`/`after_id`), order `(timestamp, id)`. Indexed windows only (user+assistant; not tools/thinking/jsonl).
- One service for recall: `pi-memoryd` only. No Python. No `lance-writer` sidecar. Embeddings stay a separate optional process (`llama-embed` / llama.cpp `:8091`) — not part of this merge.
- Language: Go + CGO, not a Rust rewrite of the daemon. Reason: `pi-memoryd` already owns ingest/query/walk/dedup/HTTP. `lancedb-go` wraps the same Rust engine (`liblancedb_go.a`). A full Rust rewrite of the daemon is extra work for no engine gain.
- Mac tests stay `CGO_ENABLED=0` + `memory://`. Do not require native Lance on the Mac for unit tests.
- Same 384-d model everywhere: `BAAI/bge-small-en-v1.5`. Query prefix only on queries. Never mix Mac Metal and VPS CPU on this R2 prefix. Current ingest embedder for this prefix is VPS llama `:8091`.
- Write-through: HTTP 200 only after persist. Compact when ≥16 fragments, not after every 32-row flush. `accepted` = persist.
- Lance Session cache must be capped: 256 MiB index + 64 MiB metadata. Defaults (~6 GiB + 1 GiB) would OOM the 7.5 GiB VPS. Official `lancedb-go` FFI (`simple_lancedb_connect_with_options`) currently only passes storage_options JSON — no Session. Patch `lancedb-go` `rust/src/connection.rs` in the Docker build (overlay) to create `lance::session::Session` with those sizes and pass it to `connect().session(...)`. Do not vendor a fork in git unless the overlay is tiny and checked in as a patch file.
- SDK pin: `github.com/lancedb/lancedb-go` **main** (not tagged v0.1.2). v0.1.2 lacks FTS/hybrid. Needs Go 1.24 + Arrow v17. Native lib: build `liblancedb_go.a` with `--features aws` for R2. R2 keys: `access_key_id`, `secret_access_key`, `region`, `aws_endpoint` (not `endpoint`), `virtual_hosted_style_request=false`.
- Store APIs to implement against Python sidecar behavior in `docker/lance/server.py`: merge_insert on `id`; hybrid via VectorQuery.WithFullText + RRF; BM25 via FullTextSearch; walk via Select/Query + SQL where + sort in Go if no order_by; compact via OptimizeWithAction Compact then Prune older_than 0; FTS on `forward_content`; btree on `session_id`; warmup 1-row scan + dummy FTS on open.
- Keep daemon HTTP API unchanged: `GET /-/ready`, `POST /v1/memory/ingest`, `POST /v1/memory/query`, `GET /v1/memory/session`. Drop `PI_MEMORYD_VECTOR_URL` / `start-vector` / OpenData yaml generation on the Lance path.
- SSH only `home-satan` (`-o IdentitiesOnly=yes -i ~/.ssh/home_satan`). Tailscale `:8090`. No fancy auth this run. Object-store keys only in project SOPS / `.env`. Don't check in a prebuilt `tireless` binary.
- Don't start bulk `tireless sync` on battery. Full history dump (~1610 jsonl / ~60k windows) is a later item, not this task's deploy smoke.
- Don't KeepAlive llama; don't Metal for background ticks.

## Remaining work (maps to checklist)

1. `lance.go` (`//go:build cgo`) + `lance_nocgo.go` (`//go:build !cgo`). Wire `server.lance` in `main.go`: open when storage is s3 (not memory://); `flushBatch`/`vectorSearch`/`sessionWalk` go through Lance; compact-on-16 after writes. SQL quote/filter/cursor helpers with tests that run without CGO.
2. Docker overlay patch for Session cache 256/64. Confirm connect uses it (log cache sizes at boot).
3. Dockerfile: rust stage build native lib for linux/arm64; Go 1.24 CGO link `-lm -ldl -lpthread`. Compose: delete `lance-writer` and `depends_on`; drop `PI_MEMORYD_VECTOR_URL`. Keep `llama-embed` profile. Makefile `test` stays `CGO_ENABLED=0`.
4. Mac `go test ./...` with CGO off must still pass (`memory://` HTTP tests). Product path must not import/run Python sidecar.
5. rsync to home-satan, `compose up --build` pi-memoryd only, smoke ingest+hybrid+walk on existing test sids (pi `019f6fe1-00a0-7214-9ce3-0e07d8852430`, Codex `019e92ea-9064-75f0-9f21-ce54439ffa4b`). Time Mac walk/hybrid vs last sidecar numbers (walk ~1.4–1.7s Mac / ~1.2s sidecar HTTP; hybrid ~0.58s after warmup). Goal: walk <1s after boot warmup if the HTTP hop was the tax.

## Out of scope this task

- Full history re-sync (empty cursor, AC only, hours, VPS embed `-t 2`).
- IVF on `vector` after ~5–10k / full dump ~60k.
- Rust rewrite of pi-memoryd.
- Sub-200ms cold object-store (needs Enterprise/Foyer; rejected whole-table RAM).
- OpenData (abandoned). Python sidecar is spike only; delete from compose when Go path is live; files under `docker/lance/` can stay until after deploy smoke.

## Current live stack (before this change)

- VPS `home-satan` / `100.127.82.49`: daemon `:8090`, llama `:8091`, Python `lance-writer` internal `:8080`.
- Table already has two test threads ingested; R2 compacted (~7 objects). Cursor emptied for Lance (`cursor.json.pre-lance.bak`).
