#!/usr/bin/env python3
"""OpenData Vector-shaped HTTP over one Lance `turns` table."""
from __future__ import annotations

import json
import os
import threading
import time
import traceback
from datetime import timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import urlparse

import lancedb
import pyarrow as pa

DIMS = int(os.environ.get("LANCE_DIMS", "384"))
TABLE = os.environ.get("LANCE_TABLE", "turns")
URI = os.environ.get("LANCE_URI", "/data/lance")
PORT = int(os.environ.get("LANCE_PORT", "8080"))
# Lance OSS: each write = one fragment. Compact when the pile is the problem.
COMPACT_AFTER = int(os.environ.get("LANCE_COMPACT_FRAGMENTS", "16"))
# Lance Session: index pages + file metadata, not a copy of turns.
# Defaults 6GiB+1GiB would OOM this 7.5GiB box.
INDEX_CACHE_MB = int(os.environ.get("LANCE_INDEX_CACHE_MB", "256"))
META_CACHE_MB = int(os.environ.get("LANCE_META_CACHE_MB", "64"))

STR_FIELDS = (
    "scope",
    "project_name",
    "file_path",
    "file_hash",
    "session_id",
    "host",
    "harness",
    "role",
)
ATTR_FIELDS = ("forward_content", "timestamp") + STR_FIELDS
OUT_COLS = ("id",) + ATTR_FIELDS  # never pull the 384-d vector on read

_db = None
_tbl = None
_ds = None
_lock = threading.Lock()


def storage_options() -> dict[str, str] | None:
    key = os.environ.get("AWS_ACCESS_KEY_ID", "")
    secret = os.environ.get("AWS_SECRET_ACCESS_KEY", "")
    if not key:
        return None
    opts = {
        "aws_access_key_id": key,
        "aws_secret_access_key": secret,
        "aws_region": os.environ.get("AWS_DEFAULT_REGION", os.environ.get("AWS_REGION", "auto")),
        "virtual_hosted_style_request": "false",
    }
    ep = os.environ.get("AWS_ENDPOINT_URL") or os.environ.get("AWS_ENDPOINT") or ""
    if ep:
        opts["aws_endpoint"] = ep
        if ep.startswith("http://"):
            opts["allow_http"] = "true"
    return opts


def schema() -> pa.Schema:
    return pa.schema(
        [
            pa.field("id", pa.utf8()),
            pa.field("vector", pa.list_(pa.float32(), DIMS)),
            pa.field("forward_content", pa.utf8()),
            pa.field("scope", pa.utf8()),
            pa.field("project_name", pa.utf8()),
            pa.field("file_path", pa.utf8()),
            pa.field("file_hash", pa.utf8()),
            pa.field("timestamp", pa.int64()),
            pa.field("session_id", pa.utf8()),
            pa.field("host", pa.utf8()),
            pa.field("harness", pa.utf8()),
            pa.field("role", pa.utf8()),
        ]
    )


def quote(v: str) -> str:
    return "'" + v.replace("\\", "\\\\").replace("'", "''") + "'"


def eq_sql(field: str, value: Any) -> str | None:
    if field not in STR_FIELDS:
        return None
    if value is None:
        return None
    return f"{field} = {quote(str(value))}"


def filter_sql(flt: dict | None) -> str:
    if not flt:
        return ""
    if flt.get("and"):
        parts = [filter_sql(p) for p in flt["and"]]
        parts = [p for p in parts if p]
        return "(" + " AND ".join(parts) + ")" if parts else ""
    eq = flt.get("eq") or {}
    return eq_sql(eq.get("field", ""), eq.get("value")) or ""


def cursor_sql(after_ts: int, after_id: str) -> str:
    if after_ts <= 0 and not after_id:
        return ""
    if after_ts <= 0:
        return f"id > {quote(after_id)}"
    if not after_id:
        return f"timestamp > {int(after_ts)}"
    return f"(timestamp > {int(after_ts)} OR (timestamp = {int(after_ts)} AND id > {quote(after_id)}))"


