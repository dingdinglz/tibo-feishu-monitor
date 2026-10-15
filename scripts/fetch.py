#!/usr/bin/env python3
"""One JSON request in, one version-1 JSON result out (Python >= 3.10).

Only the raw twscrape API is used. Its normal generator exhaustion is NOT an
end-of-timeline signal, and its public parsers can silently write response
payloads to /tmp. The small, strict parser below reads only the fields we need;
it neither invokes those parsers nor downloads media. Unknown shapes fail
closed. `complete` describes the fixed-since scan, not all historical X posts.
"""

import asyncio
from contextlib import aclosing, redirect_stderr, redirect_stdout
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from email.utils import parsedate_to_datetime
import html
import json
import logging
import math
import os
import re
import signal
import sys

UTC = timezone.utc
QUEUE = "UserTweetsAndReplies"
DETAIL_TIMEOUT = 3.0
MAX_INPUT_BYTES = 8 * 1024 * 1024
ID_RE = re.compile(r"[0-9]+\Z", re.ASCII)
NAME_RE = re.compile(r"[A-Za-z0-9_]{1,15}\Z", re.ASCII)
ERROR_CODES = frozenset({
    "invalid_request", "invalid_response", "empty_page", "head_missing_target",
    "parse_failed", "upstream_error", "upstream_incomplete", "no_account",
    "timeout", "cancelled", "dependency_error", "accounts_db_missing", "internal_error",
})


class Failure(Exception):
    """Internal, fixed codes only; never put an upstream exception in a result."""

    def __init__(self, code):
        self.code = code if code in ERROR_CODES else "upstream_error"
        super().__init__()


def result_template():
    return {
        "version": 1, "posts": [], "latest": None, "contexts": [], "head_ok": False,
        "complete": False, "cursor": "", "pages": 0, "warnings": [],
        "error": "", "retry_after_seconds": 0,
    }


def ident(value):
    # Never pass an X ID through float (including floats supplied by a caller).
    if isinstance(value, int) and not isinstance(value, bool):
        value = str(value)
    if not isinstance(value, str) or not ID_RE.fullmatch(value):
        raise ValueError()
    return value


def username(value):
    if not isinstance(value, str) or not NAME_RE.fullmatch(value):
        raise ValueError()
    return value


def timestamp(value):
    if not isinstance(value, str):
        raise ValueError()
    # Explicit replacement also supports Python 3.10's fromisoformat.
    dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if dt.tzinfo is None or dt.utcoffset() is None:
        raise ValueError()
    return dt.astimezone(UTC)


def url(twid, author=""):
    return f"https://x.com/{author or 'i/web'}/status/{twid}"


def context(twid, author="", text="", available=False):
    return {"id": twid, "author": author, "text": text,
            "url": url(twid, author), "available": available}


def path(obj, *keys):
    for key in keys:
        if not isinstance(obj, dict):
            return None
        obj = obj.get(key)
    return obj


