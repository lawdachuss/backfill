const CONFIG_URL = 'https://gofile.io/dist/js/config.js';
const API_BASE = 'https://api.gofile.io';
const UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36';

function extractWt(text) {
  const m = text.match(/wt\s*[=:]\s*['"]([a-zA-Z0-9]+)['"]/);
  return m ? m[1] : null;
}

export default {
  async fetch(request) {
    const url = new URL(request.url);
    const code = url.searchParams.get('code');
    if (!code) {
      return new Response(JSON.stringify({ error: 'missing code param' }), {
        status: 400,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // 1) fetch wt token from config
    let wt;
    try {
      const configResp = await fetch(CONFIG_URL, { headers: { 'User-Agent': UA } });
      const text = await configResp.text();
      wt = extractWt(text);
      if (!wt) wt = '4fd6sg89d7s6';
    } catch {
      wt = '4fd6sg89d7s6';
    }

    // 2) call contents API — this registers activity / resets the timer
    const apiUrl = `${API_BASE}/contents/${code}?wt=${wt}&cache=true`;
    const apiResp = await fetch(apiUrl, {
      headers: {
        'User-Agent': UA,
        'Accept': 'application/json, text/plain, */*',
        'Origin': 'https://gofile.io',
        'Referer': `https://gofile.io/d/${code}`,
      },
    });

    if (!apiResp.ok) {
      return new Response(`api error: ${apiResp.status}`, { status: apiResp.status });
    }

    const body = await apiResp.json();
    if (body.status !== 'ok') {
      return new Response(JSON.stringify(body), { status: 502 });
    }

    return new Response('ok', { status: 200 });
  },
};
