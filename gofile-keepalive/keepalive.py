import json
import os
import sys
import time
import re
import random
import urllib.request
import urllib.error

SUPABASE_URL = os.environ.get("SUPABASE_URL", "")
SUPABASE_API_KEY = os.environ.get("SUPABASE_API_KEY", "")
USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"
PAGE_SIZE = 1000
BATCH_SIZE = 500
MIN_DELAY = 1.0


def log(msg: str):
    print(msg, file=sys.stderr, flush=True)


def fetch_proxies() -> list[str]:
    urls = [
        "https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/http.txt",
        "https://raw.githubusercontent.com/ShiftyTR/Proxy-List/master/http.txt",
        "https://raw.githubusercontent.com/roosterkid/openproxylist/main/HTTPS_RAW.txt",
        "https://api.proxyscrape.com/v2/?request=getproxies&protocol=http&timeout=5000&country=all&simplified=true",
    ]
    all_proxies = set()
    for url in urls:
        try:
            req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
            with urllib.request.urlopen(req, timeout=10) as resp:
                for line in resp.read().decode().strip().splitlines():
                    line = line.strip()
                    if line and ":" in line:
                        all_proxies.add(line)
        except Exception:
            pass
    result = list(all_proxies)
    random.shuffle(result)
    return result


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


def try_opener(url: str, proxy: str = None, timeout: int = 15):
    if proxy:
        handler = urllib.request.ProxyHandler({
            "http": f"http://{proxy}",
            "https": f"http://{proxy}",
        })
    else:
        handler = urllib.request.ProxyHandler({})
    opener = urllib.request.build_opener(handler)
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
    start = time.time()
    try:
        with opener.open(req, timeout=timeout) as resp:
            resp.read()
        return resp.status, time.time() - start, None
    except urllib.error.HTTPError as e:
        return e.code, time.time() - start, None
    except Exception as e:
        return 0, time.time() - start, str(e)