@dataclass
class Request:
    accounts_db: str
    user_id: str
    username: str
    proxy: str
    since: datetime
    cursor: str
    max_pages: int
    timeout_seconds: float
    wait_seconds: float
    context_limit: int
    cached_contexts: dict
    include_latest: bool = False

    @classmethod
    def parse(cls, obj):
        if (not isinstance(obj, dict) or type(obj.get("version", 1)) is not int
                or obj.get("version", 1) != 1):
            raise ValueError()
        for key in ("accounts_db", "user_id", "username", "since"):
            if not isinstance(obj.get(key), str) or not obj[key]:
                raise ValueError()
        for key in ("cursor", "proxy"):
            if not isinstance(obj.get(key, ""), str):
                raise ValueError()
        for key, minimum in (("max_pages", 1), ("context_limit", 0)):
            value = obj.get(key, 0)
            if type(value) is not int or value < minimum:
                raise ValueError()
        for key in ("timeout_seconds", "wait_seconds"):
            value = obj.get(key, 0)
            if type(value) not in (float, int) or not math.isfinite(value) or value < 0:
                raise ValueError()
        if obj.get("timeout_seconds", 0) <= 0:
            raise ValueError()
        if type(obj.get("include_latest", False)) is not bool:
            raise ValueError()
        cached = obj.get("cached_contexts")
        if cached is None:
            cached = {}
        if not isinstance(cached, dict):
            raise ValueError()
        clean_cache = {}
        for key, item in cached.items():
            if not isinstance(item, dict) or item.get("available") is not True:
                continue
            try:
                cid = ident(item["id"])
                if key != cid or not isinstance(item["text"], str):
                    continue
                author = username(item["author"])
                # Cached text has already been unescaped; do not unescape twice.
                clean_cache[cid] = context(cid, author, item["text"], True)
            except (KeyError, ValueError, TypeError):
                continue
        return cls(
            obj["accounts_db"], ident(obj["user_id"]), username(obj["username"]),
            obj.get("proxy", ""), timestamp(obj["since"]), obj.get("cursor", ""),
            obj["max_pages"], float(obj["timeout_seconds"]),
            float(obj.get("wait_seconds", 0)), obj.get("context_limit", 0), clean_cache,
            obj.get("include_latest", False),
        )


def unwrap(raw):
    if not isinstance(raw, dict):
        raise ValueError()
    if raw.get("__typename") == "TweetWithVisibilityResults":
        raw = raw.get("tweet")
    if not isinstance(raw, dict) or raw.get("__typename") != "Tweet":
        raise ValueError()
    return raw


def flat(raw):
    legacy = raw.get("legacy")
    if legacy is not None and not isinstance(legacy, dict):
        raise ValueError()
    # 0.20.1 supports both legacy objects and legacy=null/top-level fields.
    return {**(legacy or {}), **raw}


@dataclass
class Tweet:
    id: str
    author_id: str
    author: str
    date: datetime
    text: str
    retweet: bool
    reply_id: str
    reply_author: str
    quote_id: str
    is_quote: bool

    def as_context(self):
        return context(self.id, self.author, self.text, True)


def parse_tweet(raw):
    raw = unwrap(raw)
    tw = flat(raw)
    twid = ident(tw.get("rest_id") or tw.get("id_str"))
    author_raw = (path(raw, "core", "user_results", "result")
                  or path(raw, "author_results", "result"))
    if not isinstance(author_raw, dict) or author_raw.get("__typename") != "User":
        raise ValueError()
    author = flat(author_raw)
    author_id = ident(author.get("rest_id") or author.get("id_str"))
    if tw.get("user_id_str") is not None and ident(tw["user_id_str"]) != author_id:
        raise ValueError()
    name = username(author.get("screen_name") or path(author, "core", "screen_name"))
    date_text = tw.get("created_at")
    if not isinstance(date_text, str):
        raise ValueError()
    try:
        date = parsedate_to_datetime(date_text)
    except (ValueError, TypeError):
        date = timestamp(date_text)
    if date.tzinfo is None or date.utcoffset() is None:
        raise ValueError()
    text = path(tw, "note_tweet", "note_tweet_results", "result", "text")
    if text is None:
        text = tw.get("full_text")
    if not isinstance(text, str):
        raise ValueError()
    reply_id = tw.get("in_reply_to_status_id_str")
    reply_id = ident(reply_id) if reply_id is not None else ""
    reply_author = tw.get("in_reply_to_screen_name") or ""
    if not isinstance(reply_author, str) or not NAME_RE.fullmatch(reply_author):
        reply_author = ""
    quote_id = tw.get("quoted_status_id_str")
    quote_raw = path(tw, "quoted_status_result", "result")
    if quote_id is None and isinstance(quote_raw, dict):
        try:
            quote_id = unwrap(quote_raw).get("rest_id")
        except ValueError:
            pass
    quote_id = ident(quote_id) if quote_id is not None else ""
    # A missing/inaccessible nested RT must not turn a native retweet into a post.
    retweet = any(tw.get(k) is not None for k in (
        "retweeted_status_id_str", "retweeted_status_result", "retweetedTweet"))
    return Tweet(twid, author_id, name, date.astimezone(UTC), html.unescape(text),
                 retweet, reply_id, reply_author, quote_id,
                 bool(quote_id or tw.get("is_quote_status")))


