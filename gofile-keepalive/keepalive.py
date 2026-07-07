import json, os, sys, time, re, urllib.request, urllib.error

SUPABASE_URL = os.environ.get("SUPABASE_URL", "")
SUPABASE_API_KEY = os.environ.get("SUPABASE_API_KEY", "")
GOFILE_PROXY_URL = os.environ.get("GOFILE_PROXY_URL", "").rstrip("/")
UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"
PAGE_SIZE = 1000
PROGRESS_FILE = os.path.join(os.path.dirname(os.path.abspath(__file__)), ".progress")


def log(msg: str):
    print(msg, file=sys.stderr, flush=True)


def load_progress():
    try:
        with open(PROGRESS_FILE) as f:
            return json.load(f)
    except:
        return None


def save_progress(codes: list, index: int, errors: list, state: str = "running"):
    os.makedirs(os.path.dirname(PROGRESS_FILE), exist_ok=True)
    with open(PROGRESS_FILE, "w") as f:
        json.dump({
            "codes": codes,
            "index": index,
            "errors": errors[-50:],
            "state": state,
            "updated_at": time.time(),
        }, f)


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


def get_guest_token() -> str:
    if GOFILE_PROXY_URL:
        url = f"{GOFILE_PROXY_URL}?action=token"
        req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "text/plain,*/*"})
    else:
        url = "https://api.gofile.io/accounts"
        req = urllib.request.Request(url, method="POST", headers={
            "User-Agent": UA, "Origin": "https://gofile.io", "Accept": "application/json",
        })
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            body = resp.read().decode().strip()
            if not body or body == "no-token":
                log(f"[gofile-keepalive] Token fetch returned: {body}")
                return ""
            return body
    except urllib.error.HTTPError as e:
        log(f"[gofile-keepalive] Token fetch HTTP {e.code}: {e.read().decode()[:200]}")
        return ""
    except Exception as e:
        log(f"[gofile-keepalive] Token fetch failed: {e}")
        return ""


def visit_page(code: str, token: str, timeout: int = 30):
    base = f"{GOFILE_PROXY_URL}?code={code}&token={token}" if GOFILE_PROXY_URL else f"https://gofile.io/d/{code}"
    req = urllib.request.Request(base, headers={
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
    total = len(codes)
    log(f"[gofile-keepalive] Found {total} GoFile codes")

    if not codes:
        log("[gofile-keepalive] No codes to process")
        return

    # Load progress and determine start index
    progress = load_progress()
    start_index = 0
    errors = []

    if progress and progress.get("codes") == codes:
        if progress.get("state") == "running" and progress.get("index", 0) < total:
            start_index = progress["index"]
            errors = progress.get("errors", [])
            log(f"[gofile-keepalive] Resuming from code {start_index}/{total}")
            log(f"[gofile-keepalive] Previous errors carried over: {len(errors)}")
        else:
            log("[gofile-keepalive] Previous cycle completed — starting fresh")
    else:
        log("[gofile-keepalive] No progress or codes changed — starting fresh")

    # Get guest token once, reuse for all codes
    log("[gofile-keepalive] Getting guest token...")
    token = get_guest_token()
    if not token:
        log("[gofile-keepalive] FATAL: could not get guest token")
        sys.exit(1)
    log(f"[gofile-keepalive] Got guest token: {token[:20]}...")

    # Reset progress to running state
    save_progress(codes, start_index, errors, "running")

    ok = 0
    need_token_refresh = False
    delay = 1.5
    start_time = time.time()

    for i in range(start_index, total):
        code = codes[i]

        if need_token_refresh:
            log("[gofile-keepalive] Refreshing guest token...")
            token = get_guest_token()
            if not token:
                log("[gofile-keepalive] FATAL: could not refresh guest token")
                sys.exit(1)
            need_token_refresh = False

        for attempt in range(3):
            status, elapsed, body = visit_page(code, token)

            if status == 200 or status == 206:
                ok += 1
                delay = max(1.0, delay - 0.1)
                log(f"[gofile-keepalive]   {i+1}/{total}: {code} ok ({elapsed*1000:.0f}ms) delay={delay:.1f}s")
                break
            elif status == 429:
                wait = (attempt + 1) * 8
                delay = min(delay + 2.0, 10.0)
                log(f"[gofile-keepalive]   {i+1}/{total}: {code} 429 — retry {attempt+1}/3 in {wait}s")
                time.sleep(wait)
            elif status == 502 and body == "no-token":
                log(f"[gofile-keepalive]   {i+1}/{total}: {code} token expired — will refresh")
                need_token_refresh = True
                errors.append({"code": code, "error": "token expired"})
                break
            elif status == 0:
                log(f"[gofile-keepalive]   {i+1}/{total}: {code} conn fail: {body}")
                errors.append({"code": code, "error": f"conn {body}"})
                break
            else:
                log(f"[gofile-keepalive]   {i+1}/{total}: {code} HTTP {status} ({elapsed*1000:.0f}ms) {body}")
                errors.append({"code": code, "error": f"HTTP {status}"})
                break

        elapsed_total = time.time() - start_time
        rate = (i + 1 - start_index) / elapsed_total if elapsed_total > 0 else 0
        log(f"[gofile-keepalive]   progress: {ok}/{i+1-start_index} ok, {len(errors)} err, "
            f"{elapsed_total/60:.1f}m elapsed, {rate:.1f} req/min")

        # Save progress every 50 codes
        if (i + 1) % 50 == 0:
            save_progress(codes, i + 1, errors, "running")
            log(f"[gofile-keepalive]   progress saved at {i+1}/{total}")

        if i < total - 1:
            time.sleep(delay)

    # All done — mark complete so next run starts fresh
    summary = {"total": total, "ok": ok, "errors": len(errors)}
    elapsed_min = (time.time() - start_time) / 60
    log(f"[gofile-keepalive] Done in {elapsed_min:.0f}m: {json.dumps(summary)}")

    save_progress(codes, total, errors, "completed")

    if errors:
        log(f"[gofile-keepalive] Errors ({len(errors)}):")
        for e in errors[:10]:
            log(f"  {e['code']}: {e['error']}")

    if ok == 0 and total > 0:
        log("[gofile-keepalive] FATAL: zero successes")
        sys.exit(1)

    log("[gofile-keepalive] All done — next run will start fresh")


if __name__ == "__main__":
    run()
