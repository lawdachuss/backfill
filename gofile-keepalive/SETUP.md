# GoFile Keep-Alive Setup

## How it works

1. **Cloudflare Worker** sits between GitHub Actions and GoFile
2. Worker runs on Cloudflare CDN IPs (not datacenter IPs) — GoFile doesn't block them
3. GitHub Actions sends each GoFile code to the Worker
4. Worker visits `https://gofile.io/d/{code}` from Cloudflare IP
5. This counts as activity → GoFile keeps the file alive

## Prerequisites

- A Cloudflare account (free, sign up at https://dash.cloudflare.com)
- The `SUPABASE_URL` and `SUPABASE_API_KEY` secrets already in your GitHub repo

## Step 1: Deploy the Cloudflare Worker

1. Go to https://dash.cloudflare.com → **Workers & Pages** → **Create application**
2. Choose **"Create Worker"** → give it a name like `gofile-keepalive`
3. Replace the default code with the content of `gofile-keepalive/worker.js`
4. Click **"Save and Deploy"**
5. Copy your Worker URL (e.g., `https://gofile-keepalive.your-name.workers.dev`)

## Step 2: Add the Worker URL as a GitHub secret

1. Go to your GitHub repo → **Settings** → **Secrets and variables** → **Actions**
2. Click **"New repository secret"**
3. Name: `GOFILE_PROXY_URL`
4. Value: `https://gofile-keepalive.your-name.workers.dev`
5. Click **"Add secret"**

## Step 3: Run the workflow

- Go to **Actions** → **GoFile Keep Alive** → **Run workflow**
- Or wait for the scheduled run (every 6 days)

## Troubleshooting

If you see errors, check the Worker logs:
- Go to Cloudflare Dashboard → **Workers & Pages** → your worker → **Logs**
- Test the Worker directly: `curl https://gofile-keepalive.your-name.workers.dev/?code=YOUR_CODE`

If GoFile starts blocking Cloudflare IPs too, you can try:
- The `--no-proxies` flag to fall back to direct (will likely get 429)
- Or upgrade to GoFile Premium ($7.50/month) where files never expire