def combine_sql(*parts: str) -> str:
    xs = [p for p in parts if p]
    return " AND ".join(xs)


def open_table():
    global _db, _tbl
    opts = storage_options()
    session = lancedb.Session(INDEX_CACHE_MB * 1024 * 1024, META_CACHE_MB * 1024 * 1024)
    print(
        "session cache index_mb", INDEX_CACHE_MB, "meta_mb", META_CACHE_MB,
        flush=True,
    )
    kw = {"session": session}
    if opts:
        kw["storage_options"] = opts
    _db = lancedb.connect(URI, **kw)
    try:
        _tbl = _db.open_table(TABLE)
    except Exception:
        _tbl = _db.create_table(TABLE, schema=schema())
    try:
        _tbl.create_fts_index("forward_content")
    except Exception as e:
        print("fts index:", e, flush=True)
    try:
        _tbl.create_scalar_index("session_id")
    except Exception as e:
        print("session_id index:", e, flush=True)
    t0 = time.time()
    try:
        _tbl.to_lance().scanner(columns=["id"], limit=1).to_table()
        _tbl.search("warmup", query_type="fts").limit(1).select(["id"]).to_list()
    except Exception as e:
        print("warmup:", e, flush=True)
    print("warmup_s", round(time.time() - t0, 3), flush=True)
    global _ds
    _ds = _tbl.to_lance()
    return _tbl


def tbl():
    global _tbl
    if _tbl is None:
        open_table()
    return _tbl


def ds():
    global _ds
    if _ds is None:
        open_table()
    return _ds


def attrs_of(rec: dict) -> dict:
    a = rec.get("attributes") or {}
    out = {k: a.get(k, "" if k != "timestamp" else 0) for k in ATTR_FIELDS}
    out["timestamp"] = int(out["timestamp"] or 0)
    for k in STR_FIELDS:
        if out[k] is None:
            out[k] = ""
    vec = a.get("vector") or []
    if len(vec) != DIMS:
        raise ValueError(f"vector dims={len(vec)} want={DIMS}")
    return {"id": rec.get("id", ""), "vector": [float(x) for x in vec], **out}


def row_result(row: dict, score: float) -> dict:
    attrs = {k: row.get(k) for k in ATTR_FIELDS}
    return {"score": score, "vector": {"id": row.get("id", ""), "attributes": attrs}}


def after_row(ts: int, rid: str, after_ts: int, after_id: str) -> bool:
    if after_ts <= 0 and not after_id:
        return True
    if after_ts <= 0:
        return rid > after_id
    if ts > after_ts:
        return True
    return ts == after_ts and rid > after_id


def fragment_count() -> int:
    return len(tbl().to_lance().get_fragments())


def compact_now() -> dict:
    t = tbl()
    t.optimize(cleanup_older_than=timedelta(0))
    try:
        t.create_scalar_index("session_id", replace=True)
    except TypeError:
        try:
            t.create_scalar_index("session_id")
        except Exception as e:
            print("session_id index:", e, flush=True)
    except Exception as e:
        print("session_id index:", e, flush=True)
    ds = t.to_lance()
    return {"fragments": len(ds.get_fragments()), "rows": ds.count_rows()}


def maybe_compact() -> dict | None:
    n = fragment_count()
    if COMPACT_AFTER <= 0 or n < COMPACT_AFTER:
        return None
    out = compact_now()
    print("compacted fragments", n, "->", out["fragments"], "rows", out["rows"], flush=True)
    return out


def write_rows(records: list[dict]) -> None:
    if not records:
        return
    rows = [attrs_of(r) for r in records]
    with _lock:
        tbl().merge_insert("id").when_matched_update_all().when_not_matched_insert_all().execute(rows)
        maybe_compact()


def optimize_table() -> dict:
    with _lock:
        out = compact_now()
        out["status"] = "ok"
        return out


