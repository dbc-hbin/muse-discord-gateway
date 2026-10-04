#!/usr/bin/env python3
"""
SSD hot-deal collector.

Scrapes Korean deal sources for SSD posts and emits JSON candidates
for the cron worker to judge (초특가 판정).

Sources (plain HTTP, verified working from sandbox egress):
  - clien 알뜰구매: https://www.clien.net/service/board/jirum
  - Telegram 정세몬 (뽐뿌/퀘존/쿨엔조이 미러): https://t.me/s/jsmon

Blocked from this network (documented, not retried):
  - ppomppu.co.kr: 403, arca.live: Cloudflare, quasarzone.com: Cloudflare,
    coolenjoy.net / ruliweb.com: unreachable

Usage:
  ssd-deal-scan.py --out /path/to/candidates.json

Proxy: standard *_PROXY env vars (credential-free or not; requests handles it).
"""
import argparse
import json
import re
import sys
import time
from datetime import datetime, timezone

import requests
from bs4 import BeautifulSoup

UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")

SSD_KEYWORDS = ["ssd", "솔리드", "nvme", "m.2"]


def is_ssd_post(title: str) -> bool:
    t = title.lower()
    return any(k in t for k in SSD_KEYWORDS)


def parse_price_krw(text: str):
    """Extract KRW price from Korean deal titles. Returns int or None."""
    if not text:
        return None
    t = text.replace(",", "").replace(" ", "").replace("￦", "")
    m = re.search(r"(\d+(?:\.\d+)?)만원", t)
    if m:
        return int(float(m.group(1)) * 10000)
    m = re.search(r"(\d+)만(\d+)천원", t)
    if m:
        return int(m.group(1)) * 10000 + int(m.group(2)) * 1000
    m = re.search(r"(\d+)천원", t)
    if m:
        return int(m.group(1)) * 1000
    m = re.search(r"(\d{4,7})원", t)
    if m:
        v = int(m.group(1))
        if 1000 <= v <= 10000000:
            return v
    m = re.search(r"\$(\d+(?:\.\d+)?)", t)
    if m:
        return int(float(m.group(1)) * 1380)
    return None


def parse_capacity_gb(title: str):
    t = title.lower().replace(" ", "")
    m = re.search(r"(\d+(?:\.\d+)?)tb", t)
    if m:
        return int(float(m.group(1)) * 1000)
    m = re.search(r"(\d+)gb", t)
    if m:
        return int(m.group(1))
    return None


def session():
    s = requests.Session()
    s.headers.update({"User-Agent": UA, "Accept-Language": "ko-KR,ko;q=0.9"})
    # requests honors *_PROXY env automatically; trust_env default True
    return s


def scrape_clien(s):
    url = "https://www.clien.net/service/board/jirum"
    r = s.get(url, timeout=30)
    r.raise_for_status()
    soup = BeautifulSoup(r.text, "lxml")
    out = []
    for a in soup.select('.list_item .list_title a[data-role="list-title-text"]'):
        title = re.sub(r"\s+", " ", a.get_text(strip=True))
        href = a.get("href") or ""
        if not title or not is_ssd_post(title):
            continue
        if href.startswith("/"):
            href = "https://www.clien.net" + href
        # skip notices/rules
        if "/rule/" in href:
            continue
        out.append({
            "title": title,
            "url": href.split("?")[0],
            "source": "clien",
            "price_krw": parse_price_krw(title),
            "capacity_gb": parse_capacity_gb(title),
        })
    return out


def scrape_jsmon(s):
    url = "https://t.me/s/jsmon"
    r = s.get(url, timeout=30)
    r.raise_for_status()
    soup = BeautifulSoup(r.text, "lxml")
    out = []
    for msg in soup.select(".tgme_widget_message_wrap"):
        text_el = msg.select_one(".tgme_widget_message_text")
        if not text_el:
            continue
        title = re.sub(r"\s+", " ", text_el.get_text(" ", strip=True))
        if not is_ssd_post(title):
            continue
        link_el = msg.select_one(".tgme_widget_message_date")
        href = link_el.get("href") if link_el else url
        # original source link if present
        orig = text_el.select_one("a")
        out.append({
            "title": title[:300],
            "url": href,
            "source": "jsmon-telegram",
            "price_krw": parse_price_krw(title),
            "capacity_gb": parse_capacity_gb(title),
        })
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    s = session()
    candidates = []
    for name, fn in [("clien", scrape_clien), ("jsmon", scrape_jsmon)]:
        try:
            posts = fn(s)
            print(f"[{name}] {len(posts)} ssd posts", file=sys.stderr)
            candidates.extend(posts)
        except Exception as e:
            print(f"[{name}] FAILED: {e}", file=sys.stderr)
        time.sleep(2)

    for c in candidates:
        c["scraped_at"] = datetime.now(timezone.utc).isoformat()

    seen, uniq = set(), []
    for c in candidates:
        if c["url"] not in seen:
            seen.add(c["url"])
            uniq.append(c)

    with open(args.out, "w", encoding="utf-8") as f:
        json.dump({"candidates": uniq,
                   "scraped_at": datetime.now(timezone.utc).isoformat()},
                  f, ensure_ascii=False, indent=1)
    print(f"wrote {len(uniq)} candidates -> {args.out}")


if __name__ == "__main__":
    main()
