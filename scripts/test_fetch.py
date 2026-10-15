"""Offline only: synthetic GraphQL, fake pools/locks and no X/DB access.

Run: python -m unittest discover -s scripts -p 'test_fetch.py' -v
Only the optional installed-API contract tests import twscrape; even those
replace QueueClient before constructing an API, and never touch an account DB.
"""

import asyncio
from contextlib import aclosing, redirect_stderr, redirect_stdout
from datetime import datetime, timedelta, timezone
import importlib.util
import io
import json
import os
from pathlib import Path
import select
import subprocess
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import patch

# Works with both unittest discovery and `python scripts/test_fetch.py`.
SCRIPT = Path(__file__).resolve().with_name("fetch.py")
spec = importlib.util.spec_from_file_location("tibo_fetch", SCRIPT)
fetch = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = fetch
spec.loader.exec_module(fetch)

TARGET = "1953337039510003712"
OTHER = "9007199254740993"
BIG_ID = "2000000000000000123"
NOW = datetime(2026, 6, 3, 12, 0, tzinfo=timezone.utc)
NEW = "Wed Jun 03 11:00:00 +0000 2026"
OLD = "Mon Jun 01 11:00:00 +0000 2026"
SECRET = "auth_token=DO_NOT_LEAK; proxy=https://user:password@example.invalid"
BLOCK = object()
SILENT = object()


def request(**updates):
    obj = {
        "accounts_db": "/offline-never-open/accounts.db",
        "user_id": TARGET, "username": "thsottiaux", "proxy": "",
        "since": "2026-06-02T12:00:00Z", "cursor": "", "max_pages": 10,
        "timeout_seconds": 1.0, "wait_seconds": 0, "context_limit": 0,
        "cached_contexts": {},
    }
    obj.update(updates)
    return obj


def tweet(twid=BIG_ID, author=TARGET, name=None, date=NEW, text="A &amp; B",
          reply=None, quote=None, retweet=None, new_schema=False):
    name = name or ("thsottiaux" if author == TARGET else "other")
    user = {"__typename": "User", "rest_id": author,
            "legacy": {"screen_name": name, "name": name}}
    legacy = {"id_str": twid, "user_id_str": author, "created_at": date,
              "full_text": text}
    if reply:
        legacy["in_reply_to_status_id_str"] = reply
        legacy["in_reply_to_screen_name"] = "parent_author"
    result = {"__typename": "Tweet", "rest_id": twid, "legacy": legacy,
              "core": {"user_results": {"result": user}}}
    if quote:
        legacy["quoted_status_id_str"] = quote["rest_id"]
        legacy["is_quote_status"] = True
        result["quoted_status_result"] = {"result": quote}
    if retweet:
        legacy["retweeted_status_id_str"] = retweet["rest_id"]
        legacy["retweeted_status_result"] = {"result": retweet}
    if new_schema:
        result.update(legacy)
        result["legacy"] = None
        user["core"] = user.pop("legacy")
        user["legacy"] = None
    return result


def entry(raw, pin=False, promoted=False):
    item = {"itemType": "TimelineTweet", "tweet_results": {"result": raw}}
    if pin:
        item["socialContext"] = {"contextType": "Pin"}
    if promoted:
        item["promotedMetadata"] = {"advertiser": "irrelevant"}
    return {"entryId": "tweet-" + str(raw.get("rest_id", "unavailable")),
            "content": {"entryType": "TimelineTimelineItem", "itemContent": item}}


def module(*raws):
    return {"entryId": "profile-conversation-1", "content": {
        "entryType": "TimelineTimelineModule", "displayType": "VerticalConversation",
        "items": [{"entryId": "tweet-" + x["rest_id"],
                   "item": {"itemContent": entry(x)["content"]["itemContent"]}} for x in raws],
    }}


class Response:
    status_code = 200

    def __init__(self, data, cursor=None, rows=None, status=200):
        self.data = data
        self.cursor = cursor
        self.rows = rows or []
        self.status_code = status

    def json(self):
        if isinstance(self.data, Exception):
            raise self.data
        return self.data


def page(*raws, cursor=None, entries=None, pins=None, errors=None):
    rows = list(entries) if entries is not None else [entry(x) for x in raws]
    all_rows = list(rows)
    if cursor is not None:
        all_rows.append({"entryId": "cursor-bottom-0", "content": {
            "entryType": "TimelineTimelineCursor", "cursorType": "Bottom", "value": cursor}})
    instructions = [{"type": "TimelineAddEntries", "entries": all_rows}]
    if pins:
        instructions.insert(0, {"type": "TimelinePinEntry", "entry": entry(pins)})
    data = {"data": {"user": {"result": {"timeline_v2": {
        "timeline": {"instructions": instructions}}}}}}
    if errors:
        data["errors"] = errors
    return Response(data, cursor, rows)


class NoAccount(Exception):
    pass


