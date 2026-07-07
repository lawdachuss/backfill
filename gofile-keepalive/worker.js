const API_BASE = 'https://api.gofile.io';
const CONFIG_URL = 'https://gofile.io/dist/js/config.js';
const UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36';

async function fetchWt() {
  const resp = await fetch(CONFIG_URL, { headers: { 'User-Agent': UA } });
  const text = await resp.text();
  const m = text.match(/wt\s*=\s*['"]([a-zA-Z0-9]+)['"]/);
  return m ? m[1] : '4fd6sg89d7s6';
}

async function createGuestToken() {
  const resp = await fetch(`${API_BASE}/accounts`, {
    method: 'POST',
    headers: { 'User-Agent': UA, 'Accept': 'application/json' },
  });
  const body = await resp.json();
  if (body.status === 'ok' && body.data?.token) return body.data.token;
  return null;
}

export default {
  async fetch(request) {
    const url = new URL(request.url);
    const code = url.searchParams.get('code');
    if (!code) return new Response('missing code param', { status: 400 });

    const wt = await fetchWt();

    // Attempt 1: API with X-Website-Token only (works for public files)
    let apiOk = false;
    const apiResp = await fetch(
      `${API_BASE}/contents/${code}?cache=true`,
      {
        headers: {
          'X-Website-Token': wt,
          'User-Agent': UA,
          'Accept': 'application/json, text/plain, */*',
          'Origin': 'https://gofile.io',
          'Referer': `https://gofile.io/d/${code}`,
        },
      },
    );
    const body = await apiResp.json();
    if (body.status === 'ok') {
      apiOk = true;
    }

    // Attempt 2: if API failed, visit the HTML page as fallback
    if (!apiOk) {
      const pageUrl = `https://gofile.io/d/${code}`;
      await fetch(pageUrl, {
        headers: {
          'User-Agent': UA,
          'Accept': 'text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8',
          'Accept-Language': 'en-US,en;q=0.5',
          'Origin': 'https://gofile.io',
          'Referer': 'https://gofile.io/',
          'DNT': '1',
          'Upgrade-Insecure-Requests': '1',
          'Sec-Fetch-Dest': 'document',
          'Sec-Fetch-Mode': 'navigate',
          'Sec-Fetch-Site': 'same-origin',
        },
      });
    }

    return new Response(apiOk ? 'ok' : 'ok:web', { status: 200 });
  },
};
