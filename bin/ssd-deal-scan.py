#!/usr/bin/env python3
"""
SSD hot-deal collector.

Scrapes Korean deal sources for SSD posts and emits JSON candidates
for the cron worker to judge (초특가 판정).

Sources (plain HTTP, verified working from sandbox egress):
  - clien 알뜰구매: https://www.clien.net/service/board/jirum
  - Telegram 정세몬 (뽐뿌/퀘존/쿨엔조이 미러): https://t.me/s/jsmon
  - danawa 통합검색: https://search.danawa.com/dsearch.php?query=SSD
    (상품명/최저가/pcode 링크 파싱)
  - AliExpress: https://www.aliexpress.com/wholesale?SearchText=ssd+nvme
    (상품 카드 파싱, USD->KRW 환산. 대부분 듣보 브랜드라 품질필터에서 대거 제외됨)

Blocked from this network (documented, not retried):
  - ppomppu.co.kr: 403, arca.live: Cloudflare, quasarzone.com: Cloudflare,
    coolenjoy.net / ruliweb.com: unreachable
  - coupang.com: 403 (bot protection, 2026-10-04 확인). 쿠팡 가격 모니터링은
    폴센트(Fallcent) 앱을 사용자가 직접 설치해 관심 상품 등록하는 방식으로 안내.

Quality filter (2026-10-04, owner request):
  - Every candidate gets quality_ok / quality_note via check_quality().
  - TRUSTED_BRANDS allowlist: 유명 제조사만 통과.
  - EXCLUDE_KEYWORDS: 벌크/OEM/리퍼/병행수입 등 품질보증 불가 표기는 무조건 제외.
  - The cron judge must drop quality_ok=false candidates.

Usage:
  ssd-deal-scan.py --out /path/to/candidates.json

Proxy: standard *_PROXY env vars (credential-free or not; requests handles it).
"""
import argparse
import html
import json
import re
import sys
import time
from datetime import datetime, timezone
from urllib.parse import unquote

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


# --- brand / warranty quality filter (owner request 2026-10-04) ---
# Only well-known manufacturers with proper warranty pass. Anything else
# (no-name brands, bulk/OEM/refurb/parallel-import markings) is excluded.
TRUSTED_BRANDS = [
    "samsung", "삼성",
    "sk hynix", "sk하이닉스", "하이닉스", "skhynix",
    "western digital", "웨스턴디지털", "wd black", "wd blue", "wd green",
    "sandisk", "샌디스크",
    "crucial", "크루셜", "micron", "마이크론",
    "kingston", "킹스턴",
    "seagate", "시게이트",
    "kioxia", "키오시아",
    "adata", "에이데이타",
    "transcend", "트랜센드",
    "pny", "sabrent",
    "teamgroup", "팀그룹", "team group",
    "silicon power", "실리콘파워",
    "patriot", "패트리엇",
    "corsair", "커세어",
    "lexar", "렉사",
    "msi",
    "gigabyte", "기가바이트",
    "essencore", "에센코어", "klevv",
    "netac",
    "hiksemi", "hikvision",
    "colorful", "컬러풀",
]
EXCLUDE_KEYWORDS = [
    "벌크", "bulk", "oem", "리퍼", "refurb", "재생",
    "병행", "병행수입",
]


def check_quality(title: str):
    """Return (quality_ok, quality_note) for an SSD candidate title."""
    t = title.lower()
    for kw in EXCLUDE_KEYWORDS:
        if kw in t:
            return False, "품질보증 불가 표기(%s)" % kw
    for b in TRUSTED_BRANDS:
        if b in t:
            return True, "브랜드 확인(%s)" % b
    return False, "미확인 제조사"


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


def scrape_danawa(s):
    """Danawa 통합검색 상품 목록: 상품명/최저가/pcode 링크."""
    url = "https://search.danawa.com/dsearch.php?query=SSD"
    r = s.get(url, timeout=40)
    r.raise_for_status()
    soup = BeautifulSoup(r.text, "lxml")
    out = []
    for li in soup.select('li.prod_item[id^="productItem"]'):
        pid = li["id"].replace("productItem", "")
        pel = li.select_one('input[id^="min_price_"]')
        price = None
        if pel and (pel.get("value") or "").isdigit():
            price = int(pel["value"])
        a = li.select_one('a[class*="click_log_product_standard_title_"]')
        title = re.sub(r"\s+", " ", a.get_text(strip=True)) if a else ""
        if not title or not is_ssd_post(title):
            continue
        out.append({
            "title": title,
            "url": "https://prod.danawa.com/info/?pcode=%s" % pid,
            "source": "danawa",
            "price_krw": price,
            "capacity_gb": parse_capacity_gb(title),
        })
    return out


def scrape_aliexpress(s):
    """AliExpress SSD search cards: title / USD price / item link."""
    url = "https://www.aliexpress.com/wholesale?SearchText=ssd+nvme+1tb"
    r = s.get(url, timeout=40)
    r.raise_for_status()
    out = []
    cards = re.findall(
        r'href="(//www\.aliexpress\.(?:com|us)/item/(\d+)\.html\?'
        r'[^"]*?pdp_npi=([^"&]*)[^"]*)"[^>]*>(.*?)</a>',
        r.text, re.S)
    for href, pid, npi, inner in cards:
        npi_d = unquote(npi)  # e.g. 6@dis!USD!70.32!25.55!...
        m = re.search(r"!USD!([\d.]+)!([\d.]+)", npi_d)
        price_krw = int(float(m.group(2)) * 1380) if m else None
        text = html.unescape(re.sub(r"<[^>]+>", " ", inner))
        text = re.sub(r"\s+", " ", text).strip()
        # title is the text before the price part
        title = re.split(r"\$\s*\d", text, maxsplit=1)[0].strip()[:200]
        if not title or not is_ssd_post(title):
            continue
        out.append({
            "title": title,
            "url": "https://www.aliexpress.com/item/%s.html" % pid,
            "source": "aliexpress",
            "price_krw": price_krw,
            "capacity_gb": parse_capacity_gb(title),
        })
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    s = session()
    candidates = []
    for name, fn in [("clien", scrape_clien), ("jsmon", scrape_jsmon),
                     ("danawa", scrape_danawa), ("aliexpress", scrape_aliexpress)]:
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

    for c in uniq:
        ok, note = check_quality(c["title"])
        c["quality_ok"] = ok
        c["quality_note"] = note

    # hard-exclude no-name / no-warranty items (owner request 2026-10-04);
    # keep them listed under "excluded" for transparency
    kept = [c for c in uniq if c["quality_ok"]]
    excluded = [{"title": c["title"], "url": c["url"], "source": c["source"],
                 "price_krw": c["price_krw"], "quality_note": c["quality_note"]}
                for c in uniq if not c["quality_ok"]]

    with open(args.out, "w", encoding="utf-8") as f:
        json.dump({"candidates": kept, "excluded": excluded,
                   "excluded_count": len(excluded),
                   "scraped_at": datetime.now(timezone.utc).isoformat()},
                  f, ensure_ascii=False, indent=1)
    print(f"wrote {len(kept)} candidates (+{len(excluded)} excluded) -> {args.out}")


if __name__ == "__main__":
    main()