class Pool:
    def __init__(self, next_at=None):
        self.next_at = next_at
        self.calls = []

    async def next_available_at(self, queue):
        self.calls.append(queue)
        return self.next_at


class FakeBase:
    """Emulates 0.20.1's silent None, dropped empty pages and stall checks."""

    no_account_errors = (NoAccount,)

    def __init__(self, routes, details=None, pool=None):
        self.routes = routes
        self.details = details or {}
        self.pool = pool or Pool()
        self.calls = []
        self.detail_calls = []
        self.open_locks = 0
        self.closed = []
        self.detail_closed = []

    def _is_end(self, rep, q, res, cur, cnt, lim):
        return rep if res else None, cnt + len(res), cur is not None

    def _is_stalled(self, q, res, cur, seen):
        keys = {("cursor", cur)} if cur is not None else set()
        stalled = bool(seen & keys)
        seen.clear()
        seen.update(keys)
        return stalled

    async def _gql_items(self, route):
        self.open_locks += 1
        seen = set()
        count = 0
        try:
            for value in self.routes.get(route, []):
                if value is BLOCK:
                    await asyncio.Event().wait()
                if value is SILENT:
                    return
                if isinstance(value, Exception):
                    raise value
                # Upstream decodes JSON BEFORE invoking the observer hook.
                value.json()
                if self._is_stalled(fetch.QUEUE, value.rows, value.cursor, seen):
                    return
                rep, count, active = self._is_end(
                    value, fetch.QUEUE, value.rows, value.cursor, count, -1)
                if rep is not None:
                    yield rep
                if not active:
                    return
        finally:
            self.open_locks -= 1
            self.closed.append(route)

    async def user_tweets_and_replies_raw(self, uid, limit=-1, kv=None):
        self.calls.append((uid, limit, dict(kv)))
        route = kv.get("cursor", "")
        async with aclosing(self._gql_items(route)) as gen:
            async for rep in gen:
                yield rep

    async def tweet_details_raw(self, twid):
        self.detail_calls.append(str(twid))
        self.open_locks += 1
        try:
            value = self.details.get(str(twid))
            if value is BLOCK:
                await asyncio.Event().wait()
            if isinstance(value, Exception):
                raise value
            return value
        finally:
            self.open_locks -= 1
            self.detail_closed.append(str(twid))


class FakeAPI(fetch.ObservedPages, FakeBase):
    pass


