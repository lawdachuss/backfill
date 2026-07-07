import json
import os
import sys
import time
import urllib.request
import concurrent.futures
import re

SUPABASE_URL = os.environ.get("SUPABASE_URL", "")
SUPABASE_API_KEY = os.environ.get("SUPABASE_API_KEY", "")
USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"
MAX_WORKERS = 15
PAGE_SIZE = 1000
BATCH_SIZE = 500


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


def keep_alive(code: str) -> dict:
    visit_url = f"https://gofile.io/d/{code}"
    req = urllib.request.Request(visit_url)
    req.add_header("User-Agent", USER_AGENT)
    start = time.time()
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            resp.read()
        elapsed = time.time() - start
        return {"code": code, "status": "ok", "ms": round(elapsed * 1000), "http": resp.status}
    except urllib.error.HTTPError as e:
        elapsed = time.time() - start
        return {"code": code, "status": "error", "error": f"HTTP {e.code}", "ms": round(elapsed * 1000)}
    except urllib.error.URLError as e:
        elapsed = time.time() - start
        return {"code": code, "status": "error", "error": f"URL {e.reason}", "ms": round(elapsed * 1000)}
    except Exception as e:
        elapsed = time.time() - start
        return {"code": code, "status": "error", "error": str(e)[:60], "ms": round(elapsed * 1000)}


def process_batch(batch_num: int, codes: list[str]) -> dict:
    total = len(codes)
    ok = 0
    errors = []

    log(f"[gofile-keepalive]   Starting batch {batch_num} ({total} codes, {MAX_WORKERS} workers)...")

    with concurrent.futures.ThreadPoolExecutor(max_workers=MAX_WORKERS) as ex:
        fut_map = {ex.submit(keep_alive, c): c for c in codes}
        done = 0
        for fut in concurrent.futures.as_completed(fut_map):
            r = fut.result()
            done += 1
            if r["status"] == "ok":
                ok += 1
            else:
                errors.append(r)
            if done % 100 == 0 or done == total:
                log(f"[gofile-keepalive]     {done}/{total} processed ({ok} ok, {len(errors)} errors)")

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
        log(f"[gofile-keepalive] Processing {len(link_codes)} codes from --batch argument")
    else:
        log("[gofile-keepalive] Fetching all GoFile links from Supabase...")
        links = get_all_gofile_links()
        link_codes = [extract_code(u) for u in links if extract_code(u)]
        log(f"[gofile-keepalive] Found {len(link_codes)} GoFile links total")

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
        log(f"[gofile-keepalive] Batch {batch_num}: processing {len(batch)} codes ({i+1}-{i+len(batch)} of {total})")
        result = process_batch(batch_num, batch)
        all_ok += result["ok"]
        all_errors.extend(result["errors"])
        pct = round((i + len(batch)) / total * 100)
        log(f"[gofile-keepalive] Overall: {pct}% — {all_ok}/{i+len(batch)} ok, {len(all_errors)} errors")
        if i + BATCH_SIZE < total:
            log(f"[gofile-keepalive] Waiting 2s before next batch...")
            time.sleep(2)
        batch_num += 1

    summary = {"total": total, "ok": all_ok, "errors": len(all_errors)}
    log(f"[gofile-keepalive] Done: {json.dumps(summary)}")
    print(json.dumps(summary), flush=True)

    if all_errors:
        log(f"[gofile-keepalive] First 10 errors:")
        for e in all_errors[:10]:
            log(f"  {e['code']}: {e['error']}")

    if all_ok == 0 and total > 0:
        log("[gofile-keepalive] FATAL: zero successes, exiting with code 1")
        sys.exit(1)


if __name__ == "__main__":
    if "--list" in sys.argv:
        cmd_list()
    elif "--batch" in sys.argv:
        idx = sys.argv.index("--batch")
        if idx + 1 < len(sys.argv):
            raw = sys.argv[idx + 1]
            codes = json.loads(raw)
            cmd_run(codes)
        else:
            log("--batch requires a JSON array argument")
            sys.exit(1)
    else:
        cmd_run()
