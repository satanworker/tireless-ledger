# Cold search on R2: measurements and findings

Measured on home-satan between 2026-10-09 01:00 and 16:30 EEST. The brief is in [`tasks/🧊-cold-search-minimal-r2-fetch.md`](../tasks/🧊-cold-search-minimal-r2-fetch.md), and its Results section holds the full code diff and the rollout and rollback steps. Nothing was deployed. Production data was only ever read, through a read-only proxy. Every experiment that needed writes ran in the scratch prefix `s3://tireless-ledger/bench-cold-20261009/`, which has since been deleted.

"Fully cold" means a fresh process with an empty local cache. That is the state after a restart before warm-up, for a table larger than half the cache cap, and for a client with no cache at all.

## Method (shared by both sections)

- **Read-only logging proxy.** It re-signs requests for R2 and logs every request (start, end, path, range, status, bytes). It refuses PUT, POST and DELETE.
  - For the HelixDB and Quickwit experiments, a second instance forwarded writes only under that engine's own scratch sub-prefix. Bulk deletes passed only when every key in the request body was under the prefix.
  - 0 writes reached production.
  - Two proxy artefacts were found and fixed before the numbers below were taken: a listen backlog of 5 that stalled clients on 4/8/16 s SYN retransmits, and stale pooled upstream connections.
  - Upstream TLS connections are pooled, so a fresh client pays R2 round trips, not proxy handshakes. R2 round trips from home-satan were 0.08–0.10 s p50.
- **LanceDB client.** `coldq` is a small Go program linked against the production `liblancedb_go.a` (lance-lib-0.37.1-r2, Lance 10.0.0) and the patched lancedb-go.
  - Every query runs in its own process: connect, open `chunks`, run one search.
  - It uses the production query shape: fetchK 50, the 15 output columns, RRF k=60, collapse to 5 parents.
  - `LANCE_DISK_CACHE_DIR` is unset, so nothing is cached between runs.
- **Waves.** The longest chain of R2 requests in which each request starts after the previous one finished. This is the latency-critical sequence of round trips.
- **Data.**
  - LanceDB experiments: prod `chunks` v12821, 329,485 rows. The rows were one 720 MB compacted file, one 526-row file and 15 unindexed tail fragments (133 rows).
  - Engine comparison: v12854, 329,654 rows.
  - Vectors are 384-dim f32 from bge-small-en-v1.5.
