const API_BASE = 'https://api.gofile.io';
const UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36';
const LANG = 'en-US';
const SALT = '9844d94d963d30';
const WT_WINDOW = 14400; // 4 hours in seconds

function sha256(msg) {
  const data = new TextEncoder().encode(msg);
  return crypto.subtle.digest('SHA-256', data).then(h => {
    return Array.from(new Uint8Array(h)).map(b => b.toString(16).padStart(2, '0')).join('');
  });
}

function websiteToken(accountToken, windowOffset = 0) {
  const window = Math.floor(Date.now() / 1000 / WT_WINDOW) + windowOffset;
  const raw = `${UA}::${LANG}::${accountToken}::${window}::${SALT}`;
  return sha256(raw);
}

async function createGuestToken() {
  const resp = await fetch(`${API_BASE}/accounts`, {
    method: 'POST',
    headers: { 'User-Agent': UA, 'Origin': 'https://gofile.io', 'Accept': 'application/json' },
  });
  const body = await resp.json();
  if (body.status === 'ok' && body.data?.token) return body.data.token;
  return null;
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

async function getFileData(code, token) {
  // Try current 4hr window, then previous window
  for (const offset of [0, -1]) {
    const wt = await websiteToken(token, offset);
    const body = await fetchContents(code, token, wt);
    if (body.status === 'ok') return body;
    if (body.status !== 'error-notPremium') return body;
  }
  return { status: 'error-notPremium' };
}

async function downloadViaCdn(children, code) {
  let fileCount = 0, downloadCount = 0;
  for (const [id, child] of Object.entries(children)) {
    if (child.type !== 'file') continue;
    fileCount++;
    if (!child.link) continue;
    try {
      const resp = await fetch(child.link, {
        headers: { 'User-Agent': UA, 'Range': 'bytes=0-524287', 'Referer': `https://gofile.io/d/${code}` },
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
    if (!code) return new Response('missing code param', { status: 400 });

    const token = await createGuestToken();
    if (!token) return new Response('no-token', { status: 502 });

    const body = await getFileData(code, token);
    if (body.status !== 'ok') return new Response(`api:${body.status}`, { status: 200 });

    const children = body.data?.children;
    if (!children || typeof children !== 'object') return new Response('ok:nochildren', { status: 200 });

    const { fileCount, downloadCount } = await downloadViaCdn(children, code);
    return new Response(`ok:${downloadCount}/${fileCount}`, { status: 200 });
  },
};
