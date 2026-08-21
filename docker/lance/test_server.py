#!/usr/bin/env python3
import os
import tempfile
import unittest

import server


class SqlHelpers(unittest.TestCase):
    def test_filter_eq(self):
        sql = server.filter_sql({"eq": {"field": "session_id", "value": "s1"}})
        self.assertEqual(sql, "session_id = 's1'")

    def test_filter_and_escape(self):
        sql = server.filter_sql(
            {
                "and": [
                    {"eq": {"field": "session_id", "value": "a'b"}},
                    {"eq": {"field": "host", "value": "mac"}},
                ]
            }
        )
        self.assertIn("session_id = 'a''b'", sql)
        self.assertIn("host = 'mac'", sql)

    def test_cursor(self):
        self.assertEqual(server.cursor_sql(0, ""), "")
        self.assertEqual(server.cursor_sql(10, ""), "timestamp > 10")
        self.assertEqual(
            server.cursor_sql(10, "abc"),
            "(timestamp > 10 OR (timestamp = 10 AND id > 'abc'))",
        )

    def test_unknown_field(self):
        self.assertEqual(server.filter_sql({"eq": {"field": "vector", "value": "x"}}), "")


class LanceRoundtrip(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        server.URI = self.tmp.name
        server.TABLE = "turns"
        server.DIMS = 2
        server._db = None
        server._tbl = None
        server.COMPACT_AFTER = 0
        server.INDEX_CACHE_MB = 8
        server.META_CACHE_MB = 4
        os.environ.pop("AWS_ACCESS_KEY_ID", None)

    def tearDown(self):
        server._db = None
        server._tbl = None
        self.tmp.cleanup()

    def test_write_scan_page(self):
        vec = [0.1, 0.2]
        server.write_rows(
            [
                {
                    "id": "b",
                    "attributes": {
                        "vector": vec,
                        "forward_content": "second",
                        "session_id": "s1",
                        "timestamp": 20,
                    },
                },
                {
                    "id": "a",
                    "attributes": {
                        "vector": vec,
                        "forward_content": "first",
                        "session_id": "s1",
                        "timestamp": 10,
                    },
                },
                {
                    "id": "c",
                    "attributes": {
                        "vector": vec,
                        "forward_content": "other",
                        "session_id": "s2",
                        "timestamp": 15,
                    },
                },
            ]
        )
        page1 = server.handle_search(
            {"k": 1, "filter": {"eq": {"field": "session_id", "value": "s1"}}}
        )["results"]
        self.assertEqual([r["vector"]["id"] for r in page1], ["a"])
        page2 = server.handle_search(
            {
                "k": 10,
                "filter": {"eq": {"field": "session_id", "value": "s1"}},
                "after_ts": 10,
                "after_id": "a",
            }
        )["results"]
        self.assertEqual([r["vector"]["id"] for r in page2], ["b"])

    def test_bm25(self):
        z = [0.0, 0.0]
        server.write_rows(
            [
                {
                    "id": "hit",
                    "attributes": {
                        "vector": z,
                        "forward_content": "opendata vector cannot index code",
                        "session_id": "s1",
                        "timestamp": 1,
                    },
                },
                {
                    "id": "miss",
                    "attributes": {
                        "vector": z,
                        "forward_content": "grocery list milk",
                        "session_id": "s1",
                        "timestamp": 2,
                    },
                },
            ]
        )
        out = server.handle_search(
            {"k": 5, "bm25": {"field": "forward_content", "query": "opendata code"}}
        )["results"]
        self.assertGreaterEqual(len(out), 1)
        self.assertEqual(out[0]["vector"]["id"], "hit")

    def test_optimize(self):
        z = [0.0, 0.0]
        server.write_rows(
            [
                {
                    "id": "x",
                    "attributes": {
                        "vector": z,
                        "forward_content": "hello",
                        "session_id": "s1",
                        "timestamp": 1,
                    },
                }
            ]
        )
        out = server.optimize_table()
        self.assertEqual(out["status"], "ok")
        self.assertGreaterEqual(out["rows"], 1)
        self.assertGreaterEqual(out["fragments"], 1)

    def test_maybe_compact_after_small_writes(self):
        server.COMPACT_AFTER = 3
        z = [0.0, 0.0]
        for i in range(5):
            server.write_rows(
                [
                    {
                        "id": f"r{i}",
                        "attributes": {
                            "vector": z,
                            "forward_content": f"row {i} compact me",
                            "session_id": "s1",
                            "timestamp": i + 1,
                        },
                    }
                ]
            )
        n = server.fragment_count()
        self.assertLessEqual(n, 2, n)
        hits = server.handle_search(
            {"k": 10, "filter": {"eq": {"field": "session_id", "value": "s1"}}}
        )["results"]
        self.assertEqual(len(hits), 5)


if __name__ == "__main__":
    unittest.main()
