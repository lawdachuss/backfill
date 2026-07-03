# Test tunnel accessibility from your location
# Usage: .\scripts\test-tunnel-access.ps1 <tunnel-url>

param(
    [Parameter(Mandatory=$false)]
    [string]$TunnelUrl
)

$ErrorActionPreference = "Continue"

Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Cyan
Write-Host "║           🔍 TUNNEL ACCESSIBILITY TEST                     ║" -ForegroundColor Cyan
Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Cyan
Write-Host ""

# Load environment variables
if (Test-Path ".env") {
    Get-Content ".env" | ForEach-Object {
        if ($_ -match '^\s*([^#][^=]+)=(.*)$') {
            $name = $matches[1].Trim()
            $value = $matches[2].Trim()
            [Environment]::SetEnvironmentVariable($name, $value, "Process")
        }
    }
}

# If no URL provided, fetch from Supabase
if (-not $TunnelUrl) {
    Write-Host "📡 No URL provided, fetching from Supabase..." -ForegroundColor Yellow
    
    $SUPABASE_URL = $env:SUPABASE_URL
    $SUPABASE_API_KEY = $env:SUPABASE_API_KEY
    
    if ($SUPABASE_URL -and $SUPABASE_API_KEY) {
        try {
            $response = Invoke-RestMethod -Uri "$SUPABASE_URL/rest/v1/tunnel_sessions?select=*&order=started_at.desc&limit=1" `
                -Method Get `
                -Headers @{
                    "apikey" = $SUPABASE_API_KEY
                    "Content-Type" = "application/json"
                } -ErrorAction Stop
            
            if ($response -and $response.Count -gt 0) {
                $TunnelUrl = $response[0].url
                Write-Host "✅ Found: $TunnelUrl" -ForegroundColor Green
                Write-Host ""
            }
        } catch {
            Write-Host "❌ Failed to fetch from Supabase" -ForegroundColor Red
        }
    }
    
    if (-not $TunnelUrl) {
        Write-Host "❌ No tunnel URL available" -ForegroundColor Red
        Write-Host ""
        Write-Host "Usage: .\scripts\test-tunnel-access.ps1 <tunnel-url>" -ForegroundColor Yellow
        exit 1
    }
}

Write-Host "Testing: $TunnelUrl" -ForegroundColor Cyan
Write-Host ""

# Test 1: DNS Resolution
Write-Host "[1/6] DNS Resolution..." -ForegroundColor Yellow
try {
    $hostname = ([System.Uri]$TunnelUrl).Host
    $dnsResult = [System.Net.Dns]::GetHostAddresses($hostname)
    Write-Host "   ✅ Resolves to: $($dnsResult[0].IPAddressToString)" -ForegroundColor Green
    $dnsWorking = $true
} catch {
    Write-Host "   ❌ DNS resolution failed: $_" -ForegroundColor Red
    Write-Host "   This means the tunnel URL is invalid or DNS hasn't propagated to your region yet" -ForegroundColor Yellow
    $dnsWorking = $false
}

# Test 2: Ping
Write-Host "[2/6] Ping Test..." -ForegroundColor Yellow
if ($dnsWorking) {
    try {
        $pingResult = Test-Connection -ComputerName $hostname -Count 2 -ErrorAction Stop
        $avgTime = ($pingResult | Measure-Object -Property ResponseTime -Average).Average
        Write-Host "   ✅ Ping successful (avg: $([math]::Round($avgTime, 2))ms)" -ForegroundColor Green
    } catch {
        Write-Host "   ⚠️  Ping failed (this is OK - network may block ICMP)" -ForegroundColor Yellow
    }
} else {
    Write-Host "   ⏭️  Skipped (DNS failed)" -ForegroundColor Gray
}

