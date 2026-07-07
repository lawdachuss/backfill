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

    const gofileUrl = `https://gofile.io/d/${code}`;
    const resp = await fetch(gofileUrl, {
      headers: {
        'User-Agent':
          'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36',
      },
    });

    return new Response('ok', {
      status: resp.ok ? 200 : resp.status,
    });
  },
};
