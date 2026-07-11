import json, os, sys, time, re, urllib.request, urllib.error

SUPABASE_URL = os.environ.get("SUPABASE_URL", "")
SUPABASE_API_KEY = os.environ.get("SUPABASE_API_KEY", "")
GOFILE_PROXY_URL = os.environ.get("GOFILE_PROXY_URL", "").rstrip("/")
PROXY_SOURCE = os.environ.get("PROXY_SOURCE", "").strip()
# Built-in public proxy-list sources. When PROXY_SOURCE is not set, the script
# auto-aggregates proxies from these endpoints so it never hard-depends on a
# manually configured secret. Override with PROXY_SOURCE (comma-separated URLs).
BUILTIN_PROXY_SOURCES = [
    "https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/http.txt",
    "https://raw.githubusercontent.com/ShiftyTR/Proxy-List/master/http.txt",
    "https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/http.txt",
    "https://raw.githubusercontent.com/hookzof/socks5_list/master/http.txt",
    "https://api.proxyscrape.com/v2/?request=getproxies&proxytype=http&timeout=5000&country=all",
    "https://www.proxy-list.download/api/v1/get?type=https",
]
# Max proxies tried per request before giving up (working ones stay sticky).
MAX_PROXY_TRIES = 12
PROXY_TIMEOUT = 12
UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"
PAGE_SIZE = 1000
PROGRESS_FILE = os.path.join(os.path.dirname(os.path.abspath(__file__)), ".progress")


def log(msg: str):
    print(msg, file=sys.stderr, flush=True)


def normalize_proxy(s: str) -> str:
    s = s.strip()
    if not s:
        return ""
    if "://" not in s:
        s = "http://" + s
    return s


def fetch_proxy_list(source: str) -> list:
    """Fetch a pool of egress proxies from a single source URL. Supports both a
    plain-text list (one proxy per line, "ip:port" or "scheme://ip:port") and a
    JSON array of strings. Returns [] when unreachable."""
    try:
        req = urllib.request.Request(source, headers={"User-Agent": UA, "Accept": "*/*"})
        with urllib.request.urlopen(req, timeout=30) as resp:
            body = resp.read().decode()
    except Exception as e:
        log(f"[gofile-keepalive] Proxy list fetch failed ({source}): {e}")
        return []
    out = []
    try:
        arr = json.loads(body)
        if isinstance(arr, list):
            for p in arr:
                n = normalize_proxy(str(p))
                if n:
                    out.append(n)
            log(f"[gofile-keepalive] Loaded {len(out)} proxies (JSON) from {source}")
            return out
    except Exception:
        pass
    for line in body.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if sp := line.split():
            line = sp[0]
        n = normalize_proxy(line)
        if n:
            out.append(n)
    log(f"[gofile-keepalive] Loaded {len(out)} proxies (text) from {source}")
    return out


def fetch_proxy_sources() -> list:
    """Auto-aggregate proxies from PROXY_SOURCE (if set, comma-separated) or the
    built-in public proxy-list endpoints. De-duplicates across sources."""
    sources = []
    if PROXY_SOURCE:
        sources = [s.strip() for s in PROXY_SOURCE.split(",") if s.strip()]
        log("[gofile-keepalive] Using PROXY_SOURCE override for proxy list.")
    else:
        sources = list(BUILTIN_PROXY_SOURCES)
        log("[gofile-keepalive] No PROXY_SOURCE set — auto-fetching built-in proxy lists.")
    seen = set()
    out = []
    for src in sources:
        for p in fetch_proxy_list(src):
            if p not in seen:
                seen.add(p)
                out.append(p)
    log(f"[gofile-keepalive] Aggregated {len(out)} unique proxies from {len(sources)} source(s).")
    return out


class ProxyPool:
    def __init__(self, proxies):
        self.proxies = proxies or []
        self.idx = 0

    def size(self):
        return len(self.proxies)

    def get(self):
        if not self.proxies:
            return None
        return self.proxies[self.idx % len(self.proxies)]

    def next(self):
        if self.proxies:
            self.idx = (self.idx + 1) % len(self.proxies)


def open_request(req, proxy, timeout=30):
    """Open a request, optionally routed through an HTTP(S) proxy."""
    if proxy:
        handler = urllib.request.ProxyHandler({"http": proxy, "https": proxy})
        opener = urllib.request.build_opener(handler)
        return opener.open(req, timeout=timeout)
    return urllib.request.urlopen(req, timeout=timeout)


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


