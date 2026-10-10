# Cold search: fetch as little as possible from R2

Handoff from the Mac session of 2026-10-09. Run on home-satan in `~/Developer/tireless-ledger`.

## Goal

A hybrid search that starts **fully cold** must fetch as few bytes, requests and sequential round trips from R2 as possible, without losing recall. "Fully cold" means a fresh process and an empty local cache. That situation applies to a restart before warm-up, a table larger than half the cache cap, or a browser/WASM client with no cache.

Find the best configuration by measuring, not by estimating. Come back with numbers. **Do not deploy anything to production**: report the result and the rollout steps, and the user decides.

## Hard constraints

- Do not write to the production prefix `s3://<bucket>/session-recall-lance-token-chunks-v4/`. Do not touch the `pi-memoryd` system service or `/var/cache/pi-memoryd`.
- `pi-memoryd` runs `ensureIndexes` at startup (`lance.go:149-200`) and creates any index that is missing. That is a write. Any bench instance that points at production data must go through a read-only proxy that refuses writes. This was done on 2026-10-05/06, see "Prior tooling".
- Index experiments that need R2 (cold reads of a new index type) go in a **scratch prefix** in the same bucket, for example `bench-cold-20261009/`. Copy `chunks.lance` there and build the variant indexes there. Delete the scratch prefix when finished.
- No sudo. The box has 4 CPUs, ~7.7 GiB RAM (3–4 GiB free; the production daemon holds a 1 GiB index cache) and ~43 GB of free disk. Keep builders limited so the live daemon isn't starved.

## Current production state (verified 2026-10-08/09)

- `pi-memoryd` is pinned through `home-satan/modules/tireless-ledger.nix`. Its environment, from `/run/secrets/rendered/tireless-ledger.env`:
  - `PI_MEMORYD_EXACT_VECTOR_SEARCH=false`
  - `PI_MEMORYD_VECTOR_NPROBES=128`
  - `LANCE_DISK_CACHE_DIR=/var/cache/pi-memoryd`
  - `LANCE_DISK_CACHE_MAX_BYTES=6442450944`
- Tables live in `session-recall-lance-token-chunks-v4` and share one 16-column schema (`lance.go:124-140`).
  - `chunks`: indexes `vector_ivf_flat` (IVF_FLAT, 512 partitions, L2), `forward_content_fts` and `id_btree`.
  - `messages`: indexes `session_id_btree` and `id_btree`.
  - `chunks.parent_id` points to `messages.id`. This is set in code (`raw_ingest.go:569`), not enforced by Lance.
- Sizes from the Oct 6 session: `chunks` data 719 MB plus `_indices` 1.07 GB (the IVF_FLAT index alone is 537 MiB); `messages` 330 MB. Vectors are 384-dim f32 (bge-small-en-v1.5), 1,536 bytes each.
- The disk cache, from commit `592615d` (`docker/lancedb-go-disk-cache.rs`):
  - Caches aligned 256 KiB blocks, plus whole objects up to 8 MiB.
  - Manifests, listings and conditional reads always go to R2.
  - The first read of a table triggers a background warm-up (`warm_root`) that copies the indexes, plus the data when indexes and data fit in half the cap.
  - There is no switch to disable warm-up other than leaving `LANCE_DISK_CACHE_DIR` unset, which disables the cache entirely.
  - The current journal shows both tables warmed with data (`chunks` 508 MB in 6.4 s).
- Query shape (`main.go:483-548`, `lance.go:500-530`):
  - `fetchK = max(limit*8, 50)`.
  - A single `Select` with `Columns = lanceOutputColumns` (15 columns, including `forward_content`), `VectorSearch{K: fetchK, Nprobes: 128}` plus `FullTextQuery` and an RRF reranker (k=60).
  - Then Go's `collapseChunkHits` collapses to `limit` distinct `parent_id`s.

## What has been measured

- **Proxy run, pre-`592615d`** (warm disk cache copied from production, IVF 64 partitions / 32 probes). Session `01a10d1a`, turn 484.
  - Fresh hybrid search: 163.5 R2 requests per query (median), 4.24 MB, 0.68 s p50. Of those, 117.9 were range GETs on the compacted data file, 44.7 were whole GETs of small fragment files, and about 1 was an `_indices` read.
  - Repeated query: 45 requests, 0.92 MB.
  - `strace` agreed: 150 requests fresh, 37 repeated, 8 sequential waves, 0.64 s of a 0.82 s search spent waiting on R2.
- **After `592615d`**, warm: 0 R2 requests per search; fresh hybrid 0.06–0.09 s (CHANGELOG).
- **Cold index load, 64/32 era:** the first query took 4.8–21.9 s while the index loaded from R2 (CHANGELOG, 2026-10-05).
- **Recall:** 512/128 matched 64/32 at recall@5 0.99 against numpy brute force (30 queries, local copy). Hybrid recall fell to 0.73 at 16 probes with 64 partitions.
- **Not yet measured:** a true cold-search byte count for the current 512/128 IVF_FLAT. The estimate is ~134 MiB (128/512 × 537 MiB), because IVF_FLAT stores full vectors.

## What to investigate, in order

1. **Baseline.** Measure the current production configuration fully cold, through the read-only proxy. Use a fresh process per query and no cache (or an empty cache directory with warm-up prevented). Record per query: R2 requests, bytes, number of sequential waves, wall time and CPU. Split the numbers by object kind (`_versions`, `_indices`, `data`, `_deletions`) and by query mode (vector, keyword, hybrid). This either confirms or refutes the 134 MiB estimate.
2. **Vector index variants**, in the scratch prefix:
   - IVF_PQ with varying `num_sub_vectors` (e.g. 24/48/96), partition counts and `nprobes`, with and without `refine_factor`.
   - Any other index types this Lance version supports (IVF_SQ, IVF_HNSW_SQ/PQ, RaBitQ/IVF_RQ, if available).
   - Check what the Go binding (`lancedb-go` contracts) can build and query. pylance/lancedb Python (0.37.1 / 10.0.0, as in the old sidecar) is fine for fast experiments, but the winner has to work through the Go path.
   - Measure recall@5 against numpy brute force for each, plus cold bytes, requests and waves.
3. **Two-step row fetch.**
   - Phase 1: `Columns: id, parent_id` (plus score) for the candidates.
   - Collapse in Go.
   - Phase 2: `WHERE id IN (final ids)` with the 15 output columns, using `id_btree`.
   - Measure the requests and bytes saved against the extra round trip.
   - Also check whether lowering `fetchK` hurts results after collapse.
