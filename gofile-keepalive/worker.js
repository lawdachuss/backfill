const API_BASE = 'https://api.gofile.io';
const UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36';
const LANG = 'en-US';
const SALT = '9844d94d963d30';
const WT_WINDOW = 14400;

function sha256(msg) {
  const data = new TextEncoder().encode(msg);
  return crypto.subtle.digest('SHA-256', data).then(h =>
    Array.from(new Uint8Array(h)).map(b => b.toString(16).padStart(2, '0')).join('')
  );
}

function websiteToken(accountToken, windowOffset = 0) {
  const window = Math.floor(Date.now() / 1000 / WT_WINDOW) + windowOffset;
  return sha256(`${UA}::${LANG}::${accountToken}::${window}::${SALT}`);
}

async function fetchContents(code, token, wt) {
  const params = new URLSearchParams({
    contentFilter: '', page: '1', pageSize: '1000',
    sortField: 'createTime', sortDirection: '-1',
  });
  const resp = await fetch(`${API_BASE}/contents/${code}?${params}`, {
    headers: {
      'Authorization': `Bearer ${token}`,
      'X-Website-Token': wt,
      'X-BL': LANG,
      'User-Agent': UA,
      'Accept': '*/*',
      'Origin': 'https://gofile.io',
      'Referer': `https://gofile.io/d/${code}`,
    },
  });
  return resp.json();
}

async function getGuestToken() {
  const resp = await fetch(`${API_BASE}/accounts`, {
    method: 'POST',
    headers: { 'User-Agent': UA, 'Origin': 'https://gofile.io', 'Accept': 'application/json' },
  });
  const body = await resp.json();
  if (body.status === 'ok' && body.data?.token) return body.data.token;
  return null;
}

async function getFileData(code, token) {
  for (const offset of [0, -1]) {
    const wt = await websiteToken(token, offset);
    const body = await fetchContents(code, token, wt);
    if (body.status === 'ok') return body;
    if (body.status !== 'error-notPremium') return body;
  }
  return { status: 'error-notPremium' };
}

// Resolve the best direct download link for a file via Cloudflare egress.
async function getDirectLink(code, token) {
  const body = await getFileData(code, token);
  if (body.status !== 'ok') return { status: body.status };
  const children = body.data?.children;
  if (!children || typeof children !== 'object') return { status: 'nochildren' };
  let best = null;
  for (const child of Object.values(children)) {
    if (child.type === 'file' && child.link && (!best || (child.size || 0) > (best.size || 0))) {
      best = child;
    }
  }
  if (!best) return { status: 'nofile' };
  return { status: 'ok', link: best.link, size: best.size || 0 };
}

async function downloadViaCdn(children, code) {
  let fileCount = 0, downloadCount = 0;
  for (const [id, child] of Object.entries(children)) {
    if (child.type !== 'file') continue;
    fileCount++;
    if (!child.link) continue;
    try {
      const resp = await fetch(child.link, {
        headers: { 'User-Agent': UA, 'Range': 'bytes=0-262143', 'Referer': `https://gofile.io/d/${code}` },
        signal: AbortSignal.timeout(20000),
      });
      if (resp.ok || resp.status === 206) downloadCount++;
    } catch {}
  }
  return { fileCount, downloadCount };
}

