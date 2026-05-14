#!/usr/bin/env python3
# /// script
# dependencies = [
#   "aiohttp"
# ]
# ///

"""
Stress test for the cosplay voting app.

Requirements:
    pip install aiohttp

Phases:
  1. Admin logs in and uploads NUM_COSPLAYS random-color images.
  2. Gets the current access code via WebSocket (/ws).
  3. Launches NUM_VOTERS concurrent voters; each gets a session and
     submits votes for every pair returned by the server.

Usage:
    python stress_test.py [--url http://localhost:8080]
                          [--cosplays 6]
                          [--voters 30]
                          [--skip-upload]
"""

import asyncio
import json
import random
import re
import struct
import sys
import time
import zlib
import argparse
from dataclasses import dataclass, field
from typing import Optional

import aiohttp


# ---------------------------------------------------------------------------
# Config defaults (override via CLI flags)
# ---------------------------------------------------------------------------
DEFAULT_URL = "http://localhost:8080"
ADMIN_USER = "admin"
ADMIN_PASS = "password"
DEFAULT_COSPLAYS = 6
DEFAULT_VOTERS = 30


# ---------------------------------------------------------------------------
# PNG generation — no external deps needed
# ---------------------------------------------------------------------------
def make_png(r: int, g: int, b: int, size: int = 64) -> bytes:
    """Create a solid-colour PNG in memory."""
    def chunk(tag: bytes, data: bytes) -> bytes:
        crc = zlib.crc32(tag + data) & 0xFFFFFFFF
        return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", crc)

    ihdr = struct.pack(">IIBBBBB", size, size, 8, 2, 0, 0, 0)
    raw = b"".join(b"\x00" + bytes([r, g, b] * size) for _ in range(size))
    idat = zlib.compress(raw)
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", idat)
        + chunk(b"IEND", b"")
    )


# ---------------------------------------------------------------------------
# Stats collector
# ---------------------------------------------------------------------------
@dataclass
class Stats:
    sessions_ok: int = 0
    sessions_fail: int = 0
    votes_ok: int = 0
    votes_fail: int = 0
    latencies: list = field(default_factory=list)
    _lock: asyncio.Lock = field(default_factory=asyncio.Lock)

    async def record_session(self, ok: bool) -> None:
        async with self._lock:
            if ok:
                self.sessions_ok += 1
            else:
                self.sessions_fail += 1

    async def record_vote(self, ok: bool, latency: float) -> None:
        async with self._lock:
            if ok:
                self.votes_ok += 1
            else:
                self.votes_fail += 1
            self.latencies.append(latency)

    def report(self, elapsed: float) -> None:
        total = self.votes_ok + self.votes_fail
        lats = sorted(self.latencies)
        avg = sum(lats) / len(lats) if lats else 0
        p95 = lats[int(len(lats) * 0.95)] if lats else 0
        p99 = lats[int(len(lats) * 0.99)] if lats else 0
        mx  = lats[-1] if lats else 0
        thr = total / elapsed if elapsed > 0 else 0

        print()
        print("=" * 52)
        print("  STRESS TEST RESULTS")
        print("=" * 52)
        print(f"  Total wall time : {elapsed:.2f}s")
        print(f"  Sessions        : {self.sessions_ok} OK / {self.sessions_fail} FAIL")
        print(f"  Votes           : {self.votes_ok} OK / {self.votes_fail} FAIL")
        print(f"  Throughput      : {thr:.1f} votes/s")
        print(f"  Avg latency     : {avg*1000:.1f} ms")
        print(f"  P95 latency     : {p95*1000:.1f} ms")
        print(f"  P99 latency     : {p99*1000:.1f} ms")
        print(f"  Max latency     : {mx*1000:.1f} ms")
        print("=" * 52)


# ---------------------------------------------------------------------------
# Helper: parse the `pairs` array embedded in votes.html
# ---------------------------------------------------------------------------
def parse_pairs(html: str) -> Optional[list]:
    """
    Finds `pairs: [...]` in the rendered template and JSON-parses it.
    Uses bracket counting to handle nested arrays/objects correctly.
    """
    marker = "pairs: "
    pos = html.find(marker)
    if pos == -1:
        return None
    start = html.index("[", pos)
    depth = 0
    for i, ch in enumerate(html[start:], start):
        if ch == "[":
            depth += 1
        elif ch == "]":
            depth -= 1
            if depth == 0:
                try:
                    return json.loads(html[start : i + 1])
                except json.JSONDecodeError:
                    return None
    return None


