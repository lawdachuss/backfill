import io, json, re, subprocess, html, base64, os, sys

DB      = r"C:\Users\basud\AppData\Local\Temp\opencode\db_filenames.txt"
UPN     = r"C:\Users\basud\AppData\Local\Temp\opencode\upn_full.jsonl"
SEEK    = r"C:\Users\basud\AppData\Local\Temp\opencode\seek_full.jsonl"
OUT     = r"C:\Users\basud\OneDrive\Desktop\backfill-worker\missing_catalog.html"
MIN,MAX = 1200, 5400  # 20min..1h30m
UPN_KEYS = ['a621c7a463d7a25a3e405c99','e803793a0c04bd0e10c50a69','da86ee302a96b7e7039c0cc7','e7adc175af58d975b8c066fa','a8b15af4c751ddfa88e7f5e2']
SEEK_KEY = 'e2fad7ab477cfe9f19be146b'

def read_lines(p):
    # Auto-detect encoding: UTF-16-LE (null bytes between chars) vs UTF-8
    with io.open(p, 'rb') as f:
        header = f.read(128)
    if header[:2] in (b'\xff\xfe', b'\xfe\xff'):
        enc = 'utf-16'
    elif b'\x00' in header[:64]:
        enc = 'utf-16-le'  # no BOM, but every other byte is \x00
    else:
        enc = 'utf-8-sig'
    with io.open(p, 'r', encoding=enc) as f:
        lines = []
        for l in f:
            l = l.strip()
            if l:
                lines.append(l)
        return lines

def is_junk(n):
    return not re.search(r'_\d{4}-\d{2}-\d{2}_\d{2}-\d{2}', n)

def norm(n):
    n = n.strip()
    n = re.sub(r'(\.merged)?(\.mp4)+$', '', n)
    n = re.sub(r'_\d+$', '', n)
    return n

def curl(url, key):
    return subprocess.run(
        ['curl.exe','-s', url, '-H','api-token: '+key, '-H','User-Agent: Mozilla/5.0'],
        capture_output=True, text=True).stdout

def save_host(path, base, keys):
    """
    Fetch all video records from the manage API (with pagination) and save as JSONL.
    Includes the 'id' field from the API (the real video ID for embed URLs).
    """
    seen = set()
    with io.open(path, 'w', encoding='utf-8') as out:
        for key in keys:
            page = 0
            while True:
                url = base + '?perPage=50&page=' + str(page)
                raw = curl(url, key)
                if not raw:
                    break
                try:
                    data = json.loads(raw)
                except:
                    break
                items = data.get('data') or data.get('result') or []
                if not items:
                    break
                for item in items:
                    vid = item.get('id') or ''
                    name = item.get('name') or ''
                    if not vid or not name:
                        continue
                    dedup_key = vid + '|' + name
                    if dedup_key in seen:
                        continue
                    seen.add(dedup_key)
                    rec = {
                        'id': vid,
                        'n': name,
                        'd': item.get('duration') or item.get('d') or 0,
                        'poster': item.get('poster') or '',
                        'preview': item.get('preview') or '',
                        'asset': item.get('assetUrl') or item.get('asset') or '',
                    }
                    out.write(json.dumps(rec, ensure_ascii=False) + '\n')
                page += 1
                if page > 200:
                    break  # safety: prevent infinite pagination loop

def full_url(asset, rel):
    if not rel:
        return ''
    if rel.startswith('http'):
        return rel
    if asset:
        return asset.rstrip('/') + '/' + rel.lstrip('/')
    return rel

def embed_url_for(video_id, asset_url):
    """Construct embed (player) URL using the real video ID from the API."""
    if not video_id:
        return ''
    al = asset_url.lower()
    # SeekStreaming embed (from recorder: chuglii.embedseek.com/#{id})
    if 'seekstreaming' in al or 'seeks.cloud' in al:
        return 'https://chuglii.embedseek.com/#' + video_id
    # UPnShare embed: https://upns.online/#{id} (verified working in browser)
    if 'upns' in al or 'upnshare' in al:
        return 'https://upns.online/#' + video_id
    return ''

