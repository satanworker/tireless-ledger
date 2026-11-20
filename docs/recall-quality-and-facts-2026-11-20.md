# Recall quality, access and facts (2026-11-20)

Findings and the agreed plan from the home-satan session that designed the
personal Telegram agent (`home-satan/docs/telegram-agent-design.md`). Task
checklist: `tasks/🧭-recall-quality-access-facts.md`.

tireless stays the shared memory layer for every deployment. Agents reach it
only through the `tireless` CLI and the `tireless-recall` skill, so anything
below that contract can change without touching them.

## 1. Filtered hybrid queries return 502

Seen on home-satan only, 14 times in the pi-memoryd journal since Oct 27:

```
failed to execute select query: Failed to execute query: Invalid input,
expected column _distance not found in rank. found columns ["message_id", "_score", "_rowid"]
```

| Query (home-satan, pi-memoryd `4f91861`) | Result |
|---|---|
| `tireless query -project grokbot-legal tax` | 502 |
| `tireless query -project grokbot-cars-boy tax` | 502 |
| same with `-embed=false` (BM25 only) | works |
| `-source grokbot`, `-source omp`, `-project japan` | work |
| imp-home, `-project grokbot-legal` | 0 hits, no error (no Grok Bot rows there) |

- Cause, checked on 2026-11-20 against the live store with a read-only Go probe:
  the IVF-PQ search with `project = 'grokbot-legal'` returns 0 of the 196 matching
  chunks for a query far from them (prefilter, fast search on or off, nprobes 1,
  refine 4 or none); the same search with `bypass_vector_index` returns 10. None of
  those chunks has a null vector, and a query with a chunk's own vector finds it
  through the index, so the rows are indexed. `source = 'grokbot'` returns 1 row.
  Lance's native hybrid RRF then fails on the empty vector side. A 20k-row local
  store did not reproduce the index miss.
- home-satan runs the IVF-PQ index (`PI_MEMORYD_EXACT_VECTOR_SEARCH=false`, one
  partition, refine 4); imp-home's setting is unchecked.
- Still 502 on home-satan (`4f91861`) on 2026-11-20; `lance.go` is unchanged up to
  `84b9fd3`, so the flake switch alone would not have fixed it.
- Fixed in `lance.go`: vector and FTS run separately and are fused in Go by RRF on
  `_rowid` (`selectFused`), and a filtered IVF-PQ search returning fewer than K rows
  is retried flat (`selectSearch`). `TestLanceFilteredSearchSurvivesEmptyIndexSide`
  models the observed table (cgo build only).

## 2. Access: Tailscale is the authentication

No tokens. Observed on home-satan (`tailscale debug netmap`, packet filter):
user-owned devices reach every port; `grok-box` (tagged) reaches tcp/22 only;
`imp-home` (tagged) reaches nothing. From home-satan, imp-home's tireless answers
on `:8090` (user-owned devices can read the Valantis store).

Remaining gaps, none needing auth code:

- Fixed 2026-11-20: `/v1/memory/ingest` accepted loopback. The personal agent
  (`omp-agent`, uid 950) shares home-satan and reaches `:8090` over loopback and
  over the host's own addresses (its nftables rule allows `fib daddr type local`
  port 8090), so a prompt-injected bot could plant records every agent later
  trusts. Nothing on home-satan ingests locally (it uploads to R2);
  `tireless sync/watch` from other devices ingests over the tailnet. pi-memoryd
  now answers 403 to ingest whose remote address is loopback or any address of
  its host (`refuseFromThisHost`, `main.go`).
- `grok-box` SSHes into home-satan as `satanworker1`, which can then read both
  stores. Closed by retiring grok-box (home-satan build order step 4).
- Stale user-owned nodes `cikyg6bh` and `hetzner-1` keep full access; remove them.
- imp-home `:8090` should be limited in the central tailnet policy to the
  devices that need Valantis recall.

If a non-owner caller ever needs to write, identify it with Tailscale WhoIs
through the local tailscaled, still without tokens.

## 3. Measure recall before changing ranking

Nothing measures recall quality today. Build an evaluation set: about 40 real
queries from the personal store, each with the message that should rank first.
Report top-1 and top-10 for the current hybrid ranking. Every ranking change
below is judged against it.

## 4. Optional rerank with Jev (TypeSafe)

TypeSafe's re-ranking cookbook
(https://docs.typesafe.ai/cookbooks/rerank_typesafe.md): a fast-search
shortlist of 30, one `Noul` question per query–candidate pair, sort by the
returned probability. On their legal-text set (CLERC) top-1 went 5% → 30%, top-10
38% → 65%. Those numbers are for a different corpus.

- `tireless query -rerank`, off by default, server-side; the TypeSafe key lives
  in pi-memoryd's environment, never with agents.
- If TypeSafe fails, return the normal ranking.
- Ship only if the evaluation set shows a gain worth the latency and cost
  (unmeasured: 30 parallel calls per query).

## 5. Facts

Agreed split: facts are curated statements of what is true now; tireless is the
history and the evidence. Facts start as files in each deployment (the personal
agent's `USER.md` and `skills/<name>/facts.md`), one per line:

```
- <fact> (as of YYYY-MM-DD; source: tireless <message id> | /mnt/r2/<doc>)
```

A fact is written only after the owner confirms it in Telegram; text from mail,
web pages, WhatsApp or other bots never becomes a fact. Changes replace the line;
git keeps history.

What tireless adds, after 1–2 weeks of real facts exist:

1. Index fact files read-only as `kind=fact`, so any agent can search them.
2. Citation check on write (TypeSafe citation-check cookbook,
   https://docs.typesafe.ai/cookbooks/citation_check.md): the source message
   must exist (plain lookup; missing = fabricated), then one `Choice`
   (supports / contradicts / says nothing) on the source. The verdict is shown
   on the approval button. Confidence < 0.8 goes to the owner.
3. A time filter on `/v1/memory/query` (`after_ts` / `before_ts` on search, not
   only on walks).
4. Nightly contradiction check: for each fact, candidates newer than its as-of
   date (tireless, deterministic), the same `Choice` per pair (Jev), contradictions
   at ≥ 0.8 become a "still true?" button, the rest a weekly list.

tireless returns candidates; judging and acting stay with the caller, so
pi-memoryd has no model dependency for facts.

Moving the write path into tireless (append-only fact events in R2, current set
derived) waits until it is needed; the line format above maps onto those events.

## 6. Housekeeping

- Deployments drift: home-satan `4f91861`, imp-home ≥ `d2ec366`. Bump the
  `tireless-ledger` input in both flakes together.
- PR #1 (index omp `/live` voice turns as user messages), open since Nov 15, merged
  on 2026-11-20 after a rebase onto main.
- `tasks/` holds August task files whose status no longer matches reality.