def tweet_nodes(obj):
    """Collect context candidates, never use this recursive walk as a post list."""
    stack = [obj]
    while stack:
        node = stack.pop()
        if isinstance(node, dict):
            if node.get("__typename") == "Tweet":
                yield node
            stack.extend(node.values())
        elif isinstance(node, list):
            stack.extend(node)


def excluded(envelope):
    if not isinstance(envelope, dict):
        return False
    return ("promotedMetadata" in envelope or envelope.get("isPinned") is True
            or path(envelope, "socialContext", "contextType") in ("Pin", "Pinned"))


def item_tweet(item):
    if not isinstance(item, dict):
        raise Failure("invalid_response")
    if excluded(item):
        return None
    if item.get("itemType") != "TimelineTweet":
        if item.get("itemType") in ("TimelineUser", "TimelinePrompt", "TimelineMessage"):
            return None
        raise Failure("invalid_response")
    # Keep unavailable/tombstoned results as parse failures, not empty successes.
    return path(item, "tweet_results", "result") or {}


def entry_tweets(entry):
    if not isinstance(entry, dict) or not isinstance(entry.get("content"), dict):
        raise Failure("invalid_response")
    content = entry["content"]
    if excluded(entry) or excluded(content):
        return []
    etype = content.get("entryType") or content.get("__typename")
    if etype == "TimelineTimelineItem":
        raw = item_tweet(content.get("itemContent"))
        return [] if raw is None else [raw]
    if etype == "TimelineTimelineModule":
        # Every explicit conversation item may be a distinct target-authored
        # reply (including self-replies). Keep them all; author/time/ID filters
        # and durable dedup separate real discoveries from repeated context.
        # This is NOT the recursive quote/context walk used by tweet_nodes.
        items = content.get("items")
        if not isinstance(items, list):
            raise Failure("invalid_response")
        if str(entry.get("entryId", "")).startswith("who-to-follow-"):
            return []
        raws = []
        for item in items:
            if not isinstance(item, dict):
                raise Failure("invalid_response")
            if excluded(item) or excluded(item.get("item")):
                continue
            raw = item_tweet(path(item, "item", "itemContent"))
            if raw is not None:
                raws.append(raw)
        return raws
    raise Failure("invalid_response")


@dataclass
class Page:
    raw: dict
    direct: list
    cursor: str | None
    terminal: bool


