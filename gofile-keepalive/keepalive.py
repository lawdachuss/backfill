import json
import os
import sys
import time
import urllib.request
import re

SUPABASE_URL = os.environ.get("SUPABASE_URL", "")
SUPABASE_API_KEY = os.environ.get("SUPABASE_API_KEY", "")
USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"
PAGE_SIZE = 1000
BATCH_SIZE = 500
MIN_DELAY = 1.0


def log(msg: str):
    print(msg, file=sys.stderr, flush=True)


def query_supabase(path: str) -> list:
    url = f"{SUPABASE_URL}/rest/v1{path}"
    req = urllib.request.Request(url)
    req.add_header("apikey", SUPABASE_API_KEY)
    req.add_header("Authorization", f"Bearer {SUPABASE_API_KEY}")
    req.add_header("Accept", "application/json")
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode())


def get_all_gofile_links() -> list[str]:
    links = []
    offset = 0
    page_num = 1
    while True:
        log(f"[gofile-keepalive] Fetching Supabase page {page_num}...")
        path = f"/upload_links?host=eq.GoFile&select=url&limit={PAGE_SIZE}&offset={offset}"
        page = query_supabase(path)
        urls = [row["url"].rstrip("/") for row in page]
        links.extend(urls)
        log(f"[gofile-keepalive]   Page {page_num}: {len(page)} links (total: {len(links)})")
        if len(page) < PAGE_SIZE:
            break
        offset += PAGE_SIZE
        page_num += 1
    return links


def extract_code(url: str) -> str:
    m = re.search(r"gofile\.io/d/([a-zA-Z0-9]+)", url)
    return m.group(1) if m else ""


def process_batch(batch_num: int, codes: list[str]) -> dict:
    total = len(codes)
    ok = 0
    errors = []
    delay = MIN_DELAY
    consecutive_ok = 0

    log(f"[gofile-keepalive]   Starting batch {batch_num} ({total} codes, sequential, {delay}s initial delay)...")

    for i, code in enumerate(codes):
        visit_url = f"https://gofile.io/d/{code}"
        req = urllib.request.Request(visit_url)
        req.add_header("User-Agent", USER_AGENT)
        start = time.time()

        for attempt in range(4):
            try:
                with urllib.request.urlopen(req, timeout=20) as resp:
                    resp.read()
                elapsed = time.time() - start
                ok += 1
                consecutive_ok += 1
                if consecutive_ok >= 20 and delay > MIN_DELAY:
                    delay = max(MIN_DELAY, delay - 0.2)
                    log(f"[gofile-keepalive]     No 429s for 20 — reducing delay to {delay:.1f}s")
                    consecutive_ok = 0
                break
            except urllib.error.HTTPError as e:
                if e.code == 429:
                    wait = (attempt + 1) * 6
                    delay += 0.5
                    consecutive_ok = 0
                    log(f"[gofile-keepalive]     429 on {code}, retry {attempt+1}/4 in {wait}s (delay now {delay:.1f}s)...")
                    time.sleep(wait)
                    continue
                elapsed = time.time() - start
                errors.append({"code": code, "error": f"HTTP {e.code}", "ms": round(elapsed * 1000)})
                consecutive_ok = 0
                break
            except urllib.error.URLError as e:
                elapsed = time.time() - start
                errors.append({"code": code, "error": f"URL {e.reason}", "ms": round(elapsed * 1000)})
                consecutive_ok = 0
                break
            except Exception as e:
                elapsed = time.time() - start
                errors.append({"code": code, "error": str(e)[:60], "ms": round(elapsed * 1000)})
                consecutive_ok = 0
                break
        else:
            errors.append({"code": code, "error": "429 exceeded retries", "ms": round((time.time() - start) * 1000)})
            consecutive_ok = 0

        if (i + 1) % 100 == 0 or i == total - 1:
            speed = (i + 1) / (time.time() - batch_start + 0.001)
            eta_s = (total - i - 1) / (speed + 0.001)
            etas = f"{eta_s/60:.0f}m" if eta_s < 3600 else f"{eta_s/3600:.1f}h"
            log(f"[gofile-keepalive]     {i+1}/{total} — {ok} ok, {len(errors)} errors — {speed:.1f}/s — ETA {etas}")

        time.sleep(delay)

    log(f"[gofile-keepalive]   Batch {batch_num} done: {ok}/{total} ok, {len(errors)} errors")
    return {"total": total, "ok": ok, "errors": errors}


def cmd_list():
    links = get_all_gofile_links()
    codes = [extract_code(u) for u in links if extract_code(u)]
    log(f"[gofile-keepalive] Total unique codes: {len(codes)}")
    batches = []
    for i in range(0, len(codes), BATCH_SIZE):
        batches.append(codes[i:i + BATCH_SIZE])
    log(f"[gofile-keepalive] Split into {len(batches)} batches of up to {BATCH_SIZE}")
    print(json.dumps(batches), flush=True)
    return batches


def cmd_run(codes: list[str] = None):
    if codes:
        link_codes = codes
        log(f"[gofile-keepalive] Processing {len(link_codes)} codes")
    else:
        log("[gofile-keepalive] Fetching all GoFile links from Supabase...")
        links = get_all_gofile_links()
        link_codes = [extract_code(u) for u in links if extract_code(u)]
        log(f"[gofile-keepalive] Found {len(link_codes)} GoFile links total")

    global batch_start
    batch_start = time.time()

    if not link_codes:
        log("[gofile-keepalive] No links to process")
        print(json.dumps({"total": 0, "ok": 0, "errors": []}), flush=True)
        return

    all_ok = 0
    all_errors = []
    batch_num = 1

    total = len(link_codes)
    for i in range(0, total, BATCH_SIZE):
        batch = link_codes[i:i + BATCH_SIZE]
        log(f"[gofile-keepalive] Batch {batch_num}: {len(batch)} codes ({i+1}-{i+len(batch)} of {total})")
        result = process_batch(batch_num, batch)
        all_ok += result["ok"]
        all_errors.extend(result["errors"])
        elapsed = time.time() - batch_start
        log(f"[gofile-keepalive] Overall: {all_ok}/{total} ok, {len(all_errors)} errors ({elapsed/60:.0f}m elapsed)")
        if i + BATCH_SIZE < total:
            log(f"[gofile-keepalive] Waiting 3s before next batch...")
            time.sleep(3)
        batch_num += 1

    summary = {"total": total, "ok": all_ok, "errors": len(all_errors)}
    elapsed = time.time() - batch_start
    log(f"[gofile-keepalive] Done in {elapsed/60:.0f}m: {json.dumps(summary)}")
    print(json.dumps(summary), flush=True)

    if all_errors:
        log(f"[gofile-keepalive] First 10 errors:")
        for e in all_errors[:10]:
            log(f"  {e['code']}: {e['error']}")

    if all_ok == 0 and total > 0:
        log("[gofile-keepalive] FATAL: zero successes, exiting with code 1")
        sys.exit(1)


batch_start = 0.0

if __name__ == "__main__":
    if "--list" in sys.argv:
        cmd_list()
    elif "--batch-env" in sys.argv:
        raw = os.environ.get("BATCH_CODES", "")
        if raw:
            codes = json.loads(raw)
            cmd_run(codes)
        else:
            log("BATCH_CODES env var is empty or not set")
            sys.exit(1)
    else:
        cmd_run()