4. **FTS cold cost.** How many bytes do the posting lists and token dictionary cost for typical queries? Does the FTS index layout or a newer Lance version change this?
5. **Unindexed tail.** How many requests the small fragments written since the last compaction add, and the cheapest way to keep that near zero (optimizer cadence, ingestion batch size `PI_MEMORYD_RAW_WRITE_INTERVAL`).
6. **Minimum number of waves.** What is the smallest number of sequential R2 round trips a cold search can have (manifest → index metadata → partitions/postings → rows)? Can any of them be collapsed or prefetched (for example by caching manifest and index metadata, which are small)?

## Measurement rules

- Use the same 30-query set as the 512/128 recall test where possible. Ground truth is exact search computed locally with numpy. Report recall@5 for vector and for hybrid.
- Report every configuration as one row: index type and parameters, cold bytes, cold requests, cold waves, cold wall time, warm wall time, recall@5 (vector, hybrid), index size.
- Repeat runs to get stable medians. Note R2 tail outliers instead of hiding them.
- Use throwaway scripts under `~/ledger-bench/`. Do not add permanent tests for benchmarks.

## Prior tooling

- Session `~/.omp/agent/sessions/-Developer-tireless-ledger/2026-10-05T17-27-01-680Z_01a10d1a-c930-7736-8868-7c2a161a249d.jsonl` contains:
  - the read-only logging proxy (`/var/tmp/tl-r2/proxy.py`, since deleted; its source is in the session's write/eval calls);
  - `~/ledger-bench/run-proxy.sh`, which runs a bench `pi-memoryd` on :8095 with `--raw-url "" --dry-run-s3` through the proxy on :19000;
  - the classification code (turn 482).
- tireless recall (`tireless query ...`, or the `tireless_recall` device in omp) searches older sessions.

## Deliverable

Append a **Results** section to this file containing:
- the table described above;
- the recommended configuration and why;
- the exact code and config changes needed (`lance.go`, `main.go`, `home-satan/modules/tireless-ledger.nix`, the disk-cache patch if relevant);
- the rollout and rollback steps.

Then stop and report. Do not deploy, do not touch production data, and do not publish releases.

## Results

Measured on home-satan, 2026-10-09. Nothing was deployed. Production was only read, through the read-only proxy: 0 write attempts were rejected and 0 reached R2. The scratch prefix `bench-cold-20261009/` has been deleted, and so has the throwaway tooling in `~/ledger-bench/`. The full write-up, with every measurement, is [`docs/cold-search-r2-2026-10-09.md`](../docs/cold-search-r2-2026-10-09.md).

### Method

- **Proxy.** `proxy.py` accepts only GET and HEAD, re-signs each request for R2 and logs it. Two proxy artefacts were fixed before the numbers below were taken:
  - the stdlib listen backlog of 5 dropped SYNs, which stalled the client on 4/8/16 s retransmits;
  - stale pooled upstream connections.
- **Client.** `coldq` is a Go program linked against the production `liblancedb_go.a` (lance-lib-0.37.1-r2, Lance 10.0.0) and the patched lancedb-go.
  - Every query runs in a fresh process with `LANCE_DISK_CACHE_DIR` unset: connect, open `chunks`, run one search.
  - The search uses the production query shape: fetchK 50, RRF k=60, collapse to 5 parents.
- **Waves.** The longest chain of R2 requests in which each request starts after the previous one has finished.
- **Data.** A read-only copy of prod `chunks` v12821 (329,485 rows): one 720 MB compacted file, one 526-row file and 15 tail fragments (133 rows). Variants were built locally and served from the scratch prefix.
- **Queries.** 30 queries from the 10-06 generator, `qset("recall3", 30)`. The 10-06 strings themselves could not be recovered (they were seeded with Python's per-process `hash()`), so this is a new draw with the same shape. Cold numbers are medians over the first 10 queries; the index scan in table A uses 5.
- **Recall@5.**
  - Vector truth: numpy L2 brute force over all vectors.
  - Hybrid truth: an exact pipeline (BypassVectorIndex, same FTS, fetchK 50).
  - Scoring is tie-aware: a returned parent counts if its true distance, or its exact RRF score, is at least the 5th truth value. RRF ranks and duplicate vectors tie exactly, so plain set overlap moves ±0.02 between identical runs.
- **Warm wall time.** "R2" means the same process running 10 other queries with no disk cache. "local" means the same with the dataset on local disk, which approximates production with a warm disk cache (0 R2 requests).

### 1. Baseline: current production config, fully cold (10 queries)

| mode | R2 requests | MB | waves | wall p50 (max) | CPU |
|---|---|---|---|---|---|
| vector | 515 | 202.2 | 84 | 12.0 s (12.7) | 0.56 s |
| text | 335 | 38.9 | 25 | 3.2 s (3.3) | 0.26 s |
| hybrid | 779 | 218.7 | 83 | 11.2 s (12.1) | 0.71 s |

Hybrid requests and MB by object kind:

| object | requests | MB |
|---|---|---|
| `_indices` IVF_FLAT `auxiliary.idx` | 266 | 176.7 |
| `_indices` IVF_FLAT `index.idx` | 4 | 1.0 |
| `_indices` FTS: tokens / docs / postings | 16 / 12 / 111 | 3.0 / 2.5 / 8.8 |
| `data` | 364 | 27.7 |
| `_versions` + list + `_deletions` | 4 | <0.1 |

Vector mode and text mode are the matching subsets: vector reads aux 266 / 176.7 and data 241 / 24.9; text reads FTS 141 / 14.3 (including 2 metadata reads) and data 194 / 24.3.

Findings:

- **The 134 MiB estimate was too low.** 128 of the 512 IVF_FLAT partitions add up to 177 MB, which is 35% of the 508 MB file. Partitions near a query are larger than average.
- **About 24 MB of the "data" bytes are page metadata, not rows.** Lance 2.1 loads every page's dictionary for each projected column on first access. The dictionaries of `parent_id`, `file_hash` and `file_path` alone are about 20 MB. The full-zip repetition index of `forward_content` adds another 1.3 MB.
- **The 83 waves come from partition parallelism.** IVF partitions are searched 2 at a time, because Lance's CPU pool is 4 CPUs minus 2 reserved for IO. 128 probes therefore take about 64 sequential round trips.
- **R2 tail outliers.** IVF_FLAT runs took up to 25-26 s when 1-2 MB partition reads hit 1-1.5 s R2 latency inside a 70+ step chain.
- **The real daemon behaves the same way.** Running prod `pi-memoryd` 0.2.1 on prod data through the proxy, with no disk cache:
  - startup took 5.3 s, 188 requests and 29 MB;
  - the first hybrid query took 11.1 s, 602 requests, 190 MB and 78 waves;
  - the next queries still took 3.4-6.5 s each, because each one probed partitions that were not yet loaded.

### 2. Vector index variants (vector mode, production data layout, 5 queries)

Cold MB includes the ~25 MB of dictionary pages described in §1. All 512-partition variants reuse the production IVF centroids. The 256-, 64- and 16-partition variants trained their own centroids, and the 1-partition variant has none.

| index, query params | cold MB (vector index) | requests | waves | wall p50 (max) | local warm, hybrid | recall@5 vec / hyb | index size |
|---|---|---|---|---|---|---|---|
| IVF_FLAT 512, np 128 (prod) | 202.2 (177.7) | 513 | 88 | 12.0 s (12.6) | 0.132 s | 0.973 / 0.977 | 508.6 MB |
| IVF_SQ 512, np 128, refine 2 | 69.6 (45.1) | 528 | 86 | 13.1 s (17.3) | 0.105 s | 0.973 / 0.973 | 129.2 MB |
| IVF_PQ 512 m96, np 128, refine 4 | 39.6 (14.3) | 592 | 88 | 12.5 s (17.9) | 0.135 s | 0.973 / 0.960 | 34.8 MB |
| IVF_RQ 512 (RaBitQ), np 128, refine 12 | 39.9 (11.3) | 1231 | 93 | 14.3 s (17.8) | 0.116 s | 0.973 / 0.973 | 22.4 MB |
| IVF_PQ 512 m48, np 128, refine 12 | 38.2 (8.9) | 813 | 93 | 13.0 s (16.7) | 0.156 s | 0.973 / 0.953 | 18.9 MB |
| IVF_PQ 256 m96, np 96, refine 4 | 40.3 (15.1) | 532 | 72 | 10.4 s (12.9) | 0.129 s | 0.993 / 0.980 | 34.4 MB |
| IVF_PQ 64 m96, np 32, refine 4 | 44.1 (18.7) | 402 | 38 | 5.9 s (10.3) | 0.112 s | 0.980 / 0.973 | 34.1 MB |
| IVF_PQ 16 m96, np 16, refine 4 | 59.4 (34.3) | 373 | 30 | 4.6 s (8.4) | 0.094 s | 1.000 / 0.980 | 34.0 MB |
| **IVF_PQ 1 m96, refine 4** | 59.5 (34.0) | **328** | **22** | **3.5 s (5.8)** | **0.089 s** | **1.000 / 0.987** | 34.0 MB |
| IVF_PQ 512 m96 + `LANCE_CPU_THREADS=32` | 39.9 | 772 | 31 | 3.9 s (4.2) | | as above | |
| IVF_PQ 256 m96, np 96 + 32 threads | 40.7 | 682 | 28 | 3.8 s (3.9) | | as above | |
| IVF_PQ 64 m96, np 32 + 32 threads | 44.4 | 550 | 26 | 3.2 s (3.5) | | as above | |

Variants not in the table:

- **HNSW.** IVF_HNSW_SQ with 16 or 64 partitions (148 MB) reaches at most 0.80 / 0.88 at 4-8 probes, with ef 150 and refine, and it reads whole partitions. IVF_HNSW_PQ was not built for the same reason.
- **IVF_PQ 512 m24.** 0.93 / 0.91 even with refine 12.
- **Quantized indexes without refine.** Recall falls: PQ 512 m96 at np 128 scores 0.70 / 0.79, and SQ scores 0.93 / 0.96. Refine re-reads full vectors from the data file, roughly one ~1.5 KB request per row (fetchK × refine factor rows).
- **Scan fraction.** These embeddings need 25-50% of the vectors scanned to reach recall@5 ≥ 0.97 (25% at 512/128, 37.5% at 256/96, 50% at 64/32, 100% at 16/16). That makes a full single-partition PQ scan (34 MB) competitive.
- **More CPU threads.** `LANCE_CPU_THREADS` cuts waves for indexes with many partitions, but it splits partition reads into more requests. With a single partition it does not matter.
- **Go binding support.** lancedb-go can build and query IVF_FLAT, IVF_PQ and IVF_HNSW_*. IVF_SQ and IVF_RQ need a Go enum value; the Rust side already maps `ivf_sq` and `ivf_rq`. The recommended IVF_PQ needs no change to the binding.
- **Build times on home-satan.** The recommended index (IVF_PQ 1 m96) takes about 3 min 40 s through the Go path, niced. IVF_FLAT 512 takes about 60 s per the optimize script comment, PQ 512 m96 about 5 min on 2 cores, SQ 22-42 s and RQ 9-37 s.

### Data layout (found while doing step 1)

The fix is field metadata. Set `lance-encoding:dict-divisor=1000000000` on `parent_id`, `file_hash`, `file_path` and `session_id`, and `lance-encoding:structural-encoding=miniblock` on `forward_content`.

- **Effect.** The data reads of a cold text query fall from 24.3 MB to 3.8 MB. The new data file is 737 MB instead of 720 MB.
- **No migration is needed.** After a metadata-only commit on the existing table, the production binary's own `--optimize` rewrites the 720 MB file with the new encodings. Verified on a local copy: no dictionary pages and miniblock `forward_content`. This works because the 329k-row fragment is below Lance's 1M-row compaction target, so every optimize run rewrites it anyway.

### 3. Two-step row fetch and fetchK

- **On today's layout the two-step fetch saves almost nothing.** Text drops from 335 to 300 requests and from 38.9 to 37.8 MB, with the same number of waves, because the cost is in the page dictionaries.
- **After the re-encode it pays off.** With the 1-partition PQ index, hybrid drops from 642 to 379 requests and from 53.3 to 47.7 MB, still at 27 waves.
  - Text-only gets cheaper (317 → 228 requests, 15.0 → 12.8 MB) but 0.26 s slower: 3.38 → 3.64 s p50 in an interleaved A/B, because the second read adds a round trip.
  - Phase 1 only needs `parent_id`, not `id`; dropping `id` saves another 47 requests.
- **Phase 2 should use `_rowid IN (…)`, not `id IN (…)` through `id_btree`.** The btree adds `page_lookup` and `page_data` reads: 373 vs 300 requests and 36 vs 30 waves.
  - Lance 10 bug: a `_rowid IN` scan with a Limit applies the limit as a row range before the filter and returns 0 rows. Phase 2 therefore runs without a Limit.
  - Row ids are row addresses, so a merge-insert that lands between the two phases can drop a hit. The patch falls back to the single-pass select when phase 2 returns fewer rows than phase 1 kept.
- **fetchK must stay at 50.** Measured against the fetchK-50 exact pipeline, hybrid recall@5 falls to 0.74 at fetchK 30 and 0.69 at fetchK 20. Vector-only recall stays at 1.0.

### 4. FTS cold cost

- **Size.** On the re-encoded data, FTS costs 12-14 MB, 88-141 requests and about 13 sequential waves per cold text or hybrid query. Once the vector index is small, this is the critical path.
  - The token dictionary is read whole: 1.45 MB.
  - Doc lengths: 1.3-2.4 MB.
  - Posting lists: 7.5-9.9 MB for the 3-4 query terms. These synthetic queries use common words ("build", "cache", "index").
- **Why it is sequential.** Lance 10 loads tokens, then postings, then docs. Each load is footer → metadata → data.
- **A newer Lance helps.** On the same index, pylance 13.0.0 reads 7.9 MB in 54 requests (21 waves), against 12.7 MB in 88 requests (22 waves) for pylance 10.0.0 (Python harness, 5 queries). Building the index with 13 adds nothing (8.3 MB). Getting this into the Go path needs a lancedb-go Rust rebuild on Lance 13, so it is not part of the recommendation.
- **Partition count doesn't matter.** The 2-partition FTS index that `--optimize` produces costs 12.4 MB; a fresh 1-partition rebuild costs 12.1 MB. There is no reason to rebuild FTS.
- **The doubled reads in production come from the tail.** Production reads tokens and docs twice (3.0 / 2.5 MB vs 1.45 / 1.26). The cause is the unindexed tail (§5), not the partition count.

### 5. Unindexed tail

- **Cost.** Measured on the re-encoded copy, 15 tail fragments (150 rows) against none:
  - hybrid: +83 requests (+16%), +2.9 MB, +2 waves;
  - text: +28 requests, +2.1 MB.
- **Per fragment.** That is about 5.5 requests per fragment per cold hybrid query. Both channels read every tail file, and the FTS index is loaded a second time.
- **Size in production.** The journal for 10-06 to 10-08 shows 31-58 fragments accumulating between optimizer runs (4-hourly, threshold 20). At the peak that is 170-320 extra requests, about as much as the whole post-change budget of ~400.
- **Options.**
  - **(a) Run the optimizer more often.** With the recommended index, `--optimize` folds the tail into the single PQ index: on the local copy, 15 tail fragments → 1 index, 0 unindexed rows, in 19 s, with no retrain. The drop/rebuild disappears, and an hourly run with a lower threshold becomes affordable. However, each run still rewrites the 740 MB file, invalidates the disk cache's data blocks and restarts the daemon. Time on R2 not measured.
  - **(b) Raise `PI_MEMORYD_RAW_WRITE_INTERVAL`** from 5m to 15m, so growing sessions are re-indexed less often [INFERENCE: effect not measured].
  - Neither option is part of the recommendation; both trade freshness or downtime for requests.

### 6. Minimum number of waves

The recommended config uses 28 waves for a hybrid query:

1. list `__manifest/_versions/` (lancedb 0.37's namespace probe);
2. list `chunks.lance/_versions/`;
3. read the manifest;
4. read FTS metadata, then tokens (2 waves);
5. read vector `index.idx` (read three times in a row);
6. read the PQ aux file (2-4 waves), overlapping with the FTS postings and docs (~10 waves);
7. read rows (8 waves: footer, column metadata, page metadata, rows; once for phase 1 and once for phase 2).

- **Floor.** With Lance 10 the floor is about 22: 3 to open, ~13 for the FTS chain, ~6 for rows.
- **Collapsible without changing Lance.** The two extra `index.idx` reads and the duplicate FTS reads go away with an in-process single-flight cache of identical immutable reads. Simulated in the proxy: hybrid 28 → 24 waves, 407 → 351 requests, 3.5 → 3.0 s.
  - The disk cache already behaves this way when its directory is set. But the first read starts `warm_root`, which copies the whole table, so this would need a "cache on, warm-up off" switch in `docker/lancedb-go-disk-cache.rs`. That is a Rust lib rebuild, not done here, and optional.
- **Not collapsible from config:**
  - lancedb's namespace probe;
  - list + manifest, which the disk cache sends to R2 by design;
  - Lance 10's sequential FTS loads.
- **Prefetch.** pi-memoryd's `warm()` already loads the FTS index during startup. With the patch, the first hybrid query after a no-cache restart needs only 14 waves (end-to-end table).

### End-to-end table: hybrid, fully cold, 10 queries

| # | configuration | cold MB | cold requests | cold waves | cold wall p50 (max) | warm wall: R2 / local | recall@5 vec / hyb | vector index |
|---|---|---|---|---|---|---|---|---|
| B0 | production today: IVF_FLAT 512, np 128, dictionary pages, single select | 218.7 | 779 | 83 | 11.2 s (12.1) | 1.68 s / 0.13 s | 0.973 / 0.977 | 508.6 MB |
| B1 | B0 + re-encoded data | 195.3 | 706 | 83 | 11.2 s (11.9) | – / 0.082 s | 0.973 / 0.993¹ | 507.7 MB |
| B2 | B1 + two-step fetch (`_rowid`) | 191.0 | 538 | 83 | 11.9 s (13.4) | – / 0.073 s | = B1 | 507.7 MB |
| B3 | B2 + IVF_PQ 512 m96, np 128, refine 4, `LANCE_CPU_THREADS=32` | **30.0** | 838 | 32 | 3.8 s (4.1) | – / 0.055 s | 0.973 / 0.993¹ | 33.7 MB |
| B4 | B2 + IVF_PQ 64 m96, np 32, refine 4, 32 threads | 34.8 | 631 | 30 | 3.5 s (3.7) | – / 0.084 s | 1.000 / 0.980 | 34.1 MB |
| B4′ | B4 with default threads | 34.4 | 472 | 38 | 5.2 s (6.2) | | | |
| **B5 ★** | B2 + **IVF_PQ 1 m96, refine 4**, phase 1 reads `parent_id` only | 48.6 | **407** | **28** | **3.5 s (4.0)** | **0.82 s / 0.068 s** | **1.000 / 0.987** (daemon API 1.000 / 1.000) | 34.0 MB |
| B6 | B5 + simulated single-flight read cache (§6) | 47.2 | 351 | 24 | 3.0 s (3.7) | | = B5 | |

¹ Scored against the exact pipeline on its own (single-partition) FTS index.

B5's data and index were produced the way the rollout would produce them:

- the pylance metadata-only commit on a prod copy;
- the patched `pi-memoryd --drop-vector-index`, `--optimize` and `--create-vector-index`;
- served by the patched daemon over HTTP: recall 1.000 / 1.000 and all 14 metadata fields present.

B5 by mode, compared with B0:

| mode | requests | MB | waves | wall p50 (max) | CPU |
|---|---|---|---|---|---|
| vector | 515 → 288 | 202.2 → 36.4 | 84 → 23 | 12.0 → 3.5 s (8.0, one R2 tail of 1.1 s) | 0.56 → 0.21 s |
| text | 335 → 228 | 38.9 → 12.8 | 25 → 27 | 3.3 → 3.6 s (interleaved A/B) | 0.26 → 0.18 s |
| hybrid | 779 → 407 | 218.7 → 48.6 | 83 → 28 | 11.2 → 3.5 s | 0.71 → 0.29 s |

Daemon restart, real `pi-memoryd` through the proxy with no disk cache, B0 → B5:

- startup: 5.3 → 4.8 s, 188 → 133 requests, 29 → 5.4 MB;
- first hybrid query: 11.1 → 2.0 s, 602 → 256 requests, 190 → 40 MB, 78 → 14 waves;
- next queries: 3.4-6.5 s → about 1 s.

### Recommended configuration

B5:

- **Data:** re-encode it (no dictionary pages on `parent_id`, `file_hash`, `file_path`, `session_id`; miniblock `forward_content`).
- **Vector index:** a single-partition IVF_PQ with 96 sub-vectors, queried with refine factor 4 (nprobes is irrelevant).
- **Rows:** fetch them in two steps (`parent_id` + `_rowid` first, collapse in Go, then the 15 columns for the 5 kept rows only).
- **fetchK:** 50, as today.

Why B5:

- **It has the fewest requests and waves of every config that holds recall.** Hybrid uses 407 requests (−48%) and 28 waves (−66%), and takes 3.5 s instead of 11.2 s.
- **It moves 78% fewer bytes** (48.6 MB instead of 218.7 MB).
- **Recall goes up**, from 0.973 / 0.977 to 1.000 / 0.987, because every vector is scanned and then refined.
- **Nothing to tune.** No nprobes and no thread tuning, and no IVF centroids to drift, so `--optimize` can fold new rows into the index instead of rebuilding it.
- **The rest of the latency is FTS.** It is the floor at 12-14 MB and ~13 waves. B3 and B4 do not beat B5's wall time even with fewer vector bytes.

Ceiling: a full PQ scan reads about 103 B per chunk, i.e. 34 MB at 329k chunks. Around 1M chunks (~100 MB), switch to B4: 64 partitions, nprobes 32, `LANCE_CPU_THREADS=32`, which is −14 MB against B5 today at the cost of +224 requests and +2 waves. If bytes matter more than requests right now (for example a browser client on a metered link), B3 or B4 is the trade.

### Code changes

Verified in a scratch copy of this repo: `go vet`, `go test ./...` and `CGO_ENABLED=0 go build` all pass. The Rust lib (`lance-lib-0.37.1-r2`) is unchanged, so no `make lance-lib` is needed. Every changed line is below; hunk headers are abbreviated:

```diff
--- a/lance.go
+++ b/lance.go
@@ import (
 	"sort"
+	"strconv"
 	"strings"
@@ const (
-	lanceVectorIndexName  = "vector_ivf_flat"
-	lanceVectorPartitions = uint32(512)
-	lanceVectorNProbes    = 64
+	lanceVectorIndexName = "vector_ivf_pq"
+	// One partition: a cold search reads the whole PQ index (~100 B/chunk) in a few large
+	// requests instead of two small requests per probed partition, and recall is exact
+	// after refine. ponytail: full PQ scan; past ~1M chunks use 64+ partitions, probe ~50%.
+	lanceVectorPartitions   = uint32(1)
+	lanceVectorSubVectors   = uint32(96)
+	lanceVectorRefineFactor = uint32(4)
+	lanceVectorNProbes      = 1
 )
@@ func (s *cgoLanceStore) openOrCreateTable(...)
+// A search's first read of a column loads every page's dictionary (parent_id, file_hash
+// and file_path cost ~20 MB cold at 330k rows) and every full-zip page's repetition
+// index (forward_content, ~1.3 MB). Plain miniblock pages cost KBs. Compaction applies
+// these to existing tables once the dataset schema carries them.
+var (
+	lanceNoDictionary = arrow.NewMetadata([]string{"lance-encoding:dict-divisor"}, []string{"1000000000"})
+	lanceMiniblock    = arrow.NewMetadata([]string{"lance-encoding:structural-encoding"}, []string{"miniblock"})
+)
@@ func (s *cgoLanceStore) createTable(...)
-		{Name: "forward_content", Type: arrow.BinaryTypes.String, Nullable: true},
+		{Name: "forward_content", Type: arrow.BinaryTypes.String, Nullable: true, Metadata: lanceMiniblock},
-		{Name: "file_path", Type: arrow.BinaryTypes.String, Nullable: true},
-		{Name: "file_hash", Type: arrow.BinaryTypes.String, Nullable: true},
+		{Name: "file_path", Type: arrow.BinaryTypes.String, Nullable: true, Metadata: lanceNoDictionary},
+		{Name: "file_hash", Type: arrow.BinaryTypes.String, Nullable: true, Metadata: lanceNoDictionary},
-		{Name: "session_id", Type: arrow.BinaryTypes.String, Nullable: true},
+		{Name: "session_id", Type: arrow.BinaryTypes.String, Nullable: true, Metadata: lanceNoDictionary},
-		{Name: "parent_id", Type: arrow.BinaryTypes.String, Nullable: true},
+		{Name: "parent_id", Type: arrow.BinaryTypes.String, Nullable: true, Metadata: lanceNoDictionary},
@@ func (s *cgoLanceStore) CreateVectorIndex(ctx context.Context) error {
-	partitions := lanceVectorPartitions
-	if err := table.CreateIndexWithParams(ctx, []string{"vector"}, contracts.IndexTypeIvfFlat,
-		contracts.IndexParams{NumPartitions: &partitions, DistanceType: contracts.DistanceTypeL2},
+	partitions, subVectors := lanceVectorPartitions, lanceVectorSubVectors
+	if err := table.CreateIndexWithParams(ctx, []string{"vector"}, contracts.IndexTypeIvfPq,
+		contracts.IndexParams{NumPartitions: &partitions, NumSubVectors: &subVectors, DistanceType: contracts.DistanceTypeL2},
 		&contracts.CreateIndexOptions{Name: lanceVectorIndexName, WaitTimeout: 10 * time.Minute}); err != nil {
-		return fmt.Errorf("create IVF-Flat vector index: %w", err)
+		return fmt.Errorf("create IVF-PQ vector index: %w", err)
 	}
-	slog.Info("created IVF-Flat vector index", "name", lanceVectorIndexName, "partitions", partitions, "nprobes", s.nprobes)
+	slog.Info("created IVF-PQ vector index", "name", lanceVectorIndexName, "partitions", partitions, "sub_vectors", subVectors, "nprobes", s.nprobes)
@@ func (s *cgoLanceStore) DropVectorIndex(ctx context.Context) error {
+	// By column, not name: the IVF-Flat index this replaces is named vector_ivf_flat.
 	for _, index := range indexes {
-		if index.Name == lanceVectorIndexName {
+		if len(index.Columns) == 1 && index.Columns[0] == "vector" {
 			if err := table.DropIndex(ctx, index.Name); err != nil {
-				return fmt.Errorf("drop IVF-Flat vector index: %w", err)
+				return fmt.Errorf("drop vector index %s: %w", index.Name, err)
 			}
-			slog.Info("dropped IVF-Flat vector index", "name", index.Name)
+			slog.Info("dropped vector index", "name", index.Name, "type", index.IndexType)
 			return nil
 		}
 	}
-	slog.Info("IVF-Flat vector index is absent", "name", lanceVectorIndexName)
+	slog.Info("vector index is absent")
@@ func (s *cgoLanceStore) Search(...)
 		nprobes := s.nprobes
+		refine := lanceVectorRefineFactor
 		config.VectorSearch = &contracts.VectorSearch{
-			Column: "vector", Vector: req.Vector, K: limit, Nprobes: &nprobes,
+			Column: "vector", Vector: req.Vector, K: limit, Nprobes: &nprobes, RefineFactor: &refine,
@@
-	rows, err := table.Select(ctx, config)
+	var rows []map[string]interface{}
+	var err error
+	if req.Collapse > 0 && (hasVector || hasText) {
+		rows, err = selectCollapsed(ctx, table, config, req.Collapse)
+	} else {
+		rows, err = table.Select(ctx, config)
+	}
 	if err != nil {
@@ (new function after Search)
+// selectCollapsed ranks candidates reading only parent_id, keeps the best row of each of
+// the first `parents` parents, then reads the output columns for just those rows. A cold
+// search reads ~5 rows of the wide columns instead of fetchK per channel.
+func selectCollapsed(ctx context.Context, table contracts.ITable, config contracts.QueryConfig, parents int) ([]map[string]interface{}, error) {
+	rank := config
+	rank.Columns = []string{"parent_id"}
+	rank.WithRowID = true
+	hits, err := table.Select(ctx, rank)
+	if err != nil {
+		return nil, err
+	}
+	seen := make(map[string]bool, parents)
+	kept := make([]map[string]interface{}, 0, parents)
+	rowIDs := make([]string, 0, parents)
+	for _, hit := range hits {
+		rowID := strconv.FormatUint(uint64(numberValue(hit["_rowid"])), 10)
+		parent := stringValue(hit["parent_id"])
+		if parent == "" {
+			parent = rowID
+		}
+		if seen[parent] {
+			continue
+		}
+		seen[parent] = true
+		kept = append(kept, hit)
+		rowIDs = append(rowIDs, rowID)
+		if len(kept) == parents {
+			break
+		}
+	}
+	if len(kept) == 0 {
+		return nil, nil
+	}
+	// No Limit: Lance 10 applies a limit on a `_rowid IN` scan as a row range before the
+	// filter and returns nothing.
+	full, err := table.Select(ctx, contracts.QueryConfig{Columns: config.Columns, Where: "_rowid IN (" + strings.Join(rowIDs, ",") + ")", WithRowID: true})
+	if err != nil {
+		return nil, err
+	}
+	if len(full) != len(kept) {
+		// A concurrent merge-insert rewrote a hit between the two reads (row ids are row
+		// addresses); rank and fetch in one pass instead.
+		return table.Select(ctx, config)
+	}
+	byRowID := make(map[float64]map[string]interface{}, len(full))
+	for _, row := range full {
+		byRowID[numberValue(row["_rowid"])] = row
+	}
+	for i, hit := range kept {
+		row := byRowID[numberValue(hit["_rowid"])]
+		if row == nil {
+			return table.Select(ctx, config)
+		}
+		for _, k := range []string{"_relevance_score", "_score", "_distance"} {
+			if v, ok := hit[k]; ok {
+				row[k] = v
+			}
+		}
+		delete(row, "_rowid")
+		kept[i] = row
+	}
+	return kept, nil
+}
--- a/main.go
+++ b/main.go
@@ type vectorSearchRequest struct {
 	AfterID       string        `json:"after_id,omitempty"`
+	// Collapse > 0 returns at most this many rows with distinct parent_id, reading the
+	// output columns only for them (vector/text searches).
+	Collapse int `json:"-"`
 }
@@ func loadConfig() runtimeConfig {
-	flag.IntVar(&cfg.VectorNProbes, "vector-nprobes", envInt("PI_MEMORYD_VECTOR_NPROBES", 64), "IVF partitions scanned per vector query")
+	flag.IntVar(&cfg.VectorNProbes, "vector-nprobes", envInt("PI_MEMORYD_VECTOR_NPROBES", 1), "IVF partitions scanned per vector query")
-	flag.BoolVar(&cfg.CreateVectorIndex, "create-vector-index", false, "create a 512-partition IVF-Flat vector index and exit")
-	flag.BoolVar(&cfg.DropVectorIndex, "drop-vector-index", false, "drop the IVF-Flat vector index and exit")
+	flag.BoolVar(&cfg.CreateVectorIndex, "create-vector-index", false, "create the single-partition IVF-PQ vector index and exit")
+	flag.BoolVar(&cfg.DropVectorIndex, "drop-vector-index", false, "drop the vector index and exit")
@@ func (s *server) query(w http.ResponseWriter, r *http.Request) {
-	searchReq := vectorSearchRequest{Table: "chunks", K: fetchK, Filter: filter, IncludeFields: include}
+	searchReq := vectorSearchRequest{Table: "chunks", K: fetchK, Filter: filter, IncludeFields: include, Collapse: req.Limit}
```

Other files and steps:

- `CHANGELOG.md` gets one entry when the change is implemented.
- `docker/lancedb-go-disk-cache.rs` needs no change. The "cache on, warm-up off" switch from §6 is optional and would need a lib rebuild.
- The existing table needs a one-off, metadata-only commit, because lancedb-go has no field-metadata API. Use pylance 10.0.0, the same Lance version as the Go lib. Tested on a local copy and on the scratch copy in R2. Script `set-encoding-metadata.py`:

```python
import sys
import lance
uri, so = sys.argv[1], dict(kv.split("=", 1) for kv in sys.argv[2:])
ds = lance.dataset(uri, storage_options=so or None)
for field in ("parent_id", "file_hash", "file_path", "session_id"):
    ds.update_field_metadata({field: {"lance-encoding:dict-divisor": "1000000000"}})
ds.update_field_metadata({"forward_content": {"lance-encoding:structural-encoding": "miniblock"}})
```

`home-satan/modules/tireless-ledger.nix` changes:

- Bump the `piMemoryd` input to the release that carries the diff above.
- In `optimizeLedger`, stop dropping the vector index. `--optimize` now folds new rows into the single PQ index (verified: 1 index, 0 unindexed rows), and `--create-vector-index` becomes a no-op while the index exists. This also cuts optimizer downtime by the ~1-4 min index build.

  ```diff
  -    # ponytail: retrain IVF on every compaction (~60 s for 512 partitions at 326k chunks) instead of
  -    # tracking corpus growth; add a row-count trigger if build time starts to matter.
  -    # A failed rebuild leaves no index, and queries fall back to an exact scan.
  -    "''${run_as_user[@]}" ${piMemoryd}/bin/pi-memoryd --drop-vector-index
       "''${run_as_user[@]}" ${piMemoryd}/bin/pi-memoryd --optimize
  +    # Recreates the vector index only when it is missing; --optimize folds new rows into it.
       "''${run_as_user[@]}" ${piMemoryd}/bin/pi-memoryd --create-vector-index
  ```

- After the cutover, remove `PI_MEMORYD_VECTOR_NPROBES=128`, which makes the code default of 1 apply. Leave it in place during the cutover: with 128 the new binary still serves the old IVF_FLAT index at today's recall, and on the 1-partition PQ index it costs the same as 1 (verified).

### Status (2026-10-10)

Approved after the disk-cache experiments (docs report §3). Implemented in this repo:

- `lance.go`, `main.go`: the diff above, applied verbatim; `go vet`, `go test ./...` and the `CGO_ENABLED=0` build pass.
- `CHANGELOG.md`: "Single-partition IVF_PQ, refine, two-step row fetch (2026-10-10)".
- `home-satan/modules/tireless-ledger.nix`: `optimizeLedger` no longer runs `--drop-vector-index`; the `piMemoryd` pin and `PI_MEMORYD_VECTOR_NPROBES` are changed in rollout steps 2 and 5.

Deferred, in order: optimize without stopping the daemon (measure queries during a live `--optimize` on a scratch copy); `PI_MEMORYD_RAW_WRITE_INTERVAL` 5m → 15m; the E2 disk-cache promotion with the next Rust lib rebuild.

### Rollout

1. In `tireless-ledger`, apply the diff and add the CHANGELOG entry. Run `go test ./...`, then `make release-linux` and publish pi-memoryd. There is no lib rebuild.
2. In `home-satan`, bump the input and apply the optimize-script change, keeping `PI_MEMORYD_VECTOR_NPROBES=128`. Then run `make switch`. The new binary now serves the old IVF_FLAT index with refine and the two-step fetch, at the same recall.
3. Cutover, about 6 minutes of service downtime:
   ```sh
   sudo systemctl stop tireless-optimize.timer pi-memoryd
   set -a; . /run/secrets/rendered/tireless-ledger.env; set +a
   # metadata-only commit with pylance 10.0.0 (the script from "Code changes", saved as set-encoding-metadata.py)
   LD_LIBRARY_PATH=$(nix eval --raw nixpkgs#stdenv.cc.cc.lib)/lib:$(nix eval --raw nixpkgs#zlib)/lib \
     nix shell nixpkgs#uv -c uv run --no-project --with pylance==10.0.0 \
     python set-encoding-metadata.py "$PI_MEMORYD_STORAGE_URL/chunks.lance" \
     aws_endpoint=$PI_MEMORYD_S3_ENDPOINT aws_region=auto aws_virtual_hosted_style_request=false \
     aws_access_key_id=$AWS_ACCESS_KEY_ID aws_secret_access_key=$AWS_SECRET_ACCESS_KEY
   PIM=$(systemctl show pi-memoryd -p ExecStart --value | grep -o '/nix/store/[^ ;]*/bin/pi-memoryd' | head -1)
   $PIM --drop-vector-index      # drops vector_ivf_flat (by column)
   $PIM --optimize               # rewrites chunks data with the new encodings (~740 MB to R2)
   $PIM --create-vector-index    # IVF_PQ 1×96, ~3.5-4 min on home-satan
   sudo systemctl start pi-memoryd tireless-optimize.timer
   ```
4. Verify:
   - the journal shows `created IVF-PQ vector index`;
   - `pi-memoryd --fragment-counts` reports ≤2 fragments for `chunks`;
   - one hybrid query through `/v1/memory/query` returns full metadata.
   - Optional: re-run a cold query through a read-only proxy and compare with B5 in the docs report.
5. Remove `PI_MEMORYD_VECTOR_NPROBES=128` and run `make switch` again.

### Rollback

The old binary drops the vector index by the name `vector_ivf_flat` only, and it searches without refine. If it starts while the PQ index exists, it keeps that index, and recall@5 falls to about 0.70 / 0.79 (measured on PQ 512 m96 without refine). Drop the PQ index first:

1. `sudo systemctl stop tireless-optimize.timer pi-memoryd`, then run the new binary's `--drop-vector-index`.
2. In `home-satan`, revert the input pin and the optimize script, keep `PI_MEMORYD_VECTOR_NPROBES=128`, and run `make switch`.
3. Rebuild IVF_FLAT 512 with `$OLD_PIM --create-vector-index` (~60 s), then `sudo systemctl start pi-memoryd tireless-optimize.timer`.
4. The re-encoded data can stay: the old binary reads it (same Lance 10) and it is strictly cheaper. To revert it anyway, call `update_field_metadata` with `replace=True` to drop the keys; the next `--optimize` then rewrites the file with the old encodings.

### Not done, and limits

- **Lance 13.** Not tried in the Go path, because it needs a lancedb-go Rust rebuild. Measured in Python only (§4: −38% FTS bytes).
- **Disk-cache "warm-up off" mode (§6).** Simulated, not built.
- **Tail cadence (§5).** The R2 time of an hourly `--optimize` and the effect of `PI_MEMORYD_RAW_WRITE_INTERVAL` were not measured.
- **Query set.** The 30 synthetic queries contain very common terms, so their FTS posting reads (7.5-9.9 MB) are probably above typical. Vector-side numbers do not depend on the query text.
- **Wall times include the proxy.** Upstream TLS connections are pooled; R2 round trips run 0.08-0.10 s p50.

### Addendum: HelixDB and Quickwit on the same harness (2026-10-09)

Same snapshot, refreshed to v12854 (329,654 chunks). Same 10 cold queries, same proxy and wave metric.

- Both engines ran in Docker on home-satan with their data in `bench-cold-20261009/{helix,quickwit}/`, deleted afterwards.
- Each engine had its own proxy that forwarded writes only under its own prefix. Bulk deletes were allowed only when every key sat under that prefix.
- HelixDB was capped at 2 CPUs / 2.5 GB, Quickwit at 1 CPU / 1.2 GB.
- "Fully cold" means a fresh container with an empty cache volume. For HelixDB the startup warm-up was also off (`HELIX_DISK_CACHE_WARM=off`). Quickwit used `tool local-search`, i.e. one new process per query.

| engine, mode | cold requests | cold MB | cold waves | cold wall | warm (other queries) | recall@5 | build |
|---|---|---|---|---|---|---|---|
| LanceDB, B5 recommended: vector | 288 | 36.4 | 23 | 3.5 s | 0.78 s (no disk cache) | 1.000 | IVF_PQ 3 min 40 s |
| LanceDB, B5: text | 228 | 12.8 | 27 | 3.6 s | 0.67 s (no disk cache) | n/a | FTS ~20 s |
| LanceDB, B5: hybrid | 407 | 48.6 | 28 | 3.5 s | 0.82 s (no disk cache) | 1.000 / 0.987 | |
| LanceDB, B0 production today: vector | 515 | 202.2 | 84 | 12.0 s | 1.57 s (no disk cache) | 0.973 | |
| **HelixDB v0.0.12** (HNSW): vector, first query | 260 | 828 | 97 | 16.4 s | 0.73 s p50 (9.5 req, 23 MB per new query) | **0.80** | load 15 min; vector index ~4 h 15 min |
| HelixDB: server startup before that query | 114 | 174 | 46 | 11.5 s | | | |
| HelixDB: text (BM25) | not measurable: the index build blocked twice with `invariant_violation` / `blob_missing_or_size_mismatch` | | | | | | |
| **Quickwit 0.9.1**, text only, 50 hits | 48 | 7.6 | **8** | **1.2 s** R2 span (1.8 s with container start) | 0.57 s p50 (40 req, 7 MB per new query) | n/a | **27 s** |
| Quickwit, text only, 5 hits | 22 | 1.7 | 7 | 0.9 s R2 span | | | |

HelixDB findings:

- **The disk cache fetches whole 4 MiB file parts.** 187 of the ~260 query requests were 4 MiB reads, which is why one cold vector query reads ~830 MB.
  - A smaller budget (1 GiB, which gives 2 MiB parts) made it worse: 540-700 requests, 0.93-1.2 GB, 200-290 waves, 28-39 s.
  - The cache can't be turned off on S3. Its own docs require it for vector indexes.
- **Graph search costs round trips.** 97 sequential waves, consistent with one dependent read per uncached hop.
- **Recall@5 is 0.80** against the numpy brute-force truth. The SDK exposes no ef/search-width knob.
- **The background writer reads ~3 requests per second while idle.** Over a 16 s query that is ~50 of the ~260 requests, so the query alone is ~210.
- **The text index never activated on R2.**
  - The first build broke when my proxy restarted mid-upload, and Helix's retry never re-uploaded the missing blob.
  - The clean rebuild, with no write errors and only one throttled list request in the log, blocked again in the same validation step.
  - An explicit retry re-blocked within 30 s. A HelixDB bug or an incompatibility with R2 is likely; the cause is not determined.

Quickwit findings:

- **Its cold read path is the one we were looking for:**
  1. `HEAD`+`GET manifest.json`, then `GET metastore.json`: 3 waves. The file-backed metastore on S3 costs these; a Postgres metastore or a running server avoids them.
  2. One read of the split footer + hotcache (78 KB).
  3. Term-dictionary and posting slices.
  4. Parallel doc-store block reads for the hits.

  That is 8 sequential round trips in total, against 27 for LanceDB text. The bytes are mostly doc-store blocks (~0.3 MB each) for the 50 returned documents; with 5 hits it is 1.7 MB.
- **It is text-only.** There is no vector search, so it can only replace the BM25 half of hybrid.
- **Its BM25 results differ from LanceDB's** because of the analyzer: `default` tokenizer, with no stemming or stop words in 0.9.1 (`en_stem` is not built in). Top-5 parent agreement with LanceDB's FTS is 0.44. This is a different ranking, not a recall loss.
- **Warm without a split cache still reads ~40 requests per new query.** Quickwit's searcher split cache is off by default; with it enabled, warm queries would come from local disk [INFERENCE, not measured].
- **Indexing 329k chunks took 27 s** into one 116 MB split. A first attempt through the proxy failed: single 100+ MB `PUT`s timed out in the Python proxy, the ingest retried, and it re-read the file 3 times. The successful ingest wrote directly to the scratch prefix; queries went through the proxy.

What this changes:

- **HelixDB is not a fit for cold or R2-attached search** at this point. It is slower than today's LanceDB config on every cold metric, holds lower recall, takes hours to build the index, and could not build a text index on R2.
- **Quickwit shows the text half can be done in ~8 round trips / ~1 s cold from home-satan**, against LanceDB's 27 / ~3.5 s.
  - A split stack (Quickwit for BM25 plus LanceDB IVF_PQ for vectors, fused with RRF in Go) would put the cold hybrid floor at about the slower of the two: LanceDB's ~23 vector waves.
  - Unless the vector side also gets the hotcache treatment, for example by prefetching the small index files in the disk-cache wrapper, it is not worth a second engine yet.