# Test 3: TCP Connection
Write-Host "[3/6] TCP Connection (port 443)..." -ForegroundColor Yellow
if ($dnsWorking) {
    try {
        $tcpClient = New-Object System.Net.Sockets.TcpClient
        $tcpClient.Connect($hostname, 443)
        $tcpClient.Close()
        Write-Host "   ✅ TCP connection successful" -ForegroundColor Green
        $tcpWorking = $true
    } catch {
        Write-Host "   ❌ TCP connection failed: $_" -ForegroundColor Red
        Write-Host "   The tunnel may be down or blocked by your firewall/network" -ForegroundColor Yellow
        $tcpWorking = $false
    }
} else {
    Write-Host "   ⏭️  Skipped (DNS failed)" -ForegroundColor Gray
    $tcpWorking = $false
}

# Test 4: HTTPS Request
Write-Host "[4/6] HTTPS Request..." -ForegroundColor Yellow
if ($tcpWorking) {
    try {
        $response = Invoke-WebRequest -Uri $TunnelUrl -Method Get -TimeoutSec 15 -UseBasicParsing -ErrorAction Stop
        Write-Host "   ✅ HTTP Status: $($response.StatusCode)" -ForegroundColor Green
        Write-Host "   ✅ Content Length: $($response.Content.Length) bytes" -ForegroundColor Green
        $httpWorking = $true
    } catch {
        $statusCode = $_.Exception.Response.StatusCode.value__
        Write-Host "   ❌ HTTPS request failed" -ForegroundColor Red
        if ($statusCode) {
            Write-Host "   HTTP Status Code: $statusCode" -ForegroundColor Yellow
            
            if ($statusCode -eq 502 -or $statusCode -eq 503 -or $statusCode -eq 504) {
                Write-Host "   ⚠️  Gateway error - the backend app may not be running" -ForegroundColor Yellow
            } elseif ($statusCode -eq 403) {
                Write-Host "   ⚠️  Forbidden - the tunnel endpoint may be blocking the request" -ForegroundColor Yellow
            } elseif ($statusCode -eq 521) {
                Write-Host "   ⚠️  Web server is down - the backend app crashed" -ForegroundColor Yellow
            }
        } else {
            Write-Host "   Error: $_" -ForegroundColor Yellow
        }
        $httpWorking = $false
    }
} else {
    Write-Host "   ⏭️  Skipped (TCP failed)" -ForegroundColor Gray
    $httpWorking = $false
}

# Test 5: Check if it's GoOnDVR
Write-Host "[5/6] Verify GoOnDVR App..." -ForegroundColor Yellow
if ($httpWorking) {
    try {
        $response = Invoke-WebRequest -Uri $TunnelUrl -Method Get -TimeoutSec 10 -UseBasicParsing -ErrorAction Stop
        $content = $response.Content
        
        if ($content -match "goondvr|GoOnDVR|GOONDVR") {
            Write-Host "   ✅ GoOnDVR application detected" -ForegroundColor Green
        } else {
            Write-Host "   ⚠️  Response doesn't look like GoOnDVR" -ForegroundColor Yellow
            Write-Host "   First 200 chars: $($content.Substring(0, [Math]::Min(200, $content.Length)))" -ForegroundColor Gray
        }
    } catch {
        Write-Host "   ❌ Cannot verify app" -ForegroundColor Red
    }
} else {
    Write-Host "   ⏭️  Skipped (HTTP failed)" -ForegroundColor Gray
}

# Test 6: API Endpoint
Write-Host "[6/6] Test API Endpoint..." -ForegroundColor Yellow
if ($httpWorking) {
    try {
        $apiUrl = "$TunnelUrl/api/stats"
        $response = Invoke-RestMethod -Uri $apiUrl -Method Get -TimeoutSec 10 -ErrorAction Stop
        Write-Host "   ✅ API is responding" -ForegroundColor Green
        Write-Host "   Uptime: $($response.uptime_seconds) seconds" -ForegroundColor Gray
        Write-Host "   Recording Count: $($response.recording_count)" -ForegroundColor Gray
        Write-Host "   Disk Usage: $([math]::Round($response.disk_percent, 2))%" -ForegroundColor Gray
    } catch {
        Write-Host "   ❌ API endpoint not responding" -ForegroundColor Red
    }
} else {
    Write-Host "   ⏭️  Skipped (HTTP failed)" -ForegroundColor Gray
}