# ---------------------------------------------------------------------------
# Phase 1: admin login + cosplay upload
# ---------------------------------------------------------------------------
async def admin_setup(base: str, n: int) -> bool:
    connector = aiohttp.TCPConnector(ssl=False)
    async with aiohttp.ClientSession(connector=connector) as s:
        print(f"[Admin] Logging in as '{ADMIN_USER}'…")
        async with s.post(
            f"{base}/login",
            data={"username": ADMIN_USER, "password": ADMIN_PASS},
            allow_redirects=True,
        ) as resp:
            if "admin" not in str(resp.url) and resp.status not in (200,):
                print(f"[Admin] Login failed (status {resp.status})")
                return False
        print(f"[Admin] Logged in. Uploading {n} cosplay(s)…")

        for i in range(n):
            r, g, b = random.randint(40, 230), random.randint(40, 230), random.randint(40, 230)
            img = make_png(r, g, b)
            form = aiohttp.FormData()
            form.add_field("nome", f"TestCosplay_{i + 1}")
            form.add_field("desc", f"Auto-generated cosplay #{i + 1}")
            form.add_field("email", f"cosplay{i + 1}@stress.test")
            form.add_field(
                "foto", img,
                filename=f"cosplay_{i + 1}.png",
                content_type="image/png",
            )
            async with s.post(f"{base}/new-cosplay", data=form) as resp:
                tag = "OK" if resp.status == 200 else f"FAIL({resp.status})"
                print(f"[Admin]   Cosplay {i + 1:02}: {tag}")

    return True


# ---------------------------------------------------------------------------
# Phase 2: get current code via WebSocket
# ---------------------------------------------------------------------------
async def get_code(base: str) -> Optional[str]:
    ws_url = base.replace("http://", "ws://").replace("https://", "wss://") + "/ws"
    print(f"\n[Code] Connecting to {ws_url}…")
    try:
        async with aiohttp.ClientSession() as s:
            async with s.ws_connect(ws_url) as ws:
                msg = await asyncio.wait_for(ws.receive(), timeout=10)
                data = json.loads(msg.data)
                return data["code"]
    except Exception as exc:
        print(f"[Code] WebSocket error: {exc}")
        return None


# ---------------------------------------------------------------------------
# Phase 3: single voter coroutine
# ---------------------------------------------------------------------------
async def run_voter(voter_id: int, base: str, code: str, stats: Stats) -> None:
    tag = f"V{voter_id:03}"
    async with aiohttp.ClientSession() as s:
        # --- create session by hitting /vote?code=... ---
        try:
            async with s.get(f"{base}/vote?code={code}") as resp:
                if resp.status != 200:
                    await stats.record_session(False)
                    print(f"[{tag}] Session FAIL (HTTP {resp.status})")
                    return
                html = await resp.text()
        except Exception as exc:
            await stats.record_session(False)
            print(f"[{tag}] Session error: {exc}")
            return

        await stats.record_session(True)

        pairs = parse_pairs(html)
        if not pairs:
            print(f"[{tag}] Could not parse pairs — page may be an error page")
            return

        print(f"[{tag}] Session OK, {len(pairs)} pair(s) to vote on")

        # --- submit one vote per pair ---
        for idx, pair in enumerate(pairs):
            chosen = random.randint(0, 1)
            form = aiohttp.FormData()
            form.add_field("vote", str(pair[chosen]["id"]))
            form.add_field("opt1", str(pair[0]["id"]))
            form.add_field("opt2", str(pair[1]["id"]))
            try:
                t0 = time.perf_counter()
                async with s.post(f"{base}/vote", data=form) as resp:
                    lat = time.perf_counter() - t0
                    ok = resp.status == 200
                    await stats.record_vote(ok, lat)
                    if not ok:
                        body = await resp.text()
                        print(f"[{tag}] Vote {idx + 1} FAIL ({resp.status}): {body[:60]}")
            except Exception as exc:
                await stats.record_vote(False, 0.0)
                print(f"[{tag}] Vote {idx + 1} error: {exc}")


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------
async def main(args: argparse.Namespace) -> None:
    base = args.url.rstrip("/")

    if not args.skip_upload:
        ok = await admin_setup(base, args.cosplays)
        if not ok:
            print("Admin setup failed. Is the server running with correct credentials?")
            sys.exit(1)
    else:
        print("[Admin] Skipping upload (--skip-upload)")

    code = await get_code(base)
    if not code:
        print("Could not retrieve access code. Aborting.")
        sys.exit(1)
    print(f"[Code] Access code: {code}")

    print(f"\n[Load] Spawning {args.voters} concurrent voter(s)…")
    stats = Stats()
    t_start = time.perf_counter()
    await asyncio.gather(*[run_voter(i, base, code, stats) for i in range(args.voters)])
    elapsed = time.perf_counter() - t_start

    stats.report(elapsed)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Cosplay voting stress test")
    parser.add_argument("--url",         default=DEFAULT_URL, help="Base URL of the app")
    parser.add_argument("--cosplays",    type=int, default=DEFAULT_COSPLAYS, help="Cosplays to upload")
    parser.add_argument("--voters",      type=int, default=DEFAULT_VOTERS,   help="Concurrent voters")
    parser.add_argument("--skip-upload", action="store_true", help="Skip cosplay upload phase")
    args = parser.parse_args()

    asyncio.run(main(args))