export default {
  async fetch(request) {
    const url = new URL(request.url);
    const code = url.searchParams.get('code');
    const action = url.searchParams.get('action');

    // Token endpoint: return a fresh guest token
    if (action === 'token') {
      const token = await getGuestToken();
      if (token) return new Response(token, { status: 200 });
      return new Response('no-token', { status: 502 });
    }

    // Link endpoint: resolve the direct download URL via Cloudflare egress and
    // return it as JSON { status, link, size }. Used by the backfill worker so
    // API calls are routed through this proxy (avoids IP rate-limits / Cloudflare
    // blocks on the caller's egress).
    if (action === 'link') {
      if (!code) return new Response('missing code param', { status: 400 });
      const provided = url.searchParams.get('token');
      const token = provided || (await getGuestToken());
      if (!token) return new Response(JSON.stringify({ status: 'no-token' }), {
        status: 502, headers: { 'Content-Type': 'application/json' },
      });
      const result = await getDirectLink(code, token);
      return new Response(JSON.stringify(result), {
        status: 200, headers: { 'Content-Type': 'application/json' },
      });
    }

    // Stream endpoint: proxy an arbitrary media URL through Cloudflare egress
    // with full Range support. The backfill worker uses this to fetch bytes
    // from CDNs (GoFile's tapecontent.net, Streamtape, …) that block the
    // caller's direct (datacenter) egress IP. Cloudflare's egress IPs are
    // accepted by these hosts (see downloadViaCdn), so this sidesteps the
    // -138 "Connection failed" errors ffmpeg hits when run from CI runners.
    //   ?action=stream&u=<url-encoded target media URL>[&ref=<referer>]
    if (action === 'stream') {
      const target = url.searchParams.get('u');
      if (!target) return new Response('missing u param', { status: 400 });
      let targetURL;
      try {
        targetURL = new URL(target);
      } catch {
        return new Response('invalid u param', { status: 400 });
      }
      if (targetURL.protocol !== 'http:' && targetURL.protocol !== 'https:') {
        return new Response('unsupported protocol', { status: 400 });
      }
      // SSRF guard: only proxy known media CDN hosts. Without this the worker
      // would be an open proxy usable to reach internal/cloud metadata endpoints
      // from Cloudflare's egress. Extend the list when new CDNs are added.
      const host = targetURL.hostname.toLowerCase();
      const ALLOWED_HOSTS = [
        'tapecontent.net', 'gofile.io',
        'streamtape.com', 'streamtape.net',
      ];
      const allowed = ALLOWED_HOSTS.some(h => host === h || host.endsWith('.' + h));
      if (!allowed) {
        return new Response('host not allowed: ' + host, { status: 403 });
      }
      // GoFile's CDN (tapecontent.net) requires a gofile.io Referer, just like
      // the resolution path (downloadViaCdn) sets. Default to the target origin
      // for everything else (e.g. Streamtape).
      const ref = url.searchParams.get('ref') ||
        (host.includes('gofile') || host.includes('tapecontent')
          ? 'https://gofile.io/'
          : targetURL.origin);
      const reqHeaders = {
        'User-Agent': UA,
        'Referer': ref,
        'Origin': targetURL.origin,
        'Accept': '*/*',
      };
      const range = request.headers.get('Range');
      if (range) reqHeaders['Range'] = range;
      try {
        const upstream = await fetch(targetURL.toString(), {
          headers: reqHeaders,
          signal: AbortSignal.timeout(300000),
        });
        // Forward the upstream response verbatim (status + headers + body).
        // This preserves 206/Content-Range so ffmpeg's HTTP range seeks work.
        // Strip hop-by-hop headers that must not be re-sent to the client.
        // (content-encoding / transfer-encoding are intentionally kept so the
        // client can still decode the streamed body correctly.)
        const respHeaders = new Headers(upstream.headers);
        for (const hop of ['connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization']) {
          respHeaders.delete(hop);
        }
        return new Response(upstream.body, {
          status: upstream.status,
          headers: respHeaders,
        });
      } catch (e) {
        return new Response('stream upstream error: ' + (e && e.message ? e.message : e), { status: 502 });
      }
    }

    if (!code) return new Response('missing code param', { status: 400 });

    // Use provided token, or create one
    let token = url.searchParams.get('token');
    if (!token) {
      token = await getGuestToken();
      if (!token) return new Response('no-token', { status: 502 });
    }

    const body = await getFileData(code, token);
    if (body.status !== 'ok') return new Response(`api:${body.status}`, { status: 200 });

    const children = body.data?.children;
    if (!children || typeof children !== 'object') return new Response('ok:nochildren', { status: 200 });

    const { fileCount, downloadCount } = await downloadViaCdn(children, code);
    return new Response(`ok:${downloadCount}/${fileCount}`, { status: 200 });
  },
};
