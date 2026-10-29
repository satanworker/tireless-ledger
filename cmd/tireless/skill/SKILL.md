---
name: tireless-recall
description: Recall past decisions from agent sessions (omp, Pi, Codex, Claude, ...) and Telegram chats with the `tireless` CLI. Use when the user refers to earlier sessions or chats, past decisions, "what did we decide", or "last time".
---

# Tireless recall

Search past agent sessions and Telegram chats to answer the user's surrounding message. The user's text is the task, not the search query: work out what past context it needs, then search for that.

## Commands

```sh
tireless stores                                   # configured stores (e.g. personal, valantis)
tireless query -limit 5 [-store NAME] [-project P] [-source S] [-kind K] [-container ID] <words…>
tireless container -store NAME -after-ts TS -limit 8 <container_id>
tireless sessions [-store NAME] [-since 24h] [-until …] [-project P] [-source S] [-host H]
```

- `query` searches every configured store at once; each hit shows its `store`, `container_id` and `ts`.
- `-source`: `omp`, `pi`, `codex`, `claude`, `grok`, `grokbot`, `telegram`. `-kind`: `message`, `title`, `todo`, `task`.
- `sessions` lists every session or chat active in a window (default: last 24 hours), for "what did I work on" questions.

## Workflow

1. Write 1–3 short queries in the user's own terms: names, errors, decisions. Paraphrases help because search is hybrid (BM25 + embeddings).
2. Run `tireless query -limit 5 "<words>"` for each. Add `-store` when the user names one, `-project` when they name a repo, `-source` when they name where it was said, `-container` when you already know the session or chat.
3. A promising hit without enough context → `tireless container -store <hit store> -after-ts <hit ts> -limit 8 <hit container_id>`. Read only the messages around the hit, not the whole session or chat.
4. Answer the user's actual question and cite what you used (store, source, container, author, date, project). Separate what the session or chat says from your own inference.

## Rules

- Only past decisions and discussion. For repository code use grep or code search.
- No relevant hits → say so and list the queries you tried. Never fill gaps from memory.
- A store that fails prints `store NAME: …` on stderr while the others still answer; say which store was unreachable instead of treating it as no results.
- Past decisions may be out of date: check the current code or config before acting on them.