def read_page(rep):
    if getattr(rep, "status_code", 200) != 200:
        raise Failure("upstream_error")
    try:
        obj = rep.json()
    except Exception:
        raise Failure("invalid_response") from None
    instructions = None
    # Deliberately not a recursive `find instructions`: quotes may contain data.
    for keys in (("data", "user", "result", "timeline_v2", "timeline", "instructions"),
                 ("data", "user", "result", "timeline", "timeline", "instructions"),
                 ("data", "user", "result", "timeline", "instructions")):
        value = path(obj, *keys)
        if value is not None:
            instructions = value
            break
    if not isinstance(instructions, list):
        raise Failure("invalid_response")
    entries, recognized, terminated = [], False, False
    for instruction in instructions:
        if not isinstance(instruction, dict):
            raise Failure("invalid_response")
        kind = instruction.get("type")
        if kind == "TimelineAddEntries":
            if not isinstance(instruction.get("entries"), list):
                raise Failure("invalid_response")
            entries.extend(instruction["entries"])
            recognized = True
        elif kind == "TimelineReplaceEntry":
            entries.append(instruction.get("entry"))
            recognized = True
        elif kind == "TimelineAddToModule":
            entries.append({"entryId": instruction.get("moduleEntryId", ""), "content": {
                "entryType": "TimelineTimelineModule", "items": instruction.get("moduleItems")}})
            recognized = True
        elif kind == "TimelineTerminateTimeline":
            terminated |= instruction.get("direction") == "Bottom"
        elif kind not in ("TimelinePinEntry", "TimelineClearCache"):
            raise Failure("invalid_response")
    if not recognized:
        raise Failure("empty_page")
    direct, cursors = [], []
    for entry in entries:
        content = entry.get("content") if isinstance(entry, dict) else None
        if not isinstance(content, dict):
            raise Failure("invalid_response")
        if (content.get("entryType") == "TimelineTimelineCursor"
                or content.get("__typename") == "TimelineTimelineCursor"
                or "cursorType" in content):
            if content.get("cursorType") not in ("Top", "Bottom"):
                raise Failure("invalid_response")
            if content["cursorType"] == "Bottom":
                if "value" not in content:
                    raise Failure("invalid_response")
                value = content["value"]
                if value is not None and not isinstance(value, str):
                    raise Failure("invalid_response")
                cursors.append(value or None)
            continue
        direct.extend(entry_tweets(entry))
    if len(cursors) > 1:
        raise Failure("invalid_response")
    bottom = cursors[0] if cursors else None
    # Missing Bottom is meaningful only inside the known, nonempty envelope;
    # empty pages are rejected by the observer even if Bottom is missing.
    return Page(obj, direct, bottom, terminated or bottom is None)


class ObservedPages:
    """Mixin for twscrape 0.20.1, including pages discarded before yielding.

    No upstream monkey patches. _is_stalled runs before _is_end; it must be
    observed separately so its early return can never look like completion.
    """

    def _is_end(self, rep, q, res, cur, cnt, lim):
        stop = self.page_observer(rep, cur)
        rep, total, active = super()._is_end(rep, q, res, cur, cnt, lim)
        return rep, total, active and not stop

    def _is_stalled(self, q, res, cur, seen):
        stalled = super()._is_stalled(q, res, cur, seen)
        if stalled:
            self.stall_observer()
        return stalled


def make_api(request):
    # Set BEFORE importing twscrape. No public parser is called, so its dump
    # path is unreachable; QueueClient's response dumps require debug=True.
    os.environ["TWS_TELEMETRY"] = "0"
    os.environ["DO_NOT_TRACK"] = "1"
    os.environ["TWS_LOG_LEVEL"] = "CRITICAL"
    try:
        from importlib.metadata import version
        if version("twscrape") != "0.20.1":
            raise Failure("dependency_error")
        from twscrape import API, NoAccountError
        from twscrape.logger import logger
        logger.remove()
        logger.disable("twscrape")
    except Failure:
        raise
    except Exception:
        raise Failure("dependency_error") from None
    if not os.path.isfile(request.accounts_db):
        raise Failure("accounts_db_missing")

    class SafeAPI(ObservedPages, API):
        no_account_errors = (NoAccountError,)

    return SafeAPI(
        pool=request.accounts_db, proxy=request.proxy or None, debug=False,
        raise_when_no_account=True,
        wait_timeout=min(request.wait_seconds, request.timeout_seconds),
        wait_interval=min(1.0, max(0.05, request.wait_seconds)),
    )


def retry_delay(value, now):
    """next_available_at returns local HH:MM:SS in 0.20.1, without identities."""
    if value == "now":
        return 1
    if isinstance(value, str) and re.fullmatch(r"\d{2}:\d{2}:\d{2}", value):
        h, m, s = map(int, value.split(":"))
        local = now.astimezone()
        target = local.replace(hour=h, minute=m, second=s, microsecond=0)
        if target < local.replace(microsecond=0):
            target += timedelta(days=1)
    elif isinstance(value, datetime):
        if value.tzinfo is None:
            value = value.replace(tzinfo=UTC)
        target = value
    elif isinstance(value, str):
        target = timestamp(value)
    else:
        return 0
    return max(1, math.ceil((target - now).total_seconds()))