def get_guest_token(proxy_pool=None) -> str:
    # Worker path (Cloudflare bypass) — tried first when configured.
    if GOFILE_PROXY_URL:
        url = f"{GOFILE_PROXY_URL}?action=token"
        req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "text/plain,*/*"})
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                body = resp.read().decode().strip()
                if not body or body == "no-token":
                    log(f"[gofile-keepalive] Token fetch returned: {body}")
                else:
                    return body
        except urllib.error.HTTPError as e:
            log(f"[gofile-keepalive] Token fetch HTTP {e.code}: {e.read().decode()[:200]}")
        except Exception as e:
            log(f"[gofile-keepalive] Token fetch failed: {e}")
        # Fall through to the auto-fetched proxy pool on any worker failure.
        if proxy_pool and proxy_pool.size():
            log("[gofile-keepalive] Worker token fetch failed — falling back to proxy pool.")
    # Direct path through the rotating proxy pool (no worker / worker failed).
    url = "https://api.gofile.io/accounts"
    attempts = min(proxy_pool.size(), MAX_PROXY_TRIES) if (proxy_pool and proxy_pool.size()) else 1
    last_err = ""
    for _ in range(attempts):
        proxy = proxy_pool.get() if proxy_pool else None
        req = urllib.request.Request(url, method="POST", headers={
            "User-Agent": UA, "Origin": "https://gofile.io", "Accept": "application/json",
        })
        try:
            with open_request(req, proxy, PROXY_TIMEOUT) as resp:
                body = resp.read().decode().strip()
                if not body or body == "no-token":
                    log(f"[gofile-keepalive] Token fetch returned: {body}")
                    return ""
                return body
        except urllib.error.HTTPError as e:
            last_err = f"HTTP {e.code} via {proxy}: {e.read().decode()[:120]}"
            log(f"[gofile-keepalive] Token fetch {last_err}")
            if proxy_pool:
                proxy_pool.next()
        except Exception as e:
            last_err = f"{e} via {proxy}"
            log(f"[gofile-keepalive] Token fetch failed: {last_err}")
            if proxy_pool:
                proxy_pool.next()
    return ""


def visit_page(code: str, token: str, proxy_pool=None, timeout: int = 20):
    base = f"{GOFILE_PROXY_URL}?code={code}&token={token}" if GOFILE_PROXY_URL else f"https://gofile.io/d/{code}"
    attempts = min(proxy_pool.size(), MAX_PROXY_TRIES) if (proxy_pool and proxy_pool.size()) else 1
    last = (0, 0.0, "")
    for _ in range(attempts):
        proxy = proxy_pool.get() if proxy_pool else None
        req = urllib.request.Request(base, headers={
            "User-Agent": UA,
            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
            "Accept-Language": "en-US,en;q=0.5",
            "Referer": "https://gofile.io/",
            "DNT": "1",
        })
        start = time.time()
        try:
            with open_request(req, proxy, timeout) as resp:
                body = resp.read().decode().strip()
            return resp.status, time.time() - start, body
        except urllib.error.HTTPError as e:
            b = ""
            try: b = e.read().decode().strip()[:100]
            except: pass
            last = (e.code, time.time() - start, b)
            if proxy_pool:
                proxy_pool.next()
        except Exception as e:
            last = (0, time.time() - start, str(e)[:60])
            if proxy_pool:
                proxy_pool.next()
    return last


def fetch_guest_token_with_retry(proxy_pool=None, max_attempts: int = 6) -> str:
    """Fetch a GoFile guest token, retrying transient failures (e.g. Cloudflare
    5xx while the API is flapping) with exponential backoff. Returns '' only
    after exhausting all attempts. Each attempt rotates to the next proxy."""
    for attempt in range(1, max_attempts + 1):
        token = get_guest_token(proxy_pool)
        if token:
            return token
        wait = min(2 ** attempt, 60)
        log(f"[gofile-keepalive] Guest token fetch failed (attempt {attempt}/{max_attempts}), "
            f"retrying in {wait}s…")
        if attempt < max_attempts:
            time.sleep(wait)
    return ""


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

    # Build the egress proxy pool. The GOFILE_PROXY_URL worker takes priority
    # when set; otherwise we auto-fetch a rotating pool (PROXY_SOURCE override or
    # built-in public lists) so the job never depends on a manual secret.
    proxy_pool = ProxyPool(fetch_proxy_sources())
    if GOFILE_PROXY_URL:
        log("[gofile-keepalive] Using GOFILE_PROXY_URL worker (primary path).")
    elif proxy_pool.size():
        log(f"[gofile-keepalive] No worker set — using {proxy_pool.size()} auto-fetched proxies.")
    else:
        log("[gofile-keepalive] WARNING: no GOFILE_PROXY_URL and no proxies fetched — "
            "hitting GoFile directly (may be Cloudflare-blocked).")

    # Get guest token once, reuse for all codes (retry transient 5xx)
    log("[gofile-keepalive] Getting guest token...")
    token = fetch_guest_token_with_retry(proxy_pool)
    if not token:
        log("[gofile-keepalive] FATAL: could not get guest token after retries")
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
            token = fetch_guest_token_with_retry(proxy_pool)
            if not token:
                log("[gofile-keepalive] FATAL: could not refresh guest token after retries")
                sys.exit(1)
            need_token_refresh = False

        for attempt in range(3):
            status, elapsed, body = visit_page(code, token, proxy_pool)

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