Write-Host ""
Write-Host "════════════════════════════════════════════════════════════" -ForegroundColor Cyan
Write-Host "DIAGNOSIS COMPLETE" -ForegroundColor Cyan
Write-Host "════════════════════════════════════════════════════════════" -ForegroundColor Cyan
Write-Host ""

# Summary and recommendations
if ($httpWorking) {
    Write-Host "✅ TUNNEL IS ACCESSIBLE FROM YOUR LOCATION" -ForegroundColor Green
    Write-Host ""
    Write-Host "You can access your UI at:" -ForegroundColor White
    Write-Host "  $TunnelUrl" -ForegroundColor Cyan
    Write-Host ""
    Write-Host "Opening in browser..." -ForegroundColor Yellow
    Start-Process $TunnelUrl
} elseif ($dnsWorking -and $tcpWorking) {
    Write-Host "⚠️  TUNNEL EXISTS BUT HTTP REQUESTS FAIL" -ForegroundColor Yellow
    Write-Host ""
    Write-Host "Possible causes:" -ForegroundColor White
    Write-Host "  • Backend app is not running or crashed" -ForegroundColor Gray
    Write-Host "  • The tunnel endpoint is blocking requests" -ForegroundColor Gray
    Write-Host "  • SSL/TLS handshake issues" -ForegroundColor Gray
    Write-Host ""
    Write-Host "Try:" -ForegroundColor White
    Write-Host "  1. Wait 2-3 minutes and test again" -ForegroundColor Gray
    Write-Host "  2. Check GitHub Actions logs for backend errors" -ForegroundColor Gray
    Write-Host "  3. Try accessing from a different network/device" -ForegroundColor Gray
} elseif ($dnsWorking) {
    Write-Host "⚠️  DNS WORKS BUT CANNOT CONNECT" -ForegroundColor Yellow
    Write-Host ""
    Write-Host "Possible causes:" -ForegroundColor White
    Write-Host "  • Your firewall is blocking the connection" -ForegroundColor Gray
    Write-Host "  • Your ISP is blocking tunnel domains" -ForegroundColor Gray
    Write-Host "  • The tunnel process died" -ForegroundColor Gray
    Write-Host ""
    Write-Host "Try:" -ForegroundColor White
    Write-Host "  1. Disable firewall temporarily and test" -ForegroundColor Gray
    Write-Host "  2. Try from a different network (mobile hotspot)" -ForegroundColor Gray
    Write-Host "  3. Use a VPN" -ForegroundColor Gray
} else {
    Write-Host "❌ DNS RESOLUTION FAILED" -ForegroundColor Red
    Write-Host ""
    Write-Host "Possible causes:" -ForegroundColor White
    Write-Host "  • DNS hasn't propagated to your region yet (wait 5-10 minutes)" -ForegroundColor Gray
    Write-Host "  • The tunnel URL is invalid or expired" -ForegroundColor Gray
    Write-Host "  • Your DNS server has issues" -ForegroundColor Gray
    Write-Host ""
    Write-Host "Try:" -ForegroundColor White
    Write-Host "  1. Wait 5-10 minutes and test again" -ForegroundColor Gray
    Write-Host "  2. Use Google DNS (8.8.8.8) or public DNS (1.1.1.1)" -ForegroundColor Gray
    Write-Host "  3. Flush DNS cache: ipconfig /flushdns" -ForegroundColor Gray
    Write-Host "  4. Get a fresh tunnel URL from GitHub Actions" -ForegroundColor Gray
}

Write-Host ""