class Scan:
    def __init__(self, request, clock, detail_timeout):
        self.req = request
        self.result = result_template()
        self.result["cursor"] = request.cursor
        self.cache = dict(request.cached_contexts)
        self.fresh_contexts = {}
        self.post_ids = set()
        self.latest_key = None
        self.api = None
        self.active_queue = QUEUE
        self.clock = clock
        self.detail_timeout = detail_timeout
        self.reason = ""
        self.head_only = False
        self.old_pages = 0
        self.seen_cursors = set()
        self.seen_pages = set()
        self.boundary = False
        self.timeline_finished = False

    def warn(self, code):
        if code not in self.result["warnings"]:
            self.result["warnings"].append(code)

    def remember(self, tweet):
        item = tweet.as_context()
        self.cache[tweet.id] = item
        self.fresh_contexts[tweet.id] = item

    def add_post(self, tweet):
        if tweet.author_id != self.req.user_id or tweet.retweet:
            return
        in_window = tweet.date >= self.req.since and tweet.id not in self.post_ids
        if not in_window and not self.req.include_latest:
            return
        post = {
            "id": tweet.id, "author_id": tweet.author_id, "author": tweet.author,
            "created_at": tweet.date.isoformat().replace("+00:00", "Z"),
            "text": tweet.text, "url": url(tweet.id, tweet.author),
            "kind": "reply" if tweet.reply_id else "quote" if tweet.is_quote else "post",
        }
        if tweet.reply_id:
            post["reply_to_id"] = tweet.reply_id
            post["parent"] = context(tweet.reply_id, tweet.reply_author)
        if tweet.quote_id:
            post["quote"] = context(tweet.quote_id)
        elif tweet.is_quote:
            self.warn("context_unavailable")
        if self.req.include_latest:
            key = (tweet.date, int(tweet.id))
            if self.latest_key is None or key > self.latest_key:
                self.latest_key = key
                self.result["latest"] = post
        if in_window:
            self.post_ids.add(tweet.id)
            self.result["posts"].append(post)

    def output_posts(self):
        yield from self.result["posts"]
        latest = self.result["latest"]
        if latest and latest["id"] not in self.post_ids:
            yield latest

    def on_page(self, rep, upstream_cursor):
        self.result["pages"] += 1
        page = read_page(rep)
        # Build the entire page's cache BEFORE attaching any parents/quotes.
        for raw in tweet_nodes(page.raw):
            try:
                self.remember(parse_tweet(raw))
            except (ValueError, TypeError, KeyError, OverflowError):
                pass  # Context failure is not evidence about direct timeline items.
        parsed, failed = [], 0
        for raw in page.direct:
            try:
                parsed.append(parse_tweet(raw))
            except (ValueError, TypeError, KeyError, OverflowError):
                failed += 1
        for tweet in parsed:
            self.add_post(tweet)
        self.link_contexts()
        if page.raw.get("errors"):
            raise Failure("upstream_error")
        if page.cursor != (upstream_cursor or None):
            raise Failure("invalid_response")
        if not page.direct:
            raise Failure("empty_page")
        if failed:
            if parsed:
                self.warn("partial_parse")
            raise Failure("parse_failed")
        if self.result["pages"] == 1:
            if not any(t.author_id == self.req.user_id and not t.retweet for t in parsed):
                raise Failure("head_missing_target")
            self.result["head_ok"] = True
        fingerprint = tuple(sorted({t.id for t in parsed}))
        if fingerprint in self.seen_pages or (page.cursor and page.cursor in self.seen_cursors):
            self.warn("pagination_stalled")
            raise Failure("upstream_incomplete")
        self.seen_pages.add(fingerprint)
        if page.cursor:
            self.seen_cursors.add(page.cursor)
        if self.head_only:
            # Do not overwrite a saved continuation with the fresh head cursor.
            self.reason = "head"
            return True
        if page.cursor:
            self.result["cursor"] = page.cursor
        own = [t for t in parsed if t.author_id == self.req.user_id and not t.retweet]
        self.old_pages = self.old_pages + 1 if own and all(
            t.date < self.req.since for t in own
        ) else 0
        if page.terminal or self.old_pages >= 2:
            self.boundary, self.reason = True, "boundary"
        elif self.result["pages"] >= self.req.max_pages:
            self.warn("max_pages")
            self.reason = "budget"
        return bool(self.reason)

    def on_stall(self):
        self.result["pages"] += 1
        self.warn("pagination_stalled")
        raise Failure("upstream_incomplete")

    async def segment(self, cursor, head_only=False):
        self.reason, self.head_only, self.old_pages = "", head_only, 0
        self.seen_cursors = {cursor} if cursor else set()
        self.seen_pages = set()
        kv = {"count": 40, "includePromotedContent": False}
        if cursor:
            kv["cursor"] = cursor
        gen = self.api.user_tweets_and_replies_raw(int(self.req.user_id), limit=-1, kv=kv)
        async with aclosing(gen):
            async for _ in gen:
                if self.reason:
                    break
        if not self.reason:
            # Includes client.get() -> None, swallowed upstream exceptions,
            # empty-page caps and any future unobserved upstream early return.
            raise Failure("upstream_incomplete")

    def link_contexts(self):
        for post in self.output_posts():
            for key in ("parent", "quote"):
                item = post.get(key)
                if item and item["id"] in self.cache:
                    post[key] = dict(self.cache[item["id"]])

    def no_account(self, exc):
        return isinstance(exc, getattr(self.api, "no_account_errors", ()))

    async def retry_after(self):
        try:
            method = getattr(self.api.pool, "next_available_at", None)
            if method is None:
                method = getattr(self.api.pool, "next_available", None)
            if method is not None:
                value = await asyncio.wait_for(method(self.active_queue), timeout=1.0)
                self.result["retry_after_seconds"] = retry_delay(value, self.clock())
        except Exception:
            pass  # Optional timing hint; no account names, cookies or DB rows escape.

    async def enrich(self):
        self.link_contexts()
        attempts = set()
        for post in self.output_posts():
            parent = post.get("parent")
            if not parent or parent["id"] in self.cache or parent["id"] in attempts:
                continue
            if len(attempts) >= self.req.context_limit:
                self.warn("context_limit")
                continue
            wanted = parent["id"]
            attempts.add(wanted)
            self.active_queue = "TweetDetail"
            try:
                # Raw detail avoids parse_tweet/parse_tweets' silent failures and dumps.
                rep = await asyncio.wait_for(
                    self.api.tweet_details_raw(int(wanted)), timeout=self.detail_timeout)
                if rep is not None and getattr(rep, "status_code", 200) == 200:
                    for raw in tweet_nodes(rep.json()):
                        try:
                            tweet = parse_tweet(raw)
                            if tweet.id == wanted:
                                self.remember(tweet)
                                break
                        except (ValueError, TypeError, KeyError, OverflowError):
                            continue
            except Exception as exc:
                if self.no_account(exc):
                    # Detail and timeline queues have independent rate limits.
                    # Optional context must not pause otherwise healthy polling.
                    self.warn("context_unavailable")
                    break
                # No recursive detail lookups: contexts are exactly one layer.
            self.link_contexts()
        for post in self.output_posts():
            if any(not post[key]["available"] for key in ("parent", "quote") if key in post):
                self.warn("context_unavailable")

    async def run(self, api_factory):
        try:
            self.api = api_factory(self.req)
            self.api.page_observer = self.on_page
            self.api.stall_observer = self.on_stall
            await self.segment("", head_only=bool(self.req.cursor))
            if self.req.cursor:
                if self.result["pages"] >= self.req.max_pages:
                    self.warn("max_pages")
                else:
                    # These are not consecutive pages with the refreshed head.
                    await self.segment(self.req.cursor)
            if self.boundary and self.req.cursor:
                # A one-page refresh cannot prove overlap with an old cursor:
                # more than one head page may have arrived between rounds.
                # Persist progress, then require a fresh, gap-free scan, even
                # if subsequent context enrichment fails or times out.
                self.result["cursor"] = ""
                self.warn("continuation_finished_rescan")
            if self.boundary and not self.req.cursor:
                self.result["cursor"] = ""
                self.result["complete"] = True
            self.timeline_finished = True
            await self.enrich()
        except Failure as exc:
            self.result["error"] = exc.code
        except Exception as exc:
            if self.no_account(exc):
                self.result["error"] = "no_account"
                await self.retry_after()
            else:
                self.result["error"] = "upstream_error"


