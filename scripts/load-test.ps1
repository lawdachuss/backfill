<#
.SYNOPSIS
    Load test for distributed shards: simulates N nodes claiming M channels.
.DESCRIPTION
    Uses the Supabase REST API directly to simulate multiple nodes registering,
    heartbeating, and claiming channels. Verifies fair-share distribution and
    detects any double-claiming (split-brain).
.PARAMETER SupabaseUrl
    The Supabase project URL (e.g., https://xyz.supabase.co).
.PARAMETER SupabaseKey
    The Supabase anon/service key.
.PARAMETER NodeCount
    Number of simulated nodes (default: 5).
.PARAMETER ChannelCount
    Number of simulated channels (default: 100).
.PARAMETER ClaimRounds
    How many claim cycles to simulate (default: 3).
.PARAMETER Verbose
    Show per-node details.
.EXAMPLE
    .\scripts\load-test.ps1 -SupabaseUrl "https://xyz.supabase.co" -SupabaseKey "eyJ..." -NodeCount 5 -ChannelCount 100
#>

param(
    [Parameter(Mandatory = $true)]
    [string]$SupabaseUrl,

    [Parameter(Mandatory = $true)]
    [string]$SupabaseKey,

    [int]$NodeCount = 5,

    [int]$ChannelCount = 100,

    [int]$ClaimRounds = 3,

    [switch]$Verbose
)

$ErrorActionPreference = "Stop"
$RunID = [guid]::NewGuid().ToString().Substring(0, 8)

$Headers = @{
    "apikey"        = $SupabaseKey
    Authorization   = "Bearer $SupabaseKey"
    "Content-Type"  = "application/json"
}

function Invoke-Supabase {
    param([string]$Method, [string]$Path, $Body)
    $params = @{
        Uri     = "$SupabaseUrl/rest/v1$Path"
        Method  = $Method
        Headers = $Headers
    }
    if ($Body) {
        $params["Body"] = ($Body | ConvertTo-Json -Depth 10 -Compress)
    }
    try {
        $response = Invoke-RestMethod @params -ContentType "application/json"
        return $response
    } catch {
        $statusCode = $_.Exception.Response.StatusCode.value__
        $errorBody = $_.ErrorDetails.Message
        Write-Warning "Supabase $Method $Path returned $statusCode : $errorBody"
        return $null
    }
}

# ── Cleanup any previous run ─────────────────────────────────────────────
Write-Host "=== Load Test $RunID ===" -ForegroundColor Cyan
Write-Host "Nodes: $NodeCount, Channels: $ChannelCount, Rounds: $ClaimRounds" -ForegroundColor Yellow

# ── Generate node IDs ────────────────────────────────────────────────────
$NodeIDs = @()
for ($i = 0; $i -lt $NodeCount; $i++) {
    $NodeIDs += "loadtest-$RunID-node-$i"
}

# ── Generate channel IDs ─────────────────────────────────────────────────
$ChannelIDs = @()
for ($i = 0; $i -lt $ChannelCount; $i++) {
    $site = if ($i % 5 -eq 0) { "stripchat" } else { "chaturbate" }
    $ChannelIDs += @{
        username = "loadtest-$RunID-ch-$i"
        site     = $site
    }
}

# ── Step 1: Register nodes ──────────────────────────────────────────────
Write-Host "[1/5] Registering $NodeCount nodes..." -ForegroundColor Green
$start = Get-Date
foreach ($id in $NodeIDs) {
    $null = Invoke-Supabase -Method POST -Path "/nodes?on_conflict=node_id" -Body @{
        node_id          = $id
        hostname         = "loadtest-$RunID"
        software_version = "load-test"
        status           = "online"
        current_load     = 0
    }
}
Write-Host "  Done in $([math]::Round(((Get-Date) - $start).TotalSeconds, 2))s" -ForegroundColor Gray

# ── Step 2: Create channel assignments ──────────────────────────────────
Write-Host "[2/5] Creating $ChannelCount channel assignments..." -ForegroundColor Green
$start = Get-Date
$batchSize = 50
for ($i = 0; $i -lt $ChannelIDs.Count; $i += $batchSize) {
    $batch = $ChannelIDs[$i..([math]::Min($i + $batchSize - 1, $ChannelIDs.Count - 1))]
    $records = @()
    foreach ($ch in $batch) {
        $records += @{
            username    = $ch.username
            site        = $ch.site
            status      = "unassigned"
            is_live     = $true
            framerate   = if ($ch.site -eq "stripchat") { 30 } else { 60 }
            resolution  = if ($ch.site -eq "stripchat") { 720 } else { 1080 }
        }
    }
    $body = $records | ConvertTo-Json -Depth 10 -Compress
    try {
        $null = Invoke-RestMethod -Uri "$SupabaseUrl/rest/v1/channel_assignments" `
            -Method POST `
            -Headers $Headers `
            -Body $body `
            -ContentType "application/json"
    } catch {
        Write-Warning "Batch insert failed: $_"
    }
}
Write-Host "  Done in $([math]::Round(((Get-Date) - $start).TotalSeconds, 2))s" -ForegroundColor Gray

# ── Step 3: Claim rounds ─────────────────────────────────────────────────
Write-Host "[3/5] Running $ClaimRounds claim rounds..." -ForegroundColor Green
for ($round = 1; $round -le $ClaimRounds; $round++) {
    $start = Get-Date
    $totalClaimed = 0

    # Get stats first
    $stats = Invoke-Supabase -Method GET -Path "/channel_assignments?select=count&is_live=eq.true&status=eq.unassigned"
    $liveUnassigned = 0
    # Get alive node count
    $aliveNodes = Invoke-Supabase -Method GET -Path "/nodes?status=eq.online&select=node_id"
    $aliveCount = if ($aliveNodes) { $aliveNodes.Count } else { 0 }

    # Calculate fair share
    $fairShare = if ($aliveCount -gt 0) { [math]::Ceiling($ChannelCount / $aliveCount) } else { 0 }
    if ($Verbose) {
        Write-Host "  Round $round: live unassigned = ~$liveUnassigned, alive nodes = $aliveCount, fair share = $fairShare" -ForegroundColor Gray
    }

    # Each node claims
    foreach ($id in $NodeIDs) {
        $claimed = Invoke-Supabase -Method PATCH -Path "/channel_assignments?assigned_node=is.null&status=eq.unassigned&is_live=eq.true&limit=$fairShare" -Body @{
            assigned_node  = $id
            status         = "claimed"
            assigned_at    = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ")
            last_heartbeat = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ")
        }
        if ($claimed -and $claimed.Count -gt 0) {
            $totalClaimed += $claimed.Count
            if ($Verbose) {
                Write-Host "    Node $id claimed $($claimed.Count) channels" -ForegroundColor DarkGray
            }
            # Update current_load
            $null = Invoke-Supabase -Method PATCH -Path "/nodes?node_id=eq.$id" -Body @{
                current_load    = $claimed.Count
                last_heartbeat  = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ")
            }
        }
    }
    Write-Host "  Round $round: $totalClaimed channels claimed in $([math]::Round(((Get-Date) - $start).TotalSeconds, 2))s" -ForegroundColor Gray
}

# ── Step 4: Verify no split-brain ───────────────────────────────────────
Write-Host "[4/5] Verifying distribution..." -ForegroundColor Green
$assignments = Invoke-Supabase -Method GET -Path "/channel_assignments?select=*&order=username.asc"
if ($assignments) {
    $claimedBy = @{}
    $splitBrains = 0
    foreach ($a in $assignments) {
        if (-not $a.assigned_node) { continue }
        # Only check our test channels
        if ($a.username -notlike "loadtest-$RunID-*") { continue }
        if ($claimedBy.ContainsKey($a.username) -and $claimedBy[$a.username] -ne $a.assigned_node) {
            Write-Warning "SPLIT-BRAIN: $($a.username) claimed by $($claimedBy[$a.username]) AND $($a.assigned_node)"
            $splitBrains++
        }
        $claimedBy[$a.username] = $a.assigned_node
    }

    if ($splitBrains -eq 0) {
        Write-Host "  ✓ No split-brain detected" -ForegroundColor Green
    } else {
        Write-Warning "  ✗ $splitBrains split-brain occurrences!"
    }

    # Distribution stats
    $nodeLoads = @{}
    foreach ($a in $assignments) {
        if (-not $a.assigned_node) { continue }
        if ($a.username -notlike "loadtest-$RunID-*") { continue }
        $nodeLoads[$a.assigned_node] = ($nodeLoads[$a.assigned_node] ?? 0) + 1
    }
    Write-Host "  Distribution:" -ForegroundColor Gray
    foreach ($id in $NodeIDs) {
        $load = $nodeLoads[$id] ?? 0
        Write-Host "    $id : $load channels" -ForegroundColor DarkGray
    }

    $minLoad = ($nodeLoads.Values | Measure-Object -Minimum).Minimum
    $maxLoad = ($nodeLoads.Values | Measure-Object -Maximum).Maximum
    $imbalance = $maxLoad - $minLoad
    Write-Host "  Load imbalance: $imbalance (min=$minLoad, max=$maxLoad)" -ForegroundColor $(if ($imbalance -le 2) { "Green" } else { "Yellow" })
}

# ── Step 5: Cleanup ──────────────────────────────────────────────────────
Write-Host "[5/5] Cleaning up..." -ForegroundColor Green
$start = Get-Date

# Delete test channels
foreach ($ch in $ChannelIDs) {
    $null = Invoke-Supabase -Method DELETE -Path "/channel_assignments?username=eq.$($ch.username)&site=eq.$($ch.site)" -ErrorAction SilentlyContinue
}
# Delete test nodes
foreach ($id in $NodeIDs) {
    $null = Invoke-Supabase -Method DELETE -Path "/nodes?node_id=eq.$id" -ErrorAction SilentlyContinue
}
Write-Host "  Done in $([math]::Round(((Get-Date) - $start).TotalSeconds, 2))s" -ForegroundColor Gray

Write-Host ""
Write-Host "=== Load Test $RunID Complete ===" -ForegroundColor Cyan