- **Queries.** 30 queries from the 10-06 generator, `qset("recall3", 30)` with `random.Random("recall3")`. The exact 10-06 strings could not be recovered (they were seeded with Python's per-process `hash()`).
  - These queries were embedded without the bge query prefix (`"Represent this sentence for searching relevant passages: "`) that the Mac client adds (`mac/cli.py:16`). So did the 10-06 recall test.
- **Sample sizes.** Cold metrics are medians over the first 10 queries. Index-variant scans used 5. The max is reported next to the median.
- **Recall@5.**
  - Vector truth: numpy L2 brute force over every vector.
  - Hybrid truth: the exact pipeline, i.e. BypassVectorIndex, the same FTS index, fetchK 50, RRF, collapse.
  - Scoring is tie-aware: a returned parent counts if its true distance, or its exact-pipeline RRF score, is at least the 5th truth value. Plain set overlap moves ±0.02 between identical runs because RRF ranks and duplicate vectors tie exactly.
  - Each dataset is scored against truth computed on its own data and FTS index.
- **Warm wall time.** "R2" means the same process running 10 other queries, no disk cache. "Local" means the same with the dataset on local disk, which approximates production with a warm disk cache (0 R2 requests).
- **Resource limits.** The live `pi-memoryd` kept running. Builders ran niced and pinned to 2 CPUs, and containers were capped (HelixDB 2 CPUs / 2.5 GB, Quickwit 1 CPU / 1.2 GB).

---

## 1. LanceDB optimisations

### 1.1 Baseline: production config, fully cold

IVF_FLAT with 512 partitions and 128 probes, current data encoding, a single 15-column select.

| mode | R2 requests | MB | waves | wall p50 (max) | CPU |
|---|---|---|---|---|---|
| vector | 515 | 202.2 | 84 | 12.0 s (12.7) | 0.56 s |
| text | 335 | 38.9 | 25 | 3.2 s (3.3) | 0.26 s |
| hybrid | 779 | 218.7 | 83 | 11.2 s (12.1) | 0.71 s |

Hybrid by object kind:

| object | requests | MB |
|---|---|---|
| IVF_FLAT `auxiliary.idx` (partitions) | 266 | 176.7 |
| IVF_FLAT `index.idx` (centroids) | 4 | 1.0 |
| FTS tokens / docs / postings / metadata | 16 / 12 / 111 / 2 | 3.0 / 2.5 / 8.8 / 0 |
| data files | 364 | 27.7 |
| `_versions` + list + `_deletions` | 4 | <0.1 |

Findings:

- **The 134 MiB estimate was low.** 128 of the 512 partitions add up to 177 MB, which is 35% of the 508 MB index file. Partitions near a query are larger than average.
- **Most "data" bytes are page metadata, not rows.** Lance 2.1 loads, on first access, every page's dictionary for each projected column.
  - The dictionaries of `parent_id`, `file_hash` and `file_path` (1.1–2.9 MB per page) total about 20 MB of the 24–28 MB.
  - The full-zip repetition index of `forward_content`, 1.3 MB across all 20 pages, is loaded whole as well.
  - The actual rows cost about 2 MB.
- **The 83 waves come from partition parallelism.** IVF partitions are searched 2 at a time: Lance's CPU pool is 4 CPUs minus 2 reserved for IO. 128 probes therefore become about 64 sequential partition reads.
- **R2 tail outliers.** IVF_FLAT runs reached 25–26 s when 1–2 MB partition reads hit 1–1.5 s R2 latency inside a 70+ step chain.
- **About 15% of reads are duplicates** (first baseline run: 775 hybrid requests vs 657 unique ranges). Most are FTS tokens and docs read twice; that comes from the unindexed tail (see 1.6).
- **The production daemon itself, restarted with no disk cache** (prod binary, prod data, through the proxy):
  - startup: 5.3 s, 188 requests, 29 MB;
  - first hybrid query: 11.1 s, 602 requests, 190 MB, 78 waves;
  - the next queries: 3.4–6.5 s each, because every new query probes partitions not yet loaded.
- **The old strace estimate was low.** On 10-06 the same restart was measured with strace (128 probes, no disk cache): first vector query 15.8 s with 368 TLS writes and 120 gaps. The proxy numbers here confirm the order of magnitude.

### 1.2 Data layout: page dictionaries and miniblock text

Lance chooses dictionary encoding for string columns even when almost every value is different. Two field-metadata keys fix it:

- `lance-encoding:dict-divisor=1000000000` on `parent_id`, `file_hash`, `file_path` and `session_id`;
- `lance-encoding:structural-encoding=miniblock` on `forward_content`.

Cold text query, data reads only (same FTS index):

| layout | data MB | data requests |
|---|---|---|
| production (dictionary pages, full-zip text) | 24.3 | 193 |
| no dictionary on the 4 columns | 5.0 | 213 |
| + miniblock `forward_content` | 3.8 | 217 |
| + two-step fetch (1.4) | 1.7 | 138 |

- **Size.** The rewritten data file is 737 MB instead of 720 MB.
- **No migration needed.** A metadata-only commit on the existing table (pylance 10.0.0 `update_field_metadata`) followed by the production binary's own `pi-memoryd --optimize` rewrites the data with the new encodings. Verified on a copy:
  - no dictionary pages, miniblock `forward_content`;
  - same rows;
  - all indexes remapped.

  This works because the 329k-row fragment is below Lance's 1M-row compaction target, so every optimize run rewrites it anyway. The metadata commit also worked against a copy in R2 (scratch).
- **New tables.** `createTable` can carry the same metadata as Arrow field metadata; lancedb-go sends the schema as IPC, so the metadata survives.

### 1.3 Vector index variants

Vector mode, production data layout, 5 queries. Cold MB includes the ~25 MB of dictionary pages from 1.2. All 512-partition rows reuse the production IVF centroids; the 256-, 64- and 16-partition variants trained their own; the 1-partition variant has none.

| index, query params | cold MB (vector index) | requests | waves | wall p50 (max) | local warm, hybrid | recall@5 vec / hyb | index size |
|---|---|---|---|---|---|---|---|
| IVF_FLAT 512, np 128 (prod) | 202.2 (177.7) | 513 | 88 | 12.0 s (12.6) | 0.132 s | 0.973 / 0.977 | 508.6 MB |
| IVF_SQ 512, np 128, refine 2 | 69.6 (45.1) | 528 | 86 | 13.1 s (17.3) | 0.105 s | 0.973 / 0.973 | 129.2 MB |
| IVF_PQ 512 m96, np 128, refine 4 | 39.6 (14.3) | 592 | 88 | 12.5 s (17.9) | 0.135 s | 0.973 / 0.960 | 34.8 MB |
| IVF_RQ 512 (RaBitQ 1-bit), np 128, refine 12 | 39.9 (11.3) | 1231 | 93 | 14.3 s (17.8) | 0.116 s | 0.973 / 0.973 | 22.4 MB |
| IVF_PQ 512 m48, np 128, refine 12 | 38.2 (8.9) | 813 | 93 | 13.0 s (16.7) | 0.156 s | 0.973 / 0.953 | 18.9 MB |
| IVF_PQ 256 m96, np 96, refine 4 | 40.3 (15.1) | 532 | 72 | 10.4 s (12.9) | 0.129 s | 0.993 / 0.980 | 34.4 MB |
| IVF_PQ 64 m96, np 32, refine 4 | 44.1 (18.7) | 402 | 38 | 5.9 s (10.3) | 0.112 s | 0.980 / 0.973 | 34.1 MB |
| IVF_PQ 16 m96, np 16, refine 4 | 59.4 (34.3) | 373 | 30 | 4.6 s (8.4) | 0.094 s | 1.000 / 0.980 | 34.0 MB |
| **IVF_PQ 1 m96, refine 4** | 59.5 (34.0) | **328** | **22** | **3.5 s (5.8)** | **0.089 s** | **1.000 / 0.987** | 34.0 MB |

With `LANCE_CPU_THREADS=32`:

| variant | MB | requests | waves | wall |
|---|---|---|---|---|
| IVF_PQ 512 m96, np 128, refine 4 | 39.9 | 772 | 31 | 3.9 s |
| IVF_PQ 256 m96, np 96, refine 4 | 40.7 | 682 | 28 | 3.8 s |
| IVF_PQ 64 m96, np 32, refine 4 | 44.4 | 550 | 26 | 3.2 s |
| IVF_FLAT 512, np 128 (16 threads; run before the proxy backlog fix, one 30.9 s outlier) | 197.9 | 567 | 36 | 4.8 s |

Recall sweeps (tie-aware, 30 queries, production data):

- **Refine is mandatory for every quantized index.**
  - IVF_PQ 512 m96, np 128, no refine: 0.700 / 0.793.
  - IVF_SQ 512, no refine: 0.927 / 0.960.
  - IVF_RQ 512 needs refine 12 to hold hybrid recall. Refine 2–8 gave 0.85–0.92 (plain set overlap).
- **Probe fraction needed for recall@5 ≥ 0.97 on these embeddings** (SQ, refine 2): 25% at 512/128, 37.5% at 128/48 and 256/96, and still only 0.867 at 16/6 (37.5%). That makes a full scan of a small compressed index competitive.
- **IVF_PQ 512 m24** reaches only 0.93 / 0.91 even with refine 12.
- **IVF_HNSW_SQ** (16 and 64 partitions, 148 MB index): at most 0.80 / 0.88 at 4–8 probes with ef 150 and refine 2. It also reads whole partitions. Rejected.

Other facts:

- **IVF centroids vary between builds.** Comparing quantizers fairly required reusing the production centroids: PQ m192 with fresh centroids scored worse than m96 because of the centroids, not the quantizer.
- **`LANCE_CPU_THREADS` trade-off.** It cuts waves for indexes with many partitions, but splits partition reads into more requests (PQ 512: 592 → 772). With 1 partition it does not matter.
- **Go binding support.** lancedb-go builds and queries IVF_FLAT, IVF_PQ and IVF_HNSW_*. IVF_SQ and IVF_RQ need a Go enum value; the Rust side already maps `ivf_sq` and `ivf_rq`. IVF_PQ needs no binding change.
- **Build times on home-satan** (niced, 2 cores unless noted):

  | index | build time |
  |---|---|
  | IVF_FLAT 512, fixed centroids | 11 s |
  | IVF_FLAT 512, trained | ~60 s (per the optimize script comment) |
  | IVF_SQ | 13–46 s |
  | IVF_RQ | 9–37 s |
  | IVF_PQ m96 | 4.6–5.6 min |
  | IVF_PQ 1 m96 through the Go path, 4 cores niced | 3 min 41 s – 3 min 43 s |

- **No per-run retrain needed.** With the IVF_PQ 1-partition index, `pi-memoryd --optimize` folds new rows into the existing index in 19 s for 15 tail fragments, leaving 1 index and 0 unindexed rows. The optimizer no longer needs to drop and rebuild the vector index each run.

### 1.4 Two-step row fetch and fetchK

- **Single select today.** It reads 15 columns for up to 100 candidates (50 per channel) and keeps 5. On the production layout, two-step saves almost nothing (text: 335 → 300 requests, 38.9 → 37.8 MB, same waves), because the cost is the page dictionaries.
- **After the re-encode (1-partition PQ index):**

  | query | single select | two-step (phase 1 = `parent_id` + `_rowid`) |
  |---|---|---|
  | hybrid | 642 requests, 53.3 MB, 27 waves, 3.30 s | 379 requests, 47.7 MB, 27 waves, 3.42 s |
  | text only, interleaved A/B | 317 requests, 15.0 MB, 3.38 s | 228 requests, 12.8 MB, 3.64 s (+0.26 s, one extra round trip) |

  Phase 1 needs only `parent_id`; reading `id` too costs 47 more requests.
- **Phase 2 should use `_rowid IN (…)`, not `id IN (…)` through `id_btree`.** The btree adds `page_lookup` and `page_data` reads: 373 vs 300 requests, 36 vs 30 waves.
  - **Lance 10 bug:** a `_rowid IN` scan with a Limit applies the limit as a row range before the filter (`range_before=0..n`) and returns 0 rows. Phase 2 must not set a Limit.
  - **Concurrent writes:** row ids are row addresses, so a merge-insert between the two phases can drop a hit. The patch falls back to the single select when phase 2 returns fewer rows than phase 1 kept.
- **fetchK must stay at 50.** Measured against the fetchK-50 exact pipeline:

  | fetchK | hybrid recall@5 (prod index) | hybrid recall@5 (PQ 1 m96) | vector-only |
  |---|---|---|---|
  | 30 | 0.733 | 0.74 | unchanged (1.0) |
  | 20 | 0.680 | 0.69 | unchanged (1.0) |

### 1.5 FTS cold cost

- **Size.** 12–14 MB, 88–141 requests and about 13 sequential waves per cold text or hybrid query. Once the vector index is small, this is the critical path.
  - Token dictionary, read whole: 1.45 MB.
  - Doc lengths: 1.3–2.5 MB.
  - Posting lists for the 3–4 query terms: 7.5–9.9 MB. The synthetic queries use common words ("build", "cache", "index").
- **Why it is sequential.** Lance 10 loads tokens, then postings, then docs, each as footer → metadata → data. Concurrent identical posting-list reads also happen (some ranges fetched 3×).
- **Lance version, same index, Python harness, text, 5 queries:**

  | client | FTS MB | FTS requests | waves |
  |---|---|---|---|
  | pylance 10.0.0 | 12.7 | 88 | 22 |
  | pylance 13.0.0 | 7.9 | 54 | 21 |
  | pylance 13.0.0, index rebuilt with 13 | 8.3 | 51 | 21 |

  Using this in the Go path needs a lancedb-go Rust lib rebuild on a newer lancedb.
- **Partition count doesn't matter.** The 2-partition index (what `--optimize` produces) costs 12.4 MB / 87 requests; a fresh 1-partition rebuild costs 12.1 MB / 88 requests.
- **Production's doubled token and doc reads (3.0 / 2.5 MB vs 1.45 / 1.26)** come from the unindexed tail (1.6), not the partition count.
- **Analyzer differences change rankings.** A fresh FTS rebuild changes BM25 rankings slightly: text top-5 agreement between the production index and the rebuild is 0.92.

### 1.6 Unindexed tail

Measured on the re-encoded copy, 15 tail fragments (150 rows) against none:

| mode | requests | MB | waves |
|---|---|---|---|
| hybrid, no tail → 15 fragments | 511 → 594 (+83, +16%) | 191.7 → 194.6 (+2.9) | 84 → 86 |
| text, no tail → 15 fragments | 220 → 248 (+28) | 14.0 → 16.2 (+2.1) | 28 → 28 |

- **Per fragment:** about 5.5 extra requests per cold hybrid query. Both channels flat-scan every tail file, and the FTS index is loaded a second time.
- **In production:** the journal for 10-06 to 10-08 shows 31–58 fragments accumulating between optimizer runs (4-hourly, threshold 20). That is 170–320 extra requests per cold hybrid query at the peak, close to the whole post-change budget of ~400.
- **Levers (not measured on R2):**
  - Run the optimizer more often. With the PQ index there is no retrain, but every run still rewrites the 740 MB data file, invalidates the disk cache's data blocks and restarts the daemon.
  - Raise `PI_MEMORYD_RAW_WRITE_INTERVAL` (5m → 15m) [INFERENCE: effect not measured].

### 1.7 Minimum number of waves

With the recommended config, hybrid takes 28 waves:

1. list `__manifest/_versions/` (lancedb 0.37 namespace probe);
2. list `chunks.lance/_versions/`;
3. read the manifest;
4. read FTS metadata, then tokens (2);
5. read vector `index.idx` (read three times in a row);
6. read the PQ aux file (2–4), overlapping with the FTS postings and docs (~10);
7. read rows (8: footer, column metadata, page metadata, rows; for phase 1 and again for phase 2).

- **Floor with Lance 10:** about 22 (3 open + ~13 FTS + ~6 rows).
- **Simulated in-process single-flight cache of identical immutable reads** (in the proxy): hybrid 28 → 24 waves, 407 → 351 requests, 3.5 → 3.0 s; text 228 → 176 requests, 27 → 24 waves.
  - The disk cache already behaves this way when its directory is set, but its first read starts `warm_root` (a full table copy). A "cache on, warm-up off" switch in `docker/lancedb-go-disk-cache.rs` would be needed. That is a Rust lib rebuild; not built.
- **Not collapsible from config:** the namespace probe, list + manifest (version resolution), and Lance 10's sequential FTS loads.
- **Already prefetched:** pi-memoryd's `warm()` loads the FTS index during startup, which is why the first hybrid query after a no-cache restart needs only 14 waves.

### 1.8 End-to-end results

Hybrid, fully cold, 10 queries:

| # | configuration | cold MB | cold requests | cold waves | cold wall p50 (max) | warm: R2 / local | recall@5 vec / hyb | vector index |
|---|---|---|---|---|---|---|---|---|
| B0 | production today: IVF_FLAT 512, np 128, dictionary pages, single select | 218.7 | 779 | 83 | 11.2 s (12.1) | 1.68 s / 0.13 s | 0.973 / 0.977 | 508.6 MB |
| B1 | B0 + re-encoded data | 195.3 | 706 | 83 | 11.2 s (11.9) | – / 0.082 s | 0.973 / 0.993¹ | 507.7 MB |
| B2 | B1 + two-step fetch | 191.0 | 538 | 83 | 11.9 s (13.4) | – / 0.073 s | = B1 | 507.7 MB |
| B3 | B2 + IVF_PQ 512 m96, np 128, refine 4, `LANCE_CPU_THREADS=32` | **30.0** | 838 | 32 | 3.8 s (4.1) | – / 0.055 s | 0.973 / 0.993¹ | 33.7 MB |
| B4 | B2 + IVF_PQ 64 m96, np 32, refine 4, 32 threads | 34.8 | 631 | 30 | 3.5 s (3.7) | – / 0.084 s | 1.000 / 0.980 | 34.1 MB |
| B4′ | B4 with default threads | 34.4 | 472 | 38 | 5.2 s (6.2) | | | |
| **B5 ★** | B2 + **IVF_PQ 1 m96, refine 4**, phase 1 reads `parent_id` only | 48.6 | **407** | **28** | **3.5 s (4.0)** | **0.82 s / 0.068 s** | **1.000 / 0.987** | 34.0 MB |
| B6 | B5 + simulated single-flight read cache | 47.2 | 351 | 24 | 3.0 s (3.7) | | = B5 | |

¹ Scored against the exact pipeline on its own (single-partition) FTS index.

B5 by mode, compared with B0:

| mode | requests | MB | waves | wall p50 | CPU |
|---|---|---|---|---|---|
| vector | 515 → 288 | 202.2 → 36.4 | 84 → 23 | 12.0 → 3.5 s (max 8.0, one 1.1 s R2 tail) | 0.56 → 0.21 s |
| text | 335 → 228 | 38.9 → 12.8 | 25 → 27 | 3.3 → 3.6 s (interleaved A/B) | 0.26 → 0.18 s |
| hybrid | 779 → 407 | 218.7 → 48.6 | 83 → 28 | 11.2 → 3.5 s | 0.71 → 0.29 s |

How B5 was produced and checked:

- **Built the way the rollout would build it:** a pylance metadata-only commit on a production copy, then the patched `pi-memoryd --drop-vector-index`, `--optimize` and `--create-vector-index`.
- **Served by the patched daemon over HTTP:** recall 1.000 / 1.000 on all 30 queries; all 14 metadata fields present; local warm p50 0.041 s.

Daemon restart with no disk cache, B0 → B5:

- startup: 5.3 → 4.8 s, 188 → 133 requests, 29 → 5.4 MB;
- first hybrid query: 11.1 → 2.0 s, 602 → 256 requests, 190 → 40 MB, 78 → 14 waves;
- next queries: 3.4–6.5 s → about 1 s.

### 1.9 Recommendation and what is left

**Recommended: B5.**

- re-encoded data;
- a single-partition IVF_PQ with 96 sub-vectors, refine factor 4;
- two-step row fetch through `_rowid`;
- fetchK stays 50.

Why B5:

- **Fewest requests and waves of every config that holds recall.** Hybrid: −48% requests, −66% waves, 11.2 → 3.5 s.
- **78% fewer bytes.**
- **Higher recall.**
- **Nothing to tune:** no nprobes or thread settings, and no centroid drift, so no per-optimize retrain.
- **Ceiling:** about 103 B per chunk read cold (34 MB at 329k chunks). Around 1M chunks, switch to B4.

The code diff (`lance.go`, `main.go`), the one-off metadata script, the `home-satan` changes, and the rollout and rollback steps are in the task file's Results section.

What is left after B5:

| idea | expected effect | cost | evidence |
|---|---|---|---|
| keep the unindexed tail small | removes up to 170–320 requests per cold query at peak | config only | per-fragment cost measured; the fix not measured |
| single-flight read cache without warm-up | 28 → 24 waves, 3.5 → 3.0 s | Rust lib rebuild | simulated |
| prefetch all small index files in one round trip (Quickwit's hotcache idea) | ~8–10 waves, ~1 s cold | Rust lib rebuild | estimate only |
| Lance 13 | FTS −38% bytes, −1 wave | Rust lib rebuild on newer lancedb | measured in Python |

**Warm searches are already fast.** With the disk cache, a warm search makes 0 R2 requests and takes 0.06–0.09 s (CHANGELOG). The cold path matters after restarts and optimizer runs, a few times a day.

---

## 2. Comparison with other engines

### 2.1 Landscape: who solves cold search on object storage

| engine | object storage | vector + BM25 | cold path | self-host |
|---|---|---|---|---|
| **turbopuffer** | source of truth | yes | storage engine built to a round-trip budget: "3-4 required roundtrips for a cold query often take as little as ~400ms"; cold p50 874 ms for 1M docs; cached 14 ms ([architecture](https://turbopuffer.com/docs/architecture)) | no |
| **LanceDB OSS** | yes | yes | "no built-in cache"; "500–1000 ms" from object storage ([Enterprise vs OSS](https://docs.lancedb.com/enterprise/index.md)); we measured 3.5–11 s fully cold | yes (what we run) |
| LanceDB Enterprise | yes | yes | distributed NVMe cache, "50–200 ms" after the cache fills | no |
| Distributed Chroma | WAL + indexes on object storage, SSD cache ([docs](https://docs.trychroma.com/reference/architecture/distributed)) | yes (SPANN vector index) | cache-based; "can add cold-start latency"; no published cold round trips | Apache-2.0 Helm chart, not marked official; Kubernetes, several services |
| Milvus 2.6 tiered storage | lazy-loads from object storage | yes | first query fetches from object storage; "warm up" preloads ([docs](https://milvus.io/docs/v2.6.x/warm-up.md)) | yes, heavy cluster |
| **Quickwit** | yes | **text only** (Tantivy) | per-split `hotcache`: "opening a split on Amazon S3 only takes 60ms" ([architecture](https://quickwit.io/docs/overview/architecture)) | Apache-2.0 (now owned by Datadog) |
| **HelixDB** | LSM storage engine on object storage | yes (graph + HNSW + BM25) | cache-based; docs: "every graph hop that misses [the cache] would otherwise wait on its own S3 request" ([local server](https://docs.helix-db.com/database/helix-db/start-here/local-development/local-server)) | Apache-2.0, single node; distributed only in Helix Cloud |

turbopuffer's design ([ANN v3](https://turbopuffer.com/blog/ann-v3)):

- **Navigation is a hierarchical tree of centroids (SPFresh), not a graph.** This "bounds the number of round-trips to object storage to the height of the SPFresh tree".
- **Vectors are compressed with RaBitQ binary quantization** (1 bit per dimension, 16–32× smaller). Under 1% of candidates are re-checked against their full vectors.
- **Our B5 follows the same "compress, then re-check" idea.** It lacks the tree; at 330k chunks a full scan of the compressed index is still cheap.

Why other engines don't do this [INFERENCE: opinion]:

- Most were designed for local disk or RAM, with object storage added later behind a cache.
- Round-trip minimization needs one planner that controls the file layout end to end. In layered stacks each file format pays its own footer → metadata → data reads.
- Most workloads are warm. turbopuffer's customers have millions of mostly cold namespaces, so cold latency was their core problem.
- Open-source projects monetize the cache tier: Enterprise, Cloud.

### 2.2 HelixDB v0.0.12, measured

- **Setup.**
  - Docker, 2 CPUs / 2.5 GB, S3 mode on the scratch prefix through the write-restricted proxy.
  - R2 enforces the `If-None-Match` / `If-Match` conditional writes HelixDB needs (verified: 412 on a violated condition).
  - Nodes `Chunk {chunk_id, parent_id, session_id, content, embedding f32[384]}`; vector index Euclidean 384-dim; text index on `content`. The SDK exposes no analyzer or HNSW knobs.
- **Load.** 329,654 chunks in 906 s (400 per write batch).
- **Vector index build: about 4 h 15 min** from creation to activation.
  - The scan phase slowed from ~4k to ~0.7k entities per minute.
  - By the 230k-entity mark it had written 1.6 GB in 19.7 M key-value operations into the LSM on R2.
  - Then about 10 min in validation.
- **Text index: never activated.**
  - **First build:** my proxy restart (to allow bulk deletes) made one blob PUT fail after 10 fast retries. HelixDB's retry never re-uploaded that blob, and validation failed with `invariant_violation` / `blob_missing_or_size_mismatch`.
  - **Clean rebuild** (after an abort that took ~30 min to delete 1M+ metadata entries): no write errors and only one throttled list request in the log, yet it blocked in the same validation step.
  - **Explicit retry:** re-blocked within 30 s.
  - The cause is not determined: a HelixDB bug or an R2 incompatibility.
- **Cold vector query:** fresh container, empty cache volume, `HELIX_DISK_CACHE_WARM=off`, 10 queries, k=50, projecting `chunk_id, parent_id, session_id, content`.

| | requests | MB | waves | wall |
|---|---|---|---|---|
| server startup (to readiness) | 114 | 174 | 46 | 11.5 s |
| first vector query | 260 | 828 | 97 | 16.4 s |
| first vector query, cache budget 1 GiB (2 MiB parts) | 543–700 | 931–1195 | 197–287 | 27.6–39.2 s |
| warm, other queries (same process) | 9.5 per query | 23 per query | | 0.73 s p50 |

- **Why the bytes are so high.** The S3 object cache fetches whole 4 MiB file parts: 187 of a typical query's 260 requests were 4 MiB. The cache can't be disabled in S3 mode.
- **Why the waves are so high.** 97 waves fits one dependent read per uncached graph hop.
- **Background noise.** The writer reads about 3 requests per second while idle, so ~50 of the 260 query-window requests are background.
- **Recall@5:** 0.80 against the numpy truth on the v12854 snapshot.

### 2.3 Quickwit 0.9.1, measured

- **Setup.**
  - Docker, 1 CPU / 1.2 GB.
  - File-backed metastore and splits on the scratch prefix.
  - Index `chunks`: `chunk_id`, `parent_id`, `session_id` (raw) and `content` (tokenizer `default`, record freq, fieldnorms). 0.9.1 has no built-in `en_stem`.
- **Indexing:** 329,654 docs in 27 s, one 116 MB split.
  - A first attempt through the proxy failed: single 100+ MB `PUT`s timed out in the Python proxy, and `local-ingest` retried by re-reading the input file 3 times (988,962 docs counted, nothing published). It was wiped and redone writing directly to the scratch prefix; queries then went through the proxy.
- **Cold query:** `quickwit tool local-search` in a fresh container per query; query terms OR-joined to match LanceDB's OR semantics; sorted by `_score`.

| | requests | MB | waves | R2 span | process wall |
|---|---|---|---|---|---|
| 50 hits | 48 | 7.6 | 8 | 1.2 s | 1.8 s (includes container start) |
| 5 hits | 22 | 1.7 | 7 | 0.9 s | 1.5 s |
| long-running server, first query | 54 | 10.5 | | | 0.89 s |
| long-running server, warm, other queries (split cache off) | 40 per query | 7.0 per query | | | 0.57 s p50 |

- **Cold read sequence:**
  1. `HEAD` + `GET manifest.json`, then `GET metastore.json`: 3 waves, because the metastore is a file on S3. Postgres or a running server avoids them.
  2. One 78 KB read of the split footer + hotcache.
  3. Term-dictionary and posting slices.
  4. Parallel doc-store block reads (~0.3 MB each) for the returned hits.
- **Ranking differs from LanceDB's.** Top-5 parent agreement with LanceDB's BM25 is 0.44: different analyzer, no stemming, no stop words. This is a ranking difference, not a recall loss.
- **No vector search.**

### 2.4 Side by side (fully cold, same snapshot, same proxy and metric)

| engine, mode | cold requests | cold MB | cold waves | cold wall | warm (other queries) | recall@5 | index build |
|---|---|---|---|---|---|---|---|
| LanceDB B5: vector | 288 | 36.4 | 23 | 3.5 s | 0.78 s (no disk cache) | 1.000 | 3 min 40 s |
| LanceDB B5: text | 228 | 12.8 | 27 | 3.6 s | 0.67 s (no disk cache) | – | ~20 s |
| LanceDB B5: hybrid | 407 | 48.6 | 28 | 3.5 s | 0.82 s (no disk cache) | 1.000 / 0.987 | |
| LanceDB B0 (production today): vector | 515 | 202.2 | 84 | 12.0 s | 1.57 s (no disk cache) | 0.973 | |
| HelixDB: vector (+ startup 114 / 174 / 46 / 11.5 s) | 260 | 828 | 97 | 16.4 s | 0.73 s | 0.80 | load 15 min + index ~4 h 15 min |
| HelixDB: text | not measurable (index build blocked) | | | | | | |
| Quickwit: text, 50 hits | 48 | 7.6 | 8 | 1.2 s R2 span | 0.57 s (split cache off) | – | 27 s |
| Quickwit: text, 5 hits | 22 | 1.7 | 7 | 0.9 s R2 span | | | |

### 2.5 Conclusions

- **Text search on object storage in ~8 round trips (~1 s cold from home-satan) is solved.** Quickwit does it with a per-split hotcache: all the metadata a query needs to open a split, in one file, read once.
- **Correct vector search exists; few-round-trip cold vector search does not, among self-hostable engines.**
  - LanceDB B5 is exact (1.000) at ~23 vector waves.
  - HelixDB's graph index is both slower and less accurate.
  - turbopuffer's 3–4 round trips come from a purpose-built centroid tree with binary quantization, and it isn't self-hostable.
- **HelixDB is not a fit for cold or R2-attached search today.** It is slower than LanceDB on every cold metric, has lower recall, takes hours to build the index, and could not build a text index on R2.
- **A split stack (Quickwit for BM25, LanceDB IVF_PQ for vectors, fused in Go)** would put the cold hybrid floor at about LanceDB's ~23 vector waves. It isn't worth a second engine unless the vector side also gets the hotcache treatment (1.9).
- **Next step for pi-memoryd:** deploy B5. Its warm path is already fast, and the cold windows are a few per day.

## 3. Round-trip experiments with the disk cache (2026-10-10)

Sections 1–2 measured every query fully cold, one fresh process per query, with no disk cache. Production runs with `LANCE_DISK_CACHE_DIR=/var/cache/pi-memoryd` (6 GB), and the daemon stays up. This section measures what the user actually sees: a daemon start, then queries, through the same logging proxy, on the same B5 scratch copy (prod v12926 converted: encoding metadata, `--optimize`, IVF_PQ 1×96).

Two experiments:

- **E1**: the shipped disk-cache wrapper, as is.
- **E2**: a changed wrapper (`docker/lancedb-go-disk-cache.rs`, Rust lib rebuilt): first-touch promotion and single-flight. Diff at the end of this section.

Method: `e1.sh` starts the daemon (`--raw-url ""`, scratch storage URL, proxy endpoint), waits for `pi-memoryd listening`, runs 4–10 hybrid queries (`/v1/memory/query`, `limit 5`, bge-embedded with the query prefix), optionally waits for the wrapper's `disk cache warmed` line and runs the set again. "Waves" are the dependency depth of R2 requests inside each window (a request that starts after another ended counts as dependent, so this is an upper bound). Startup includes the daemon's own warm-up scan and FTS query. Wall times include the proxy; the first E1 runs overlapped a 2-CPU Rust build and are marked.

### 3.1 E1: what the existing cache already does

B5 layout, B5 binary (published lib):

| scenario | startup s (waves) | query 1 s (waves / requests) | query 2 | queries 3–4 | everything local after |
|---|---|---|---|---|---|
| no cache dir | 3.81 (27) | 3.04 (17 / 278) | 0.78 | 0.69 / 0.67 | never: every query reads its rows from R2, 150–250 requests, 0.7–1.3 s |
| no cache dir, under build load | 7.81 (30) | 5.14 (18 / 286) | 1.34 | 1.39 / 1.68 | – |
| empty cache dir | 1.91 (13) | 2.37 (11 / 174) | 1.42 | 0.05 / 0.06 | 5.2 s (830 MB, 12 files, with data) |
| empty cache dir, under build load | 2.59 (18) | 2.18 (11 / 146) | 0.78 | 0.41 / 0.41 | 6.6 s |
| cache 1 GB (indexes only, `with_data=false`) | 1.91 | 1.20 | 0.86 | 0.55 / 0.45 | 1.4 s for 94 MB of indexes; rows stay remote, 0.45–0.6 s per query |
| restart, valid cache | 0.87 (7) | 0.20 (1) | 0.14 | 0.06 / 0.06 | immediately |
| `--optimize` that rewrote the data file (2→1 fragments), then restart | 2.80 (16) | 1.52 (5 / 55) | 0.09 | 0.06 / 0.06 | 4.1 s (680 MB) |

Production layout (IVF_FLAT 512, np 128, dictionary pages), prod binary 0.2.1:

| scenario | startup s (waves) | query 1 s (waves / requests) | query 2 | queries 3–4 | everything local after |
|---|---|---|---|---|---|
| no cache dir | 4.63 (31) | 11.19 (80 / 601) | 7.18 | 6.19 / 4.72 | never |
| empty cache dir | 3.95 (25) | 2.08 (9 / 181) | 1.40 | 1.40 / 0.79 | 10.4 s (1.29 GB, 27 files) |

Findings:

- **With the cache dir, after the warm-up a query makes zero R2 requests.** The table handle does not re-list versions per query. The 7 startup waves of a restart with a valid cache are the uncacheable part: 5 listings and 2 manifests.
- **The warm-up copies index files first, so the daemon's own FTS warm-up query and the first user query already find most index blocks local or in flight.** That is why the first query drops from 11.2 s to 2.1 s on the production layout with nothing but the cache dir. The 11 s figure only happens without the cache dir.
- **The cold window in production is therefore not "every query"; it is startup + first query after a restart, about 6 s on the current layout, and all-local after ~10 s.** Production's `warmLedger` fires one hybrid query right after start, so a user usually never sees it; what they see is the optimize downtime every 4 h (drop index → optimize → create index, minutes) plus that window.
- **B5 on top of the cache**: startup 1.9 s instead of 4.0, first query 2.4 s instead of 2.1 (noise), all-local after 5.2 s instead of 10.4 because the warm volume is 830 MB instead of 1.29 GB. After an optimize that only touches the index (no fragments to compact), the re-warm is 34 MB.
- **Rows must be in the cache too.** With a 1 GB cap the wrapper skips the data file (`with_data=false`) and every query pays 0.45–0.6 s for 100–250 row reads. Production's 6 GB cap is right.
- **R2 bulk throughput from home-satan is about 125 MB/s** (830 MB in 5.2–6.6 s with 8 × 16 MiB in flight). Bytes are cheap; dependent round trips are what cost time.
- **Optimize through the proxy fails** with `InvalidPart: All non-trailing parts must have the same length`: under the proxy's backpressure Lance's writer emits 5 MiB and 10 MiB parts (101 × 5 MiB + 19 × 10 MiB for the 725 MB file), and R2 requires uniform parts. Direct to R2 the parts are uniform and prod's optimize completes every 4 h. Bench artifact; the scratch optimizes ran directly against R2.

### 3.2 E2: first-touch promotion and single-flight in the wrapper

The change, all inside `docker/lancedb-go-disk-cache.rs`:

1. **Promotion.** Lance's first read of any file is its 64 KiB footer, so that request's end offset is the file size. When the footer blocks are not cached, the wrapper widens the fetch: index files up to `LANCE_DISK_CACHE_PROMOTE_BYTES` (default 64 MiB) are fetched whole, data files get their last 8 MiB (footer, column metadata, page tables). The dependent reads that Lance issues next are cache hits. This is Quickwit's hotcache without a format change: the PQ index (34 MB), the FTS tokens, docs and postings (1.4 + 2.2 + 42 MB) each become one request.
2. **Single-flight.** Concurrent readers of the same blocks (the vector and text channels open the same data file at the same time; three reads of `index.idx` in a row) share one in-flight fetch, keyed by (object, block). Warm-up chunks are not published, so a query's 256 KiB row read never queues behind a 16 MiB copy.
3. `LANCE_DISK_CACHE_WARM=0` switches the background copy off, for measurement.

Results, B5 layout, E2 binary (same Go code as B5, rebuilt lib):

| scenario | startup s (waves) | query 1 s (waves / requests) | query 2 | queries 3–4 | everything local after |
|---|---|---|---|---|---|
| empty cache dir, warm on (3 runs) | 1.91 / 2.16 / 1.93 (13 / 14 / 13) | 1.35 / 1.65 / 1.41 (8 / 9 / 10) | 0.72 / 0.58 / 0.65 | 0.36 / 0.35 / 0.38 | 4.4 / 5.7 / 4.8 s |
| empty cache dir, warm off (promotion only) | 1.92 (13, 17 requests, 55 MB) | 1.75 (8 / 161: 159 row reads + `index.idx` + `auxiliary.idx`, one request each) | 0.71 | 0.50 / 0.42 | never; steady 0.45 s |
| warm off, promotion off (single-flight only) | 2.96 (21) | 2.15 (13 / 193) | 0.78 | 0.45 / 0.44 | – |
| warm off, promotion 16 MiB (postings not promoted) | 2.54 (16) | 1.82 (13) | 0.72 | 0.50 / 0.46 | – |
| `--optimize` touching only the index, then restart | 0.86 (7) | 1.06 (3 / 8) | 0.11 | 0.07 / 0.06 | 1.2 s (34 MB) |

Production layout with the E2 lib (unpatched Go code, np 128): startup 3.21 s (16 waves), first query 1.41 s (9 / 151), then 0.81 / 0.46 / 0.80 s, all-local after 8.6 s (1.25 GB). Against 3.95 / 2.08 / 1.40 / 10.4 s with the shipped lib.

Findings:

- **First query 2.4 s → 1.4 s on B5 (11 → 8 waves), 2.1 → 1.4 s on the production layout.** Startup is unchanged at 13 waves with the warm on, because its remaining waves are the 5 listings and 2 manifests plus the daemon's FTS warm-up query racing the copy.
- **Promotion is the part that matters; single-flight alone saves little** (21 → 13 startup waves and 13 → 8 first-query waves come from promotion; single-flight only: 2.96 / 2.15 s).
- **64 MiB is the right threshold**: at 16 MiB the 42 MB postings file is read in 13 dependent pieces again.
- **The first query's remaining 8 waves are row reads**: ~100 candidate rows for `parent_id` (phase 1, both channels) and 5 full rows (phase 2), scattered across the 740 MB file in 256 KiB blocks, 130–160 requests. The cache layer cannot collapse those; only the warm-up does, 4–6 s after start.
- **The widen rule must key on the footer blocks, not on "size unknown".** The first E2 build keyed on unknown size; when the warm-up had already recorded the file size the promotion did not fire and the post-optimize first query read the new PQ index in 10 pieces (1.40 s, 6 waves). Keying on "footer read whose blocks are uncached" fixed it (1.06 s, 3 waves).

### 3.3 What this changes in the recommendation

- The shipped disk cache already removes most of the cold cost in production. The remaining user-visible window is ~6 s after each restart, and the optimize downtime around it.
- B5 is still the larger win: it halves the warm volume, removes the index rebuild from the optimize cycle (34 MB re-warm instead of 508 MB + 740 MB), and takes the no-cache fallback from 11 s to 3 s.
- E2 is worth about 1 s of a 4 s window, six times a day. Ship it when the Rust lib is rebuilt for another reason (Lance 13, for example); it is not worth a release on its own. The diff is 203 changed lines, no new dependencies, and leaves behaviour unchanged with `LANCE_DISK_CACHE_PROMOTE_BYTES=0`.
- The one setting that matters today is the cache cap: it must stay above indexes + data (`with_data=true`), or every query pays ~0.5 s for rows.

### 3.4 E2 diff

Kept at `~/ledger-bench/e2-disk-cache.diff` on home-satan together with the rebuilt lib (`~/ledger-bench/lance1/liblancedb_go.a`, built from the `lance-artifact` stage with the modified wrapper; a control build of the unmodified wrapper took 1 h 45 min on a 2-CPU builder, the incremental rebuild 2 min). Summary of the change:

```text
const PROMOTE_BYTES_ENV = "LANCE_DISK_CACHE_PROMOTE_BYTES"   (default 64 MiB; 0 disables)
const DATA_TAIL_BYTES   = 8 MiB
const FOOTER_READ_BYTES = 64 KiB
const WARM_ENV          = "LANCE_DISK_CACHE_WARM"            ("0" disables the background copy)

DiskRangeCache { +promote_bytes, +inflight: Mutex<HashMap<(object_hash, block), Shared<fetch>>> }

fn widen(location, cached_size, range, lo, hi):
    footer = range.len == 64 KiB && (size unknown || range.end == size)
    if !footer || promote == 0: (lo, hi)
    "/_indices/" && range.end <= promote  -> (0, hi)          whole index file
    "/data/"                              -> (end-8MiB, hi)   file tail

fn fetch_span(location, lo..=hi) -> (meta, fetched_range, bytes)   one GET, caches every block

fn get_blocks(location, range, options, share):
    cached blocks as before
    (lo, hi) = widen(...)
    if every missing block has an in-flight fetch: await those
    else: start fetch_span as a Shared future; if share, publish it per block; await; unpublish
    anything still missing: fetch directly (surfaces the real error)

warm_chunk -> get_blocks(..., share=false); get_opts/get_ranges -> share=true
```

## Caveats

- **Query set.** The 30 synthetic queries contain very common terms, so posting-list reads are probably above typical. They were embedded without the bge query prefix that production uses. That affects how many IVF probes recall needs, not the bytes, requests or waves of a given configuration. B5 scans every vector, so its recall doesn't depend on clustering.
- **No relevance judgements.** All recall numbers measure how faithfully an index reproduces exact search, not answer quality. Text-only and vector-only results each overlap hybrid's top 5 by 0.39.
- **Wall times include the proxy.** R2 round trips from home-satan are 0.08–0.10 s; in-region S3 is ~63 ms (turbopuffer's figure).
- **CPU caps on the other engines.** HelixDB and Quickwit ran with capped CPUs; HelixDB's index build time is partly due to its 2-CPU cap.
- **Throwaway tooling.** The section 1–2 tooling (`coldq`, variant builders, logs) was removed after that round; the raw per-run numbers come from those logs. The section 3 tooling, the rebuilt lib and the E2 diff are kept in `~/ledger-bench/` on home-satan (`proxy.py`, `e1.sh`, `bench.py`, `waves.py`, `logs/requests.jsonl`).