def process_batch(batch_num: int, codes: list[str], proxies: list[str]) -> dict:
    total = len(codes)
    ok = 0
    errors = []
    delay = MIN_DELAY
    consecutive_ok = 0
    pi = 0
    bad_proxies = set()
    no_proxy_once = True

    proxy_mode = len(proxies) > 0
    if proxy_mode:
        log(f"[gofile-keepalive]   Starting batch {batch_num} ({total} codes, {len(proxies)} proxies round-robin)")
    else:
        log(f"[gofile-keepalive]   Starting batch {batch_num} ({total} codes, direct connection)")

    for i, code in enumerate(codes):
        url = f"https://gofile.io/d/{code}"

        if proxy_mode:
            proxy = None
            for attempt in range(3):
                for _ in range(50):
                    p = proxies[pi % len(proxies)]
                    pi += 1
                    if p not in bad_proxies:
                        proxy = p
                        break

                status, elapsed, err_str = try_opener(url, proxy)

                if status == 200 or status == 206:
                    ok += 1
                    consecutive_ok += 1
                    if consecutive_ok >= 20 and delay > MIN_DELAY:
                        delay = max(MIN_DELAY, delay - 0.3)
                        consecutive_ok = 0
                    break
                elif status == 429:
                    wait = (attempt + 1) * 6
                    delay += 1.0
                    consecutive_ok = 0
                    log(f"[gofile-keepalive]     429 on {code} via {proxy}, retry {attempt+1}/3 in {wait}s (delay={delay:.1f}s)")
                    time.sleep(wait)
                elif status == 0:
                    bad_proxies.add(proxy)
                    if attempt < 2:
                        continue
                    errors.append({"code": code, "error": f"proxy {err_str or 'fail'}"})
                    consecutive_ok = 0
                    break
                else:
                    errors.append({"code": code, "error": f"HTTP {status}", "ms": round(elapsed * 1000)})
                    consecutive_ok = 0
                    break
            else:
                errors.append({"code": code, "error": "429 exceeded retries"})
                consecutive_ok = 0
        else:
            status, elapsed, err_str = try_opener(url, None)
            if status == 200 or status == 206:
                ok += 1
            elif status == 429:
                if no_proxy_once:
                    log(f"[gofile-keepalive]     429 without proxy — trying proxies as fallback...")
                    no_proxy_once = False
                    if not proxies:
                        log("[gofile-keepalive]     No proxies available, retrying directly...")
                        time.sleep(10)
                        status2, _, _ = try_opener(url, None, timeout=20)
                        if status2 == 200 or status2 == 206:
                            ok += 1
                        else:
                            errors.append({"code": code, "error": f"429 direct"})
                    else:
                        proxy_mode = True
                        pi = 0
                        continue
                else:
                    errors.append({"code": code, "error": "429 direct"})
            elif status == 0:
                errors.append({"code": code, "error": f"conn {err_str}"})
            else:
                errors.append({"code": code, "error": f"HTTP {status}", "ms": round(elapsed * 1000)})

        if (i + 1) % 100 == 0 or i == total - 1:
            speed = (i + 1) / (time.time() - global_batch_start + 0.001)
            eta_s = (total - i - 1) / (speed + 0.001) if speed > 0 else 0
            etas = f"{eta_s/60:.0f}m" if eta_s < 3600 else f"{eta_s/3600:.1f}h"
            pd = "proxy" if proxy_mode else "direct"
            log(f"[gofile-keepalive]     {i+1}/{total} — {ok} ok, {len(errors)} err — {speed:.2f}/s — ETA {etas} — {pd} — delay={delay:.1f}s")

        time.sleep(delay)

    if proxy_mode:
        log(f"[gofile-keepalive]   Bad proxies discarded: {len(bad_proxies)}/{len(proxies)}")
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
    global global_batch_start

    if codes:
        link_codes = codes
        log(f"[gofile-keepalive] Processing {len(link_codes)} codes")
    else:
        log("[gofile-keepalive] Fetching all GoFile links from Supabase...")
        links = get_all_gofile_links()
        link_codes = [extract_code(u) for u in links if extract_code(u)]
        log(f"[gofile-keepalive] Found {len(link_codes)} GoFile links total")

    if not link_codes:
        log("[gofile-keepalive] No links to process")
        print(json.dumps({"total": 0, "ok": 0, "errors": []}), flush=True)
        return

    proxies = []
    if "--no-proxies" not in sys.argv:
        log("[gofile-keepalive] Fetching proxy list...")
        raw = fetch_proxies()
        proxies = raw
        log(f"[gofile-keepalive] Loaded {len(proxies)} proxies (no pre-test, on-demand fallback)")
    else:
        log("[gofile-keepalive] Proxies disabled via --no-proxies")

    all_ok = 0
    all_errors = []
    batch_num = 1
    global_batch_start = time.time()

    total = len(link_codes)
    for i in range(0, total, BATCH_SIZE):
        batch = link_codes[i:i + BATCH_SIZE]
        log(f"[gofile-keepalive] Batch {batch_num}: {len(batch)} codes ({i+1}-{i+len(batch)} of {total})")
        result = process_batch(batch_num, batch, proxies)
        all_ok += result["ok"]
        all_errors.extend(result["errors"])
        elapsed = time.time() - global_batch_start
        log(f"[gofile-keepalive] Overall: {all_ok}/{total} ok, {len(all_errors)} errors ({elapsed/60:.0f}m elapsed)")
        if i + BATCH_SIZE < total:
            time.sleep(3)
        batch_num += 1

    summary = {"total": total, "ok": all_ok, "errors": len(all_errors)}
    elapsed = time.time() - global_batch_start
    log(f"[gofile-keepalive] Done in {elapsed/60:.0f}m: {json.dumps(summary)}")
    print(json.dumps(summary), flush=True)

    if all_errors:
        log(f"[gofile-keepalive] First 10 errors:")
        for e in all_errors[:10]:
            log(f"  {e['code']}: {e['error']}")

    if all_ok == 0 and total > 0:
        log("[gofile-keepalive] FATAL: zero successes, exiting with code 1")
        sys.exit(1)


global_batch_start = 0.0

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