async def fetch(data, api_factory=None, clock=None, detail_timeout=DETAIL_TIMEOUT):
    """Inject a fake raw API and clock for fully offline standard-library tests."""
    try:
        request = Request.parse(data)
    except Exception:
        result = result_template()
        result["error"] = "invalid_request"
        return result
    scan = Scan(request, clock or (lambda: datetime.now(UTC)), detail_timeout)
    try:
        # Covers paging, pool waits, context details and optional retry metadata.
        # Cancellation unwinds every aclosing / QueueClient context before return.
        await asyncio.wait_for(scan.run(api_factory or make_api), request.timeout_seconds)
    except asyncio.TimeoutError:
        if scan.timeline_finished:
            # The timeline's coverage evidence survives optional enrichment
            # timing out. Replies remain sendable without their parents.
            scan.warn("context_timeout")
        else:
            scan.result["complete"] = False
            scan.result["error"] = scan.result["error"] or "timeout"
            scan.warn("timeout")
    except asyncio.CancelledError:
        scan.result["complete"] = False
        scan.result["error"] = "cancelled"
        scan.warn("cancelled")
    except Exception:
        scan.result["complete"] = False
        scan.result["error"] = "upstream_error"
    finally:
        scan.link_contexts()
        scan.result["contexts"] = list(scan.fresh_contexts.values())
    return scan.result