class FetchTests(unittest.IsolatedAsyncioTestCase):
    async def run_scan(self, api, **updates):
        return await fetch.fetch(request(**updates), api_factory=lambda _: api,
                                 clock=lambda: NOW, detail_timeout=0.02)

    async def test_filter_author_retweets_nested_context_and_id_precision(self):
        nested = tweet("2000000000000000201", text="nested target must not become a post")
        quoted = tweet("2000000000000000202", author=OTHER, quote=nested)
        foreign = tweet("2000000000000000203", author=OTHER)
        native_rt = tweet("2000000000000000204", retweet=nested)
        inaccessible_rt = tweet("2000000000000000205")
        inaccessible_rt["legacy"]["retweeted_status_id_str"] = "123"
        post = tweet(quote=quoted, reply="901", new_schema=True)
        api = FakeAPI({"": [page(post, foreign, native_rt, inaccessible_rt)]})
        result = await self.run_scan(api)
        self.assertTrue(result["complete"])
        self.assertTrue(result["head_ok"])
        self.assertEqual([x["id"] for x in result["posts"]], [BIG_ID])
        saved = result["posts"][0]
        self.assertIsInstance(saved["id"], str)
        self.assertEqual(saved["author_id"], TARGET)
        self.assertEqual(saved["text"], "A & B")
        self.assertEqual(saved["kind"], "reply")  # reply wins over quote
        self.assertEqual(saved["quote"]["id"], quoted["rest_id"])
        self.assertNotIn("quote", saved["quote"])
        self.assertEqual(saved["url"], f"https://x.com/thsottiaux/status/{BIG_ID}")
        self.assertTrue(saved["created_at"].endswith("Z"))
        self.assertFalse(saved["parent"]["available"])
        self.assertIn("/status/901", saved["parent"]["url"])
        self.assertEqual(api.open_locks, 0)
        self.assertEqual(api.calls[0][0], int(TARGET))

    async def test_debug_latest_can_be_outside_window_without_normal_posts(self):
        latest = tweet("101", date=OLD, reply="201")
        parent = tweet("201", author=OTHER, date=OLD)
        api = FakeAPI({"": [page(latest, parent)]})
        result = await self.run_scan(api, include_latest=True)
        self.assertTrue(result["head_ok"])
        self.assertEqual(result["posts"], [])
        self.assertEqual(result["latest"]["id"], "101")
        self.assertEqual(result["latest"]["kind"], "reply")
        self.assertTrue(result["latest"]["parent"]["available"])
        self.assertEqual(api.detail_calls, [])

    async def test_debug_latest_uses_time_then_numeric_id_not_order_or_nested_quote(self):
        nested = tweet("9999", date="Wed Jun 03 11:59:00 +0000 2026")
        api = FakeAPI({"": [page(tweet("100", date=OLD), tweet("10", quote=nested),
                                 tweet("9"), tweet("900", author=OTHER),
                                 tweet("999", retweet=nested))]})
        result = await self.run_scan(api, include_latest=True)
        self.assertEqual(result["latest"]["id"], "10")
        self.assertTrue(result["latest"]["quote"]["available"])
        self.assertEqual({p["id"] for p in result["posts"]}, {"9", "10"})

    async def test_without_debug_does_not_fetch_old_latest_context(self):
        api = FakeAPI({"": [page(tweet("101", date=OLD, reply="201"))]})
        result = await self.run_scan(api, context_limit=4)
        self.assertIsNone(result["latest"])
        self.assertEqual(result["posts"], [])
        self.assertEqual(api.detail_calls, [])

    async def test_debug_old_latest_can_enrich_with_existing_detail_budget(self):
        api = FakeAPI({"": [page(tweet("101", date=OLD, reply="201"))]},
                      details={"201": Response({"data": {"tweet": tweet("201", author=OTHER)}})})
        result = await self.run_scan(api, include_latest=True, context_limit=1)
        self.assertEqual(result["posts"], [])
        self.assertTrue(result["latest"]["parent"]["available"])
        self.assertEqual(api.detail_calls, ["201"])

    async def test_post_and_quote_kinds_and_original_long_text(self):
        raw = tweet("101", text="short")
        raw["note_tweet"] = {"note_tweet_results": {"result": {"text": "long &lt;original&gt;"}}}
        quoted = tweet("102", quote=tweet("103", author=OTHER))
        result = await self.run_scan(FakeAPI({"": [page(raw, quoted)]}))
        self.assertEqual([x["kind"] for x in result["posts"]], ["post", "quote"])
        self.assertEqual(result["posts"][0]["text"], "long <original>")

    async def test_old_pin_promoted_and_known_ids_do_not_stop(self):
        known = fetch.context("101", "thsottiaux", "cached", True)
        old = tweet("80", date=OLD)
        api = FakeAPI({"": [
            page(entries=[entry(old, pin=True), entry(tweet("101")),
                          entry(tweet("99"), promoted=True)], cursor="C1", pins=old),
            page(tweet("102"), cursor="C2"), page(tweet("103")),
        ]})
        result = await self.run_scan(api, cached_contexts={"101": known})
        self.assertTrue(result["complete"])
        self.assertEqual(result["pages"], 3)
        self.assertEqual([x["id"] for x in result["posts"]], ["101", "102", "103"])

    async def test_one_old_tweet_or_page_is_not_a_boundary(self):
        api = FakeAPI({"": [page(tweet("90", date=OLD), tweet("101"), cursor="C1"),
                              page(tweet("89", date=OLD), cursor="C2"),
                              page(tweet("102"), cursor="C3"),
                              page(tweet("88", date=OLD), cursor="C4"),
                              page(tweet("87", date=OLD), cursor="C5"), BLOCK]})
        result = await self.run_scan(api)
        self.assertTrue(result["complete"])
        self.assertEqual(result["pages"], 5)
        self.assertEqual([x["id"] for x in result["posts"]], ["101", "102"])
        self.assertEqual(result["cursor"], "")
        self.assertEqual(api.open_locks, 0)

    async def test_foreign_context_does_not_prevent_old_target_page_boundary(self):
        api = FakeAPI({"": [page(tweet("101"), cursor="C1"),
                              page(tweet("90", date=OLD), cursor="C2"),
                              page(tweet("89", date=OLD), tweet("88", author=OTHER, date=OLD), cursor="C3"),
                              page(tweet("87", date=OLD), cursor="C4"),
                              page(tweet("86", date=OLD), cursor="C5")]})
        result = await self.run_scan(api)
        self.assertTrue(result["complete"])
        self.assertEqual(result["pages"], 3)

    async def test_since_is_time_not_id_and_boundary_is_inclusive(self):
        api = FakeAPI({"": [page(tweet("1", date="Tue Jun 02 12:00:00 +0000 2026"),
                              tweet("9999999999999999999", date=OLD))]})
        result = await self.run_scan(api)
        self.assertEqual([x["id"] for x in result["posts"]], ["1"])

    async def test_head_then_saved_cursor_and_budget_counts_head(self):
        api = FakeAPI({"": [page(tweet("101"), cursor="NEW"), BLOCK],
                       "SAVED": [page(tweet("102"), cursor="NEXT"), BLOCK]})
        result = await self.run_scan(api, cursor="SAVED", max_pages=2)
        self.assertEqual([c[2].get("cursor", "") for c in api.calls], ["", "SAVED"])
        self.assertTrue(all(c[2]["count"] == 40 and c[1] == -1 for c in api.calls))
        self.assertEqual(result["pages"], 2)
        self.assertFalse(result["complete"])
        self.assertEqual(result["cursor"], "NEXT")
        self.assertIn("max_pages", result["warnings"])
        self.assertEqual([x["id"] for x in result["posts"]], ["101", "102"])
        self.assertEqual(api.open_locks, 0)
        self.assertEqual(api.closed, ["", "SAVED"])

    async def test_head_only_budget_preserves_saved_cursor(self):
        api = FakeAPI({"": [page(tweet("101"), cursor="NEW"), BLOCK]})
        result = await self.run_scan(api, cursor="SAVED", max_pages=1)
        self.assertFalse(result["complete"])
        self.assertEqual(result["cursor"], "SAVED")
        self.assertEqual(len(api.calls), 1)
        self.assertIn("max_pages", result["warnings"])

    async def test_completed_continuation_requires_fresh_rescan(self):
        api = FakeAPI({"": [page(tweet("101"), cursor="NEW")],
                       "SAVED": [page(tweet("90", date=OLD), cursor="C1"),
                                 page(tweet("89", date=OLD), cursor="C2")]})
        result = await self.run_scan(api, cursor="SAVED")
        self.assertFalse(result["complete"])
        self.assertEqual(result["cursor"], "")
        self.assertEqual(result["error"], "")
        self.assertIn("continuation_finished_rescan", result["warnings"])
        fresh = await self.run_scan(FakeAPI({"": [page(tweet("101"), cursor="H1"),
                                                   page(tweet("102"), cursor="H2"),
                                                   page(tweet("90", date=OLD))]}))
        self.assertTrue(fresh["complete"])
        self.assertEqual([x["id"] for x in fresh["posts"]], ["101", "102"])

    async def test_terminal_continuation_is_also_not_complete(self):
        api = FakeAPI({"": [page(tweet("101"))], "SAVED": [page(tweet("102"))]})
        result = await self.run_scan(api, cursor="SAVED")
        self.assertFalse(result["complete"])
        self.assertIn("continuation_finished_rescan", result["warnings"])
        self.assertEqual(result["cursor"], "")

    async def test_old_head_does_not_count_toward_continuation_streak(self):
        api = FakeAPI({"": [page(tweet("90", date=OLD), cursor="NEW")],
                       "SAVED": [page(tweet("89", date=OLD), cursor="C1"),
                                 page(tweet("101")), BLOCK]})
        result = await self.run_scan(api, cursor="SAVED")
        self.assertEqual(result["pages"], 3)
        self.assertEqual([x["id"] for x in result["posts"]], ["101"])
        self.assertFalse(result["complete"])

    async def test_empty_structure_and_all_parse_failure_do_not_make_baseline(self):
        bad = tweet()
        bad["legacy"]["created_at"] = SECRET
        samples = [page(cursor="NEXT"), page(), Response({}), page(bad),
                   page(entries=[entry(tweet(), promoted=True)]),
                   page(tweet(author=OTHER)),
                   page(tweet(), errors=[{"message": SECRET}])]
        for response in samples:
            with self.subTest(response=response):
                api = FakeAPI({"": [response, BLOCK]})
                result = await self.run_scan(api)
                self.assertFalse(result["head_ok"])
                self.assertFalse(result["complete"])
                self.assertTrue(result["error"])
                self.assertNotIn(SECRET, json.dumps(result))
                self.assertEqual(api.open_locks, 0)

    async def test_empty_continuation_observed_even_when_upstream_drops_it(self):
        api = FakeAPI({"": [page(tweet(), cursor="GOOD"), page(cursor="BAD"), BLOCK]})
        result = await self.run_scan(api)
        self.assertEqual(result["pages"], 2)
        self.assertEqual(result["cursor"], "GOOD")
        self.assertEqual(result["error"], "empty_page")
        self.assertFalse(result["complete"])
        self.assertEqual(len(result["posts"]), 1)

    async def test_silent_and_raised_upstream_failures_keep_partial_posts(self):
        for failure in (SILENT, RuntimeError(SECRET), Response(ValueError(SECRET))):
            with self.subTest(failure=failure):
                api = FakeAPI({"": [page(tweet(), cursor="GOOD"), failure]})
                result = await self.run_scan(api)
                self.assertFalse(result["complete"])
                self.assertTrue(result["error"])
                self.assertEqual(result["cursor"], "GOOD")
                self.assertEqual(len(result["posts"]), 1)
                self.assertNotIn(SECRET, json.dumps(result))
                self.assertEqual(api.open_locks, 0)
        result = await self.run_scan(FakeAPI({"": [SILENT]}))
        self.assertFalse(result["head_ok"])
        self.assertEqual(result["error"], "upstream_incomplete")

    async def test_partial_parse_keeps_posts_but_not_unreliable_next_cursor(self):
        bad = tweet("103")
        del bad["legacy"]["full_text"]
        api = FakeAPI({"": [page(tweet("101"), cursor="GOOD"),
                              page(tweet("102"), bad, cursor="BAD")]})
        result = await self.run_scan(api)
        self.assertEqual(result["cursor"], "GOOD")
        self.assertFalse(result["complete"])
        self.assertEqual(result["error"], "parse_failed")
        self.assertIn("partial_parse", result["warnings"])
        self.assertEqual([x["id"] for x in result["posts"]], ["101", "102"])

    async def test_upstream_stall_and_long_cursor_cycle_never_complete(self):
        for cursors in (("A", "A"), ("A", "B", "A")):
            api = FakeAPI({"": [page(tweet(str(100 + i)), cursor=c)
                                   for i, c in enumerate(cursors)]})
            result = await self.run_scan(api)
            self.assertFalse(result["complete"])
            self.assertIn("pagination_stalled", result["warnings"])
            self.assertEqual(result["pages"], len(cursors))
            self.assertEqual(api.open_locks, 0)

    async def test_repeated_old_entries_with_new_cursor_are_not_two_old_pages(self):
        api = FakeAPI({"": [page(tweet("101"), cursor="A"),
                              page(tweet("90", date=OLD), cursor="B"),
                              page(tweet("90", date=OLD), cursor="C")]})
        result = await self.run_scan(api)
        self.assertFalse(result["complete"])
        self.assertIn("pagination_stalled", result["warnings"])
        self.assertEqual(result["cursor"], "B")

    async def test_malformed_cursor_or_unknown_instruction_is_not_terminal(self):
        for mutate in ("cursor", "instruction"):
            response = page(tweet(), cursor="GOOD")
            instructions = response.data["data"]["user"]["result"]["timeline_v2"]["timeline"]["instructions"]
            if mutate == "cursor":
                del instructions[0]["entries"][-1]["content"]["value"]
            else:
                instructions.append({"type": "NewUnknownTimelineType"})
            result = await self.run_scan(FakeAPI({"": [response]}))
            self.assertEqual(result["error"], "invalid_response")
            self.assertFalse(result["head_ok"])
            self.assertFalse(result["complete"])

    async def test_context_cache_and_same_page_ancestors_avoid_details(self):
        ancestor = tweet("201", text="fresh parent", reply="999", date=OLD)
        child = tweet("202", reply="201")
        cached_parent = fetch.context("203", "cached_parent", "already &lt;decoded", True)
        cached_stale = fetch.context("201", "thsottiaux", "stale", True)
        cached_reply = tweet("204", reply="203")
        api = FakeAPI({"": [page(entries=[module(ancestor, child), entry(cached_reply)])]})
        result = await self.run_scan(api, context_limit=5,
                                     cached_contexts={"201": cached_stale, "203": cached_parent})
        self.assertTrue(result["complete"])
        self.assertEqual([x["id"] for x in result["posts"]], ["202", "204"])
        self.assertEqual(result["posts"][0]["parent"]["text"], "fresh parent")
        self.assertEqual(result["posts"][1]["parent"]["text"], "already &lt;decoded")
        self.assertEqual(api.detail_calls, [])
        self.assertIn("201", {x["id"] for x in result["contexts"]})
        self.assertNotIn("parent", result["posts"][0]["parent"])

    async def test_all_target_self_replies_in_conversation_module_are_retained(self):
        root = tweet("200", author=OTHER)
        first = tweet("201", reply="200")
        second = tweet("202", reply="201")
        third = tweet("203", reply="202")
        api = FakeAPI({"": [page(entries=[module(root, first, second, third)])]})
        result = await self.run_scan(api, context_limit=4)
        self.assertTrue(result["complete"])
        self.assertEqual([p["id"] for p in result["posts"]], ["201", "202", "203"])
        self.assertEqual([p["kind"] for p in result["posts"]], ["reply"] * 3)
        self.assertTrue(all(p["parent"]["available"] for p in result["posts"]))
        self.assertEqual(api.detail_calls, [])

    async def test_later_page_context_avoids_request_and_other_reply_is_retained(self):
        reply = tweet("202", reply="201")
        parent = tweet("201", author=OTHER)
        api = FakeAPI({"": [page(reply, cursor="A"), page(parent)]})
        result = await self.run_scan(api, context_limit=1)
        self.assertEqual([x["id"] for x in result["posts"]], ["202"])
        self.assertEqual(result["posts"][0]["parent"]["author"], "other")
        self.assertEqual(api.detail_calls, [])

    async def test_detail_limit_dedup_success_failure_and_no_recursive_fetch(self):
        replies = [tweet("301", reply="201"), tweet("302", reply="201"),
                   tweet("303", reply="202"), tweet("304", reply="203")]
        detail = Response({"data": {"tweet": tweet("201", author=OTHER, reply="999")}})
        api = FakeAPI({"": [page(*replies)]},
                      details={"201": detail, "202": RuntimeError(SECRET)})
        result = await self.run_scan(api, context_limit=2)
        self.assertEqual(api.detail_calls, ["201", "202"])
        self.assertTrue(result["posts"][0]["parent"]["available"])
        self.assertTrue(result["posts"][1]["parent"]["available"])
        self.assertFalse(result["posts"][2]["parent"]["available"])
        self.assertFalse(result["posts"][3]["parent"]["available"])
        self.assertIn("context_limit", result["warnings"])
        self.assertIn("context_unavailable", result["warnings"])
        self.assertTrue(result["complete"])
        self.assertNotIn(SECRET, json.dumps(result))
        self.assertEqual(api.open_locks, 0)
        self.assertIn("201", {x["id"] for x in result["contexts"]})

    async def test_individual_detail_timeout_releases_lock(self):
        api = FakeAPI({"": [page(tweet(reply="201"))]}, details={"201": BLOCK})
        result = await self.run_scan(api, context_limit=1)
        self.assertTrue(result["complete"])
        self.assertFalse(result["posts"][0]["parent"]["available"])
        self.assertIn("context_unavailable", result["warnings"])
        self.assertEqual(api.detail_closed, ["201"])
        self.assertEqual(api.open_locks, 0)

    async def test_overall_timeout_closes_nested_generators_and_keeps_progress(self):
        api = FakeAPI({"": [page(tweet(reply="201"), cursor="GOOD"), BLOCK]})
        result = await self.run_scan(api, timeout_seconds=0.02, context_limit=1)
        self.assertTrue(result["head_ok"])
        self.assertFalse(result["complete"])
        self.assertEqual(result["error"], "timeout")
        self.assertIn("timeout", result["warnings"])
        self.assertEqual(result["cursor"], "GOOD")
        self.assertEqual(len(result["posts"]), 1)
        self.assertFalse(result["posts"][0]["parent"]["available"])
        self.assertEqual(api.closed, [""])
        self.assertEqual(api.open_locks, 0)

    async def test_optional_context_timeout_does_not_invalidate_timeline(self):
        api = FakeAPI({"": [page(tweet(reply="201"))]}, details={"201": BLOCK})
        result = await fetch.fetch(request(timeout_seconds=0.02, context_limit=1),
                                   api_factory=lambda _: api, detail_timeout=10)
        self.assertTrue(result["complete"])
        self.assertEqual(result["error"], "")
        self.assertIn("context_timeout", result["warnings"])
        self.assertEqual(len(result["posts"]), 1)
        self.assertEqual(api.open_locks, 0)
        self.assertEqual(api.detail_closed, ["201"])

    async def test_finished_continuation_still_rescans_if_contexts_timeout(self):
        api = FakeAPI({"": [page(tweet(reply="201"), cursor="NEW")],
                       "SAVED": [page(tweet("102"))]}, details={"201": BLOCK})
        result = await fetch.fetch(request(cursor="SAVED", timeout_seconds=0.02, context_limit=1),
                                   api_factory=lambda _: api, detail_timeout=10)
        self.assertFalse(result["complete"])
        self.assertEqual(result["cursor"], "")
        self.assertIn("continuation_finished_rescan", result["warnings"])
        self.assertIn("context_timeout", result["warnings"])
        self.assertEqual(api.open_locks, 0)

    async def test_timeout_during_resume_preserves_resume_not_head_cursor(self):
        api = FakeAPI({"": [page(tweet(), cursor="NEW")], "SAVED": [BLOCK]})
        result = await self.run_scan(api, cursor="SAVED", timeout_seconds=0.02)
        self.assertEqual(result["cursor"], "SAVED")
        self.assertTrue(result["head_ok"])
        self.assertEqual(api.open_locks, 0)

    async def test_no_account_has_fixed_error_and_identity_free_retry_hint(self):
        pool = Pool(NOW + timedelta(seconds=125))
        api = FakeAPI({"": [NoAccount(SECRET)]}, pool=pool)
        result = await self.run_scan(api)
        self.assertEqual(result["error"], "no_account")
        self.assertEqual(result["retry_after_seconds"], 125)
        self.assertEqual(pool.calls, [fetch.QUEUE])
        self.assertFalse(result["head_ok"])
        self.assertNotIn(SECRET, json.dumps(result))

    async def test_detail_rate_limit_does_not_back_off_the_timeline_queue(self):
        api = FakeAPI({"": [page(tweet(reply="201"))]}, details={"201": NoAccount(SECRET)})
        result = await self.run_scan(api, context_limit=1)
        self.assertEqual(result["error"], "")
        self.assertTrue(result["complete"])
        self.assertEqual(len(result["posts"]), 1)
        self.assertFalse(result["posts"][0]["parent"]["available"])
        self.assertEqual(api.pool.calls, [])
        self.assertEqual(result["retry_after_seconds"], 0)

    async def test_cancellation_releases_nested_generator_locks(self):
        api = FakeAPI({"": [page(tweet(), cursor="GOOD"), BLOCK]})
        task = asyncio.create_task(self.run_scan(api, timeout_seconds=5))
        await asyncio.sleep(0.01)
        task.cancel()
        result = await task
        self.assertEqual(result["error"], "cancelled")
        self.assertFalse(result["complete"])
        self.assertEqual(result["cursor"], "GOOD")
        self.assertEqual(api.open_locks, 0)

    async def test_unavailable_cache_does_not_prevent_retry_and_unknown_parent_has_link(self):
        raw = tweet(reply="201")
        del raw["legacy"]["in_reply_to_screen_name"]
        api = FakeAPI({"": [page(raw)]}, details={"201": None})
        result = await self.run_scan(api, context_limit=1,
                                     cached_contexts={"201": fetch.context("201")})
        self.assertEqual(api.detail_calls, ["201"])
        parent = result["posts"][0]["parent"]
        self.assertFalse(parent["available"])
        self.assertEqual(parent["url"], "https://x.com/i/web/status/201")
        self.assertIn("context_unavailable", result["warnings"])

    async def test_visibility_wrapper_and_utc_normalization(self):
        raw = tweet(date="Wed Jun 03 19:00:00 +0800 2026")
        wrapped = {"__typename": "TweetWithVisibilityResults", "tweet": raw}
        result = await self.run_scan(FakeAPI({"": [page(entries=[entry(wrapped)])]}))
        self.assertTrue(result["complete"])
        self.assertEqual(result["posts"][0]["created_at"], "2026-06-03T11:00:00Z")

    async def test_invalid_resume_page_retains_saved_cursor_and_head_posts(self):
        api = FakeAPI({"": [page(tweet(), cursor="NEW")], "SAVED": [page()]})
        result = await self.run_scan(api, cursor="SAVED")
        self.assertEqual(result["cursor"], "SAVED")
        self.assertTrue(result["head_ok"])
        self.assertEqual(len(result["posts"]), 1)
        self.assertFalse(result["complete"])
        self.assertEqual(result["error"], "empty_page")

    async def test_request_validation_never_constructs_api(self):
        for update in ({"user_id": 1.9533370395100037e18}, {"max_pages": 0},
                       {"timeout_seconds": float("nan")}, {"context_limit": -1},
                       {"since": "2026-06-03T12:00:00"}, {"version": True},
                       {"cached_contexts": []}, {"include_latest": "true"}):
            with self.subTest(update=update):
                def forbidden(_):
                    self.fail("API factory must not be called")
                result = await fetch.fetch(request(**update), api_factory=forbidden)
                self.assertEqual(result["error"], "invalid_request")


