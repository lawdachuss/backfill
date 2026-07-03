#!/bin/bash
# Get the current tunnel URL from Supabase
# Usage: ./scripts/get-tunnel-url.sh

set -e

# Load environment variables from .env if it exists
if [ -f .env ]; then
    export $(grep -v '^#' .env | xargs)
fi

if [ -z "$SUPABASE_URL" ] || [ -z "$SUPABASE_API_KEY" ]; then
    echo "❌ Error: SUPABASE_URL and SUPABASE_API_KEY must be set"
    echo "Set them in .env file or as environment variables"
    exit 1
fi

echo "🔍 Fetching latest tunnel URL from Supabase..."

RESPONSE=$(curl -s -X GET "$SUPABASE_URL/rest/v1/tunnel_sessions?select=*&order=started_at.desc&limit=1" \
    -H "apikey: $SUPABASE_API_KEY" \
    -H "Content-Type: application/json")

TUNNEL_URL=$(echo "$RESPONSE" | grep -oP '"url":"https://[^"]+' | head -1 | cut -d'"' -f4)
RUN_ID=$(echo "$RESPONSE" | grep -oP '"run_id":\d+' | head -1 | cut -d':' -f2)
STARTED_AT=$(echo "$RESPONSE" | grep -oP '"started_at":"[^"]+' | head -1 | cut -d'"' -f4)

if [ -n "$TUNNEL_URL" ]; then
    echo ""
    echo "╔════════════════════════════════════════════════════════════╗"
    echo "║                    🌐 WEB UI ACCESS                        ║"
    echo "╚════════════════════════════════════════════════════════════╝"
    echo ""
    echo "   $TUNNEL_URL"
    echo ""
    echo "   Run ID: $RUN_ID"
    echo "   Started: $STARTED_AT"
    echo ""
else
    echo "❌ No tunnel sessions found in Supabase"
    echo "Make sure the GitHub Actions workflow has run at least once"
    exit 1
fi