def search_scan(filter_where: str, after_ts: int, after_id: str, k: int) -> list[dict]:
    where = combine_sql(filter_where, cursor_sql(after_ts, after_id))
    kwargs: dict[str, Any] = {
        "columns": list(OUT_COLS),
        "late_materialization": True,
        "use_scalar_index": True,
        "order_by": ["timestamp", "id"],
    }
    if where:
        kwargs["filter"] = where
    if k > 0:
        kwargs["limit"] = k
    with _lock:
        table = tbl().to_lance().scanner(**kwargs).to_table()
    if table.num_rows == 0:
        return []
    return [row_result(rec, float(rec.get("timestamp") or 0)) for rec in table.to_pylist()]


def _with_select(q):
    return q.select(list(OUT_COLS))


def search_vector(vec: list[float], where: str, k: int) -> list[dict]:
    with _lock:
        q = _with_select(tbl().search(vec))
        if where:
            q = q.where(where, prefilter=True)
        rows = q.limit(max(k, 1)).to_list()
    out = []
    for rec in rows:
        dist = rec.get("_distance")
        score = 0.0 if dist is None else 1.0 / (1.0 + float(dist))
        out.append(row_result(rec, score))
    return out


def search_bm25(query: str, field: str, where: str, k: int) -> list[dict]:
    if field and field != "forward_content":
        raise ValueError(f"bm25 field {field}")
    with _lock:
        q = _with_select(tbl().search(query, query_type="fts"))
        if where:
            q = q.where(where, prefilter=True)
        rows = q.limit(max(k, 1)).to_list()
    out = []
    for rec in rows:
        score = rec.get("_score", rec.get("score", 0.0))
        out.append(row_result(rec, float(score or 0)))
    return out


def search_hybrid(vec: list[float], query: str, where: str, k: int) -> list[dict]:
    with _lock:
        q = _with_select(tbl().search(query_type="hybrid").vector(vec).text(query))
        if where:
            q = q.where(where, prefilter=True)
        rows = q.limit(max(k, 1)).to_list()
    out = []
    for rec in rows:
        score = rec.get("_relevance_score", rec.get("_score", rec.get("_distance", 0.0)))
        out.append(row_result(rec, float(score or 0)))
    return out


def handle_search(body: dict) -> dict:
    k = int(body.get("k") or 10)
    where = combine_sql(filter_sql(body.get("filter")), cursor_sql(int(body.get("after_ts") or 0), str(body.get("after_id") or "")))
    vec = body.get("vector") or []
    bm25 = body.get("bm25")
    text = (bm25 or {}).get("query") or ""
    if vec and text:
        results = search_hybrid([float(x) for x in vec], text, where, k)
    elif vec:
        results = search_vector([float(x) for x in vec], where, k)
    elif text:
        results = search_bm25(text, (bm25 or {}).get("field") or "forward_content", where, k)
    else:
        results = search_scan(filter_sql(body.get("filter")), int(body.get("after_ts") or 0), str(body.get("after_id") or ""), k)
    return {"status": "ok", "results": results}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print("%s - %s" % (self.address_string(), fmt % args), flush=True)

    def _send(self, code: int, obj: Any):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        path = urlparse(self.path).path
        if path in ("/-/ready", "/health"):
            self._send(200, {"status": "ready"})
            return
        self._send(404, {"message": "not found"})

    def do_POST(self):
        path = urlparse(self.path).path
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            body = json.loads(raw.decode() or "{}")
        except json.JSONDecodeError as e:
            self._send(400, {"message": str(e)})
            return
        try:
            if path == "/api/v1/vector/write":
                write_rows(body.get("upsertVectors") or [])
                self._send(200, {"status": "ok"})
                return
            if path == "/api/v1/vector/search":
                self._send(200, handle_search(body))
                return
            if path == "/api/v1/vector/optimize":
                self._send(200, optimize_table())
                return
            self._send(404, {"message": "not found"})
        except Exception as e:
            traceback.print_exc()
            self._send(500, {"message": str(e)})


def main():
    open_table()
    httpd = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"lance sidecar {URI} table={TABLE} :{PORT}", flush=True)
    httpd.serve_forever()


if __name__ == "__main__":
    main()