class ProtocolTests(unittest.TestCase):
    @unittest.skipUnless(os.name == "posix", "POSIX signal/pipe integration")
    def test_sigterm_unwinds_real_subprocess(self):
        read_fd, write_fd = os.pipe()
        code = f'''from test_fetch import *
import os
class ReadyAPI(FakeAPI):
    def _is_end(self, *args):
        result = super()._is_end(*args)
        os.write({write_fd}, b"1")
        return result
api = ReadyAPI({{"": [page(tweet(), cursor="GOOD"), BLOCK]}})
fetch.make_api = lambda _: api
fetch.main(io.StringIO(json.dumps(request(timeout_seconds=10))))
raise SystemExit(0 if api.open_locks == 0 else 2)
'''
        process = subprocess.Popen([sys.executable, "-c", code], cwd=SCRIPT.parent,
                                   pass_fds=(write_fd,), stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True)
        os.close(write_fd)
        try:
            ready, _, _ = select.select([read_fd], [], [], 5)
            self.assertTrue(ready, "child did not reach active raw iterator")
            self.assertEqual(os.read(read_fd, 1), b"1")
            process.terminate()
            stdout, stderr = process.communicate(timeout=5)
            self.assertEqual(process.returncode, 0)
            self.assertEqual(stderr, "")
            result = json.loads(stdout)
            self.assertEqual(result["error"], "cancelled")
            self.assertEqual(result["cursor"], "GOOD")
        finally:
            os.close(read_fd)
            if process.poll() is None:
                process.kill()
                process.communicate()

    def test_factory_pins_dependency_and_disables_logs_telemetry_and_debug_dumps(self):
        calls = []

        class API:
            def __init__(self, **kwargs):
                calls.append(kwargs)

        logger = SimpleNamespace(remove=lambda: calls.append("remove"),
                                 disable=lambda name: calls.append(("disable", name)))
        fake_modules = {
            "twscrape": SimpleNamespace(API=API, NoAccountError=NoAccount),
            "twscrape.logger": SimpleNamespace(logger=logger),
        }
        with patch.dict(sys.modules, fake_modules), patch.dict(fetch.os.environ, {}, clear=True), \
                patch("importlib.metadata.version", return_value="0.20.1"), \
                patch.object(fetch.os.path, "isfile", return_value=True):
            api = fetch.make_api(fetch.Request.parse(request(proxy=SECRET, wait_seconds=2)))
            self.assertIsInstance(api, fetch.ObservedPages)
            self.assertEqual(fetch.os.environ["TWS_TELEMETRY"], "0")
            self.assertEqual(fetch.os.environ["DO_NOT_TRACK"], "1")
            self.assertEqual(calls[0:2], ["remove", ("disable", "twscrape")])
            self.assertFalse(calls[2]["debug"])
            self.assertTrue(calls[2]["raise_when_no_account"])
            self.assertEqual(calls[2]["wait_timeout"], 1)
            self.assertEqual(calls[2]["proxy"], SECRET)
        with patch("importlib.metadata.version", return_value="different-version"):
            with self.assertRaises(fetch.Failure) as error:
                fetch.make_api(fetch.Request.parse(request()))
            self.assertEqual(error.exception.code, "dependency_error")
        self.assertEqual(fetch.Failure(SECRET).code, "upstream_error")

    def test_retry_hint_time_of_day_wraps_midnight(self):
        local = NOW.astimezone().replace(hour=23, minute=59, second=59, microsecond=0)
        self.assertEqual(fetch.retry_delay("00:00:04", local), 5)
        self.assertEqual(fetch.retry_delay("23:59:59", local), 1)
        self.assertEqual(fetch.retry_delay(None, NOW), 0)
        self.assertEqual(fetch.retry_delay("now", NOW), 1)

    def test_one_json_response_even_for_upstream_prints_and_sensitive_exception(self):
        def failing_factory(_):
            print(SECRET)
            print(SECRET, file=sys.stderr)
            raise RuntimeError(SECRET)
        stdin, stdout, stderr = io.StringIO(json.dumps(request())), io.StringIO(), io.StringIO()
        with patch.object(fetch, "make_api", failing_factory), redirect_stderr(stderr):
            fetch.main(stdin, stdout)
        data = json.loads(stdout.getvalue())
        self.assertEqual(data["version"], 1)
        self.assertEqual(data["error"], "upstream_error")
        self.assertEqual(len(stdout.getvalue().splitlines()), 1)
        self.assertEqual(stderr.getvalue(), "")
        self.assertNotIn(SECRET, stdout.getvalue())

    def test_cli_invalid_or_multiple_json_needs_no_dependencies(self):
        for input_text in ("", "{", "{} {}", "null", json.dumps({"proxy": SECRET})):
            process = subprocess.run([sys.executable, str(SCRIPT)], input=input_text,
                                     capture_output=True, text=True, timeout=5)
            self.assertEqual(process.returncode, 0)
            self.assertEqual(process.stderr, "")
            self.assertEqual(len(process.stdout.splitlines()), 1)
            self.assertEqual(json.loads(process.stdout)["error"], "invalid_request")
            self.assertNotIn(SECRET, process.stdout)


