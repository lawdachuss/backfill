import json
import os
import sys
import time
import urllib.request
import concurrent.futures
import re
import math

SUPABASE_URL = os.environ.get("SUPABASE_URL", "")
SUPABASE_API_KEY = os.environ.get("SUPABASE_API_KEY", "")
USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"
MAX_WORKERS = 20
PAGE_SIZE = 1000
BATCH_SIZE = 500


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
    while True:
        path = f"/upload_links?host=eq.GoFile&select=url&limit={PAGE_SIZE}&offset={offset}"
        page = query_supabase(path)
        urls = [row["url"].rstrip("/") for row in page]
        links.extend(urls)
        if len(page) < PAGE_SIZE:
            break
        offset += PAGE_SIZE
    return links


def extract_code(url: str) -> str:
    m = re.search(r"gofile\.io/d/([a-zA-Z0-9]+)", url)
    return m.group(1) if m else ""


def keep_alive(url: str) -> dict:
    code = extract_code(url)
    if not code:
        return {"code": "", "status": "error", "error": "no code found in URL"}
    visit_url = f"https://gofile.io/d/{code}"
    req = urllib.request.Request(visit_url)
    req.add_header("User-Agent", USER_AGENT)
    req.add_header("Range", "bytes=0-0")
    start = time.time()
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            resp.read()
        elapsed = time.time() - start
        return {"code": code, "status": "ok", "ms": round(elapsed * 1000)}
    except urllib.error.HTTPError as e:
        if e.code == 206:
            return {"code": code, "status": "ok", "ms": 0}
        return {"code": code, "status": "error", "error": f"HTTP {e.code}"}
    except Exception as e:
        return {"code": code, "status": "error", "error": str(e)}


def process_codes(codes: list[str]) -> dict:
    total = len(codes)
    ok = 0
    errors = []
    results = []

    with concurrent.futures.ThreadPoolExecutor(max_workers=MAX_WORKERS) as ex:
        fut_map = {ex.submit(keep_alive, f"https://gofile.io/d/{c}"): c for c in codes}
        for fut in concurrent.futures.as_completed(fut_map):
            r = fut.result()
            results.append(r)
            if r["status"] == "ok":
                ok += 1
            else:
                errors.append(r)

    return {"total": total, "ok": ok, "errors": errors, "results": results}


def cmd_list():
    links = get_all_gofile_links()
    codes = [extract_code(u) for u in links if extract_code(u)]
    batches = []
    for i in range(0, len(codes), BATCH_SIZE):
        batches.append(codes[i:i + BATCH_SIZE])
    print(json.dumps(batches))
    return batches


def cmd_run(codes: list[str] = None):
    if codes:
        link_codes = codes
    else:
        links = get_all_gofile_links()
        link_codes = [extract_code(u) for u in links if extract_code(u)]

    if not link_codes:
        print(json.dumps({"total": 0, "ok": 0, "errors": []}))
        return

    total = len(link_codes)
    print(f"[gofile-keepalive] Found {total} GoFile links", flush=True)

    all_ok = 0
    all_errors = []

    for i in range(0, total, BATCH_SIZE):
        batch = link_codes[i:i + BATCH_SIZE]
        result = process_codes(batch)
        all_ok += result["ok"]
        all_errors.extend(result["errors"])
        pct = round((i + len(batch)) / total * 100)
        print(
            f"[gofile-keepalive] {pct}% — batch {i//BATCH_SIZE + 1}: "
            f"{result['ok']}/{len(batch)} ok"
            f"{'  ⚠ %d errors' % len(result['errors']) if result['errors'] else ''}",
            flush=True,
        )
        if i + BATCH_SIZE < total:
            time.sleep(1)

    summary = {"total": total, "ok": all_ok, "errors": len(all_errors)}
    print(json.dumps(summary), flush=True)

    if all_errors:
        print("First 10 errors:", json.dumps(all_errors[:10]), flush=True)

    if all_ok == 0 and total > 0:
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
            print("--batch requires a JSON array argument", file=sys.stderr)
            sys.exit(1)
    else:
        cmd_run()