async def run_request(data):
    # Go cancels this subprocess with SIGTERM. Let asyncio unwind twscrape's
    # QueueClient contexts instead of leaving 15-minute account-use locks.
    loop = asyncio.get_running_loop()
    task = asyncio.current_task()
    installed = []
    for sig in (signal.SIGTERM, signal.SIGINT):
        try:
            loop.add_signal_handler(sig, task.cancel)
            installed.append(sig)
        except (NotImplementedError, RuntimeError):
            pass
    try:
        return await fetch(data)
    finally:
        for sig in installed:
            loop.remove_signal_handler(sig)


def main(stdin=None, stdout=None):
    source = stdin or sys.stdin
    destination = stdout or sys.stdout
    # Redirect before dependency import as well as during execution. Never
    # forward upstream stderr, even when it looks like a harmless error message.
    with open(os.devnull, "w") as sink, redirect_stdout(sink), redirect_stderr(sink):
        previous_disable = logging.root.manager.disable
        logging.disable(logging.CRITICAL)
        try:
            data = source.read(MAX_INPUT_BYTES + 1)
            if len(data.encode("utf-8")) > MAX_INPUT_BYTES:
                raise ValueError()
            request = json.loads(data)
        except Exception:
            result = result_template()
            result["error"] = "invalid_request"
        else:
            try:
                result = asyncio.run(run_request(request))
            except BaseException:
                result = result_template()
                result["error"] = "internal_error"
        finally:
            logging.disable(previous_disable)
    destination.write(json.dumps(result, ensure_ascii=True, allow_nan=False) + "\n")
    destination.flush()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