def main():
    print('Reading local host data (already fetched)...')
    if not os.path.exists(UPN) or not os.path.exists(SEEK):
        save_host(UPN, 'https://upnshare.com/api/v1/video/manage', UPN_KEYS)
        # SEEK_KEY is a single string; wrap in list so iteration works
        save_host(SEEK, 'https://seekstreaming.com/api/v1/video/manage', [SEEK_KEY])

    db = set(norm(l) for l in read_lines(DB))
    idx = {}
    for path in (UPN, SEEK):
        for l in read_lines(path):
            try: o = json.loads(l)
            except: continue
            raw = o.get('n',''); d = o.get('d',0) or 0
            if not raw or is_junk(raw):
                continue
            if not (MIN <= d <= MAX):
                continue
            nn = norm(raw)
            rec = idx.get(nn)
            has_media = bool(o.get('poster') or o.get('preview'))
            if rec is None or (has_media and not rec['_media']):
                preview_url = full_url(o.get('asset'), o.get('preview'))
                asset_url = o.get('asset') or ''
                video_id = o.get('id') or ''
                video_url = embed_url_for(video_id, asset_url)
                idx[nn] = {'raw': raw, 'd': d,
                           'poster': full_url(asset_url, o.get('poster')),
                           'preview': preview_url,
                           'video': video_url,
                           '_media': has_media}

    missing = [k for k in idx if k not in db]
    missing.sort(key=lambda k: idx[k]['raw'])
    print('Missing (20-90m, non-junk):', len(missing))

    cards = []
    for k in missing:
        r = idx[k]
        poster = r['poster'] or ''
        preview = r['preview'] or ''
        dur = '%dm' % (r['d']//60)
        name = html.escape(r['raw'])
        name_attr = html.escape(r['raw'], quote=True)
        poster_attr = html.escape(poster, quote=True)
        preview_attr = html.escape(preview, quote=True)

        # Build the card's thumbnail (poster image, not video - preview.webp can't play in <video>)
        if poster:
            inner = '<img class="thumb-img" loading="lazy" src="%s" alt="thumb" onerror="this.outerHTML=\'<div class=ph>no poster</div>\'"/>' % poster_attr
        elif preview:
            inner = '<img class="thumb-img" loading="lazy" src="%s" alt="thumb" onerror="this.outerHTML=\'<div class=ph>no preview</div>\'"/>' % preview_attr
        else:
            inner = '<div class="ph">no media</div>'

        # video URL (embed player page for the real recording)
        video_url = r.get('video', '')
        video_attr = html.escape(video_url, quote=True)

        # onclick opens modal with iframe embed player
        onclick = "openModal('%s','%s','%s')" % (video_attr, poster_attr, name_attr)

        badge = '<span class="dur-badge">%s</span>' % dur
        card = (
            '<div class="card" onclick="%s">\n'
            '  <div class="thumb">%s%s</div>\n'
            '  <div class="name" title="%s">%s</div>\n'
            '</div>'
        ) % (onclick, inner, badge, name_attr, name)
        cards.append(card)

    PER_PAGE = 100
    cards_json = json.dumps(cards, ensure_ascii=False)

    # Modal HTML block (static, same for every page)
    modal_html = (
        '<!-- Modal -->\n'
        '<div id="modal-overlay" onclick="closeModal(event)">\n'
        '  <div id="modal-box" onclick="event.stopPropagation()">\n'
        '    <button id="modal-close" onclick="closeModal()">&times;</button>\n'
        '    <div id="modal-spinner"></div>\n    <iframe id="modal-frame" allow="autoplay; fullscreen" onload="onFrameLoaded()"></iframe>\n'
        '    <div id="modal-title"></div>\n'
        '  </div>\n'
        '</div>'
    )

    html_doc = '''<!doctype html>
<html><head><meta charset="utf-8">
<title>Missing Videos Catalog (%d)</title>
<style>
*{box-sizing:border-box;}
body{background:#0e0e10;color:#e6e6e6;font-family:system-ui,Arial;margin:0;padding:16px;}
h1{font-size:18px;}
#bar{position:sticky;top:0;background:#0e0e10;padding:8px 0;z-index:5;}
input{background:#1b1b1f;color:#fff;border:1px solid #333;padding:8px;border-radius:6px;width:320px;font-size:14px;}
input:focus{outline:none;border-color:#6af;}
button{background:#26262c;color:#fff;border:1px solid #333;padding:8px 12px;border-radius:6px;cursor:pointer;font-size:14px;transition:background .15s;}
button:hover{background:#333;}
button:disabled{opacity:.4;cursor:default;}
#pginfo{margin-left:12px;color:#9aa;}
#count{margin-left:12px;color:#9aa;}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));gap:12px;margin-top:12px;}
.card{background:#16161a;border:1px solid #26262c;border-radius:8px;overflow:hidden;cursor:pointer;transition:border-color .15s,transform .1s;}
.card:hover{border-color:#4488ff;transform:translateY(-2px);}
.thumb{position:relative;aspect-ratio:16/9;background:#000;display:block;text-decoration:none;}
.thumb img, .thumb video, .thumb .thumb-img{width:100%%;height:100%%;object-fit:cover;display:block;background:#000;}
.thumb .ph{width:100%%;height:100%%;display:flex;align-items:center;justify-content:center;color:#777;font-size:12px;}
.dur-badge{position:absolute;bottom:6px;right:6px;background:rgba(0,0,0,.75);color:#fff;font-size:11px;font-weight:600;padding:2px 7px;border-radius:4px;line-height:1.4;backdrop-filter:blur(2px);}
.name{padding:4px 8px 8px;font-size:12px;word-break:break-all;color:#ccc;}
#pager{margin-top:14px;display:flex;gap:8px;align-items:center;flex-wrap:wrap;}

/* -- Modal -- */
#modal-overlay{display:none;position:fixed;inset:0;z-index:1000;background:rgba(0,0,0,.85);backdrop-filter:blur(4px);align-items:center;justify-content:center;}
#modal-overlay.active{display:flex;}
#modal-box{position:relative;max-width:95vw;max-height:95vh;background:#000;border-radius:12px;overflow:hidden;box-shadow:0 20px 60px rgba(0,0,0,.8);display:flex;flex-direction:column;}
#modal-close{position:absolute;top:12px;right:12px;z-index:10;background:rgba(0,0,0,.6);color:#fff;border:1px solid rgba(255,255,255,.2);border-radius:50%%;width:40px;height:40px;font-size:22px;cursor:pointer;display:flex;align-items:center;justify-content:center;transition:background .15s;line-height:1;}
#modal-close:hover{background:rgba(255,0,0,.7);}
#modal-frame{border:0;width:95vw;height:85vh;max-width:1200px;max-height:90vh;background:transparent;display:block;position:relative;z-index:2;}
#modal-title{background:rgba(0,0,0,.8);color:#ccc;padding:10px 16px;font-size:13px;word-break:break-all;border-top:1px solid #222;}

/* -- Loading spinner -- */
#modal-spinner{position:absolute;top:50%%;left:50%%;width:60px;height:60px;margin:-30px 0 0 -30px;border:4px solid rgba(255,255,255,.1);border-top-color:#4488ff;border-radius:50%%;animation:spin .8s linear infinite;pointer-events:none;}
@keyframes spin{to{transform:rotate(360deg);}}
</style></head><body>
<h1>Missing Videos Catalog &mdash; duration 20m&ndash;1h30m, not in DB</h1>
<div id="bar">
  <input id="q" placeholder="filter by name..." oninput="onFilter()">
  <span id="count"></span>
  <span id="pginfo"></span>
</div>
<div class="grid" id="g"></div>
<div id="pager">
  <button id="prev" onclick="prev()">&larr; Prev</button>
  <span id="pagetxt"></span>
  <button id="next" onclick="next()">Next &rarr;</button>
  <span>Jump:</span>
  <input id="jump" type="number" min="1" style="width:80px" onchange="jumpTo(this.value)">
  <button onclick="jumpTo(document.getElementById('jump').value)">Go</button>
</div>

%s

<script>
var ALL=%s;
var filtered=ALL.slice();
var page=0, PER=%d;
var mi=document.getElementById('modal-frame');
var mt=document.getElementById('modal-title');
var ms=document.getElementById('modal-spinner');

function openModal(videoUrl, poster, title){
  mt.textContent=title;
  ms.style.display='block';
  mi.removeAttribute('src');
  mi.style.background = poster ? 'url('+poster+') center/contain no-repeat #000' : '#000';
  if(videoUrl){
    mi.setAttribute('src', videoUrl);
  }
  document.getElementById('modal-overlay').classList.add('active');
  document.body.style.overflow='hidden';
}

function onFrameLoaded(){
  ms.style.display='none';
}

function closeModal(e){
  if(e && e.target !== e.currentTarget) return;
  mi.removeAttribute('src');
  ms.style.display='block';
  document.getElementById('modal-overlay').classList.remove('active');
  document.body.style.overflow='';
}

document.addEventListener('keydown', function(e){
  if(e.key==='Escape') closeModal(e);
});

function render(){
  var g=document.getElementById('g');
  var start=page*PER, end=Math.min(start+PER, filtered.length);
  g.innerHTML=filtered.slice(start,end).join('');
  document.getElementById('pagetxt').textContent='Page '+(page+1)+' / '+Math.max(1,Math.ceil(filtered.length/PER));
  document.getElementById('prev').disabled = page<=0;
  document.getElementById('next').disabled = end>=filtered.length;
  document.getElementById('pginfo').textContent='showing '+(filtered.length?start+1:0)+'-'+end+' of '+filtered.length;
}
function onFilter(){
  var v=document.getElementById('q').value.toLowerCase();
  filtered = v ? ALL.filter(function(c){ return c.toLowerCase().indexOf(v)>=0; }) : ALL.slice();
  page=0; render();
  document.getElementById('count').textContent=filtered.length+' matches / '+ALL.length+' total';
}
function prev(){ if(page>0){page--; render();} }
function next(){ if((page+1)*PER < filtered.length){page++; render();} }
function jumpTo(n){ n=parseInt(n,10); if(isNaN(n))return; n=Math.max(1,Math.min(n,Math.ceil(filtered.length/PER))); page=n-1; render(); }
render();
</script></body></html>''' % (len(missing), modal_html, cards_json, PER_PAGE)

    with io.open(OUT, 'w', encoding='utf-8') as f:
        f.write(html_doc)
    print('Wrote', OUT)

if __name__ == '__main__':
    main()