class InstalledContractTests(unittest.IsolatedAsyncioTestCase):
    async def test_real_gql_loop_with_fake_queue_observes_discarded_empty_page(self):
        # Optional integration against the pinned source, still zero network/DB.
        import os
        os.environ["TWS_TELEMETRY"] = "0"
        os.environ["DO_NOT_TRACK"] = "1"
        os.environ["TWS_LOG_LEVEL"] = "CRITICAL"
        try:
            with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                from twscrape.api import API
                from twscrape import api as api_module
                from twscrape.logger import logger
                logger.remove()
        except ImportError:
            self.skipTest("optional twscrape not installed")
        responses = [page(tweet(), cursor="GOOD"), page(cursor="BAD")]
        closed = []

        class Queue:
            def __init__(self, *args, **kwargs):
                pass

            async def __aenter__(self):
                return self

            async def __aexit__(self, *args):
                closed.append(True)

            async def get(self, *args, **kwargs):
                return responses.pop(0)

        class APIWithoutDB(fetch.ObservedPages, API):
            def __init__(self):
                self.pool, self.debug, self.proxy = None, False, None

        with patch.object(api_module, "QueueClient", Queue):
            result = await fetch.fetch(request(), api_factory=lambda _: APIWithoutDB())
        self.assertEqual(result["pages"], 2)
        self.assertEqual(result["error"], "empty_page")
        self.assertEqual(result["cursor"], "GOOD")
        self.assertFalse(result["complete"])
        self.assertEqual(closed, [True])


if __name__ == "__main__":
    unittest.main()
