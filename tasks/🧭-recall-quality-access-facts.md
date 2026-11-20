---
id: recall-quality-access-facts
title: Recall quality, access and facts
emoji: 🧭
status: pending
created: 2026-11-20T13:30:00.000Z
updated: 2026-11-20T15:40:00.000Z
---
Background and evidence: `docs/recall-quality-and-facts-2026-11-20.md`.

## Now
- [x] Re-test filtered hybrid queries on home-satan: still 502 on `4f91861`; `lance.go` is unchanged up to `84b9fd3`, so the switch alone would not fix it
- [x] Regression test (`TestLanceFilteredSearchSurvivesEmptyIndexSide`), vector and FTS fused in Go instead of Lance's native RRF, flat retry for an under-filled filtered IVF-PQ search
- [x] Refuse `/v1/memory/ingest` from the server's own host (loopback and its addresses); tailnet ingest still works (`TestIngestRefusedFromThisHost`)
- [x] Merge PR #1 (omp `/live` voice turns), rebased onto main
- [ ] Deploy to home-satan, re-run `tireless query -project grokbot-legal tax`

## After 1–2 weeks of personal-agent use
- [ ] Recall evaluation set: ~40 real queries with the expected top message; report top-1 / top-10
- [ ] `-rerank` with Jev behind a flag, server-side key, fallback to normal ranking; keep only if the eval set improves
- [ ] Time filter on `/v1/memory/query`
- [ ] Index deployment fact files read-only as `kind=fact`
- [ ] Citation check for proposed facts (source exists, then supports / contradicts / says nothing)
- [ ] Nightly contradiction check: candidates newer than each fact's as-of date, judged per pair

## Housekeeping
- [ ] Bump `tireless-ledger` in home-satan and imp-home together
- [ ] Close or update the August files in `tasks/`
- [ ] Remove stale tailnet nodes `cikyg6bh`, `hetzner-1`; limit imp-home `:8090` in the central policy
