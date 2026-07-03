# Check GitHub Actions workflow status and get tunnel URL from logs
# Requires: gh CLI (GitHub CLI) installed
# Usage: .\scripts\check-github-actions-status.ps1

$ErrorActionPreference = "Stop"

Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Cyan
Write-Host "║        📊 GITHUB ACTIONS STATUS CHECK                     ║" -ForegroundColor Cyan
Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Cyan
Write-Host ""

# Check if gh CLI is installed
try {
    $ghVersion = gh --version 2>&1 | Select-Object -First 1
    Write-Host "✅ GitHub CLI detected: $ghVersion" -ForegroundColor Green
    Write-Host ""
} catch {
    Write-Host "❌ GitHub CLI (gh) is not installed" -ForegroundColor Red
    Write-Host ""
    Write-Host "Install it from: https://cli.github.com/" -ForegroundColor Yellow
    Write-Host "Or use: winget install --id GitHub.cli" -ForegroundColor Cyan
    Write-Host ""
    exit 1
}

# Get current repository
try {
    $repoInfo = gh repo view --json nameWithOwner,url | ConvertFrom-Json
    Write-Host "📦 Repository: $($repoInfo.nameWithOwner)" -ForegroundColor Cyan
    Write-Host "🔗 URL: $($repoInfo.url)" -ForegroundColor Gray
    Write-Host ""
} catch {
    Write-Host "❌ Not in a git repository or not authenticated with GitHub" -ForegroundColor Red
    Write-Host ""
    Write-Host "Run: gh auth login" -ForegroundColor Yellow
    exit 1
}

# Get latest workflow runs
Write-Host "🔍 Fetching latest workflow runs..." -ForegroundColor Yellow
Write-Host ""

try {
    $runs = gh run list --workflow="24/7 Recorder" --limit 5 --json databaseId,status,conclusion,createdAt,displayTitle,url | ConvertFrom-Json
    
    if ($runs.Count -eq 0) {
        Write-Host "❌ No workflow runs found for '24/7 Recorder'" -ForegroundColor Red
        Write-Host ""
        Write-Host "Check if the workflow file exists: .github/workflows/recorder.yml" -ForegroundColor Yellow
        exit 1
    }
    
    Write-Host "Recent runs:" -ForegroundColor White
    Write-Host ""
    
    $activeRun = $null
    foreach ($run in $runs) {
        $statusIcon = switch ($run.status) {
            "in_progress" { "🟡" }
            "completed" { 
                if ($run.conclusion -eq "success") { "✅" }
                elseif ($run.conclusion -eq "failure") { "❌" }
                else { "⚠️" }
            }
            default { "⚪" }
        }
        
        $timeAgo = (Get-Date) - [DateTime]::Parse($run.createdAt)
        $timeStr = if ($timeAgo.TotalHours -lt 1) {
            "$([math]::Floor($timeAgo.TotalMinutes)) minutes ago"
        } else {
            "$([math]::Floor($timeAgo.TotalHours)) hours ago"
        }
        
        Write-Host "  $statusIcon Run #$($run.databaseId) - $($run.status) - $timeStr" -ForegroundColor Gray
        Write-Host "     $($run.url)" -ForegroundColor DarkGray
        
        if ($run.status -eq "in_progress" -and $null -eq $activeRun) {
            $activeRun = $run
        }
    }
    
    Write-Host ""
    
    if ($null -eq $activeRun) {
        Write-Host "⚠️  No active workflow runs found" -ForegroundColor Yellow
        Write-Host ""
        Write-Host "The tunnel only works while a workflow is running." -ForegroundColor White
        Write-Host "Start a new run with:" -ForegroundColor White
        Write-Host "  gh workflow run '24/7 Recorder'" -ForegroundColor Cyan
        Write-Host ""
        exit 0
    }
    
    Write-Host "✅ Active run found: #$($activeRun.databaseId)" -ForegroundColor Green
    Write-Host ""
    
    # Get logs for the active run
    Write-Host "📜 Fetching logs to find tunnel URL..." -ForegroundColor Yellow
    Write-Host ""
    
    try {
        $logs = gh run view $activeRun.databaseId --log 2>&1 | Out-String
        
        # Extract tunnel URL from logs
        if ($logs -match 'https://[a-zA-Z0-9-]+\.trycloudflare\.com') {
            $tunnelUrl = $matches[0]
            
            Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Green
            Write-Host "║                    🌐 WEB UI ACCESS                        ║" -ForegroundColor Green
            Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Green
            Write-Host ""
            Write-Host "   $tunnelUrl" -ForegroundColor Cyan
            Write-Host ""
            
            # Copy to clipboard
            try {
                Set-Clipboard -Value $tunnelUrl
                Write-Host "✅ URL copied to clipboard!" -ForegroundColor Green
            } catch {
                Write-Host "ℹ️  Copy the URL above to access the UI" -ForegroundColor Yellow
            }
            
            Write-Host ""
            Write-Host "Testing tunnel connectivity..." -ForegroundColor Yellow
            
            # Quick connectivity test
            try {
                $response = Invoke-WebRequest -Uri $tunnelUrl -Method Head -TimeoutSec 10 -UseBasicParsing -ErrorAction Stop
                Write-Host "✅ Tunnel is accessible (HTTP $($response.StatusCode))" -ForegroundColor Green
            } catch {
                $statusCode = $_.Exception.Response.StatusCode.value__
                if ($statusCode) {
                    Write-Host "⚠️  Tunnel responds but with HTTP $statusCode" -ForegroundColor Yellow
                    
                    if ($statusCode -eq 502 -or $statusCode -eq 503 -or $statusCode -eq 504) {
                        Write-Host ""
                        Write-Host "🔍 Gateway error detected. Possible causes:" -ForegroundColor Yellow
                        Write-Host "  • The application hasn't started yet (wait 1-2 minutes)" -ForegroundColor Gray
                        Write-Host "  • The app crashed during startup" -ForegroundColor Gray
                        Write-Host "  • Port 8080 is not accessible" -ForegroundColor Gray
                        Write-Host ""
                        Write-Host "Check the 'Build and start recorder' step in the workflow logs:" -ForegroundColor White
                        Write-Host "  $($activeRun.url)" -ForegroundColor Cyan
                    }
                } else {
                    Write-Host "❌ Cannot connect to tunnel: $_" -ForegroundColor Red
                }
            }
            
        } else {
            Write-Host "⚠️  Tunnel URL not found in logs yet" -ForegroundColor Yellow
            Write-Host ""
            Write-Host "This could mean:" -ForegroundColor White
            Write-Host "  • The workflow hasn't reached the tunnel setup step yet" -ForegroundColor Gray
            Write-Host "  • The tunnel creation failed" -ForegroundColor Gray
            Write-Host ""
            Write-Host "Check the workflow progress:" -ForegroundColor White
            Write-Host "  $($activeRun.url)" -ForegroundColor Cyan
            Write-Host ""
            
            # Check if we can see any errors in the logs
            if ($logs -match "(?i)(error|failed|fatal)") {
                Write-Host "⚠️  Errors detected in logs. Check the workflow for details." -ForegroundColor Yellow
            }
        }
        
    } catch {
        Write-Host "❌ Failed to fetch logs: $_" -ForegroundColor Red
        Write-Host ""
        Write-Host "View logs manually:" -ForegroundColor White
        Write-Host "  $($activeRun.url)" -ForegroundColor Cyan
    }
    
} catch {
    Write-Host "❌ Error: $_" -ForegroundColor Red
    exit 1
}

Write-Host ""
