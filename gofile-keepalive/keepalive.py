import json, os, sys, time, re, urllib.request, urllib.error

SUPABASE_URL = os.environ.get("SUPABASE_URL", "")
SUPABASE_API_KEY = os.environ.get("SUPABASE_API_KEY", "")
GOFILE_PROXY_URL = os.environ.get("GOFILE_PROXY_URL", "").rstrip("/")
UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"
PAGE_SIZE = 1000


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


def visit_page(code: str, timeout: int = 30):
    url = f"{GOFILE_PROXY_URL}?code={code}" if GOFILE_PROXY_URL else f"https://gofile.io/d/{code}"
    req = urllib.request.Request(url, headers={
        "User-Agent": UA,
        "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
        "Accept-Language": "en-US,en;q=0.5",
        "Referer": "https://gofile.io/",
        "DNT": "1",
    })
    start = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read().decode().strip()
        return resp.status, time.time() - start, body
    except urllib.error.HTTPError as e:
        b = ""
        try: b = e.read().decode().strip()[:100]
        except: pass
        return e.code, time.time() - start, b
    except Exception as e:
        return 0, time.time() - start, str(e)[:60]


def run():
    log("[gofile-keepalive] Fetching all GoFile links from Supabase...")
    links = get_all_gofile_links()
    codes = [extract_code(u) for u in links if extract_code(u)]
    log(f"[gofile-keepalive] Found {len(codes)} GoFile codes")

    if not codes:
        log("[gofile-keepalive] No codes to process")
        return

    ok = 0
    errors = []
    delay = 1.5
    total = len(codes)
    start_time = time.time()

    for i, code in enumerate(codes):
        log(f"[gofile-keepalive]   {i+1}/{total}: {code}")

        for attempt in range(3):
            status, elapsed, body = visit_page(code)

            if status == 200 or status == 206:
                ok += 1
                delay = max(1.0, delay - 0.1)
                log(f"[gofile-keepalive]     ok ({elapsed*1000:.0f}ms) delay={delay:.1f}s")
                break
            elif status == 429:
                wait = (attempt + 1) * 8
                delay = min(delay + 2.0, 10.0)
                log(f"[gofile-keepalive]     429 — retry {attempt+1}/3 in {wait}s, delay now {delay:.1f}s")
                time.sleep(wait)
            elif status == 0:
                log(f"[gofile-keepalive]     conn fail: {body}")
                errors.append({"code": code, "error": f"conn {body}"})
                break
            else:
                log(f"[gofile-keepalive]     HTTP {status} ({elapsed*1000:.0f}ms) {body}")
                errors.append({"code": code, "error": f"HTTP {status}"})
                break

        elapsed_total = time.time() - start_time
        rate = (i + 1) / elapsed_total if elapsed_total > 0 else 0
        log(f"[gofile-keepalive]   progress: {ok}/{i+1} ok, {len(errors)} err, "
            f"{elapsed_total/60:.1f}m elapsed, {rate:.1f} req/min")

        if i < total - 1:
            time.sleep(delay)

    summary = {"total": total, "ok": ok, "errors": len(errors)}
    log(f"[gofile-keepalive] Done in {(time.time()-start_time)/60:.0f}m: {json.dumps(summary)}")
    print(json.dumps(summary), flush=True)

    if errors:
        log(f"[gofile-keepalive] First 10 errors:")
        for e in errors[:10]:
            log(f"  {e['code']}: {e['error']}")

    if ok == 0 and total > 0:
        log("[gofile-keepalive] FATAL: zero successes")
        sys.exit(1)


if __name__ == "__main__":
    run()
