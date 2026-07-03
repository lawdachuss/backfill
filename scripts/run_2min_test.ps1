# Run chaturbate-dvr for ~2 minutes to test the pipeline end-to-end
# Captures all output, then gracefully shuts down and waits for pipeline to finish.

param(
    [string]$Username = "8abycat",
    [int]$Duration = 2  # minutes
)

$ProjectDir = "C:\Users\basud\OneDrive\Desktop\MiniDelectableService"
$LogFile = Join-Path $ProjectDir "test_run_output.log"

Write-Host "=== Starting DVR test for $Username ($Duration min) ==="
Write-Host "Log: $LogFile"
Write-Host ""

# Start the app in a new window so we can capture output
$psi = New-object System.Diagnostics.ProcessStartInfo
$psi.CreateNoWindow = $false
$psi.UseShellExecute = $false
$psi.RedirectStandardOutput = $true
$psi.RedirectStandardError = $true
$psi.FileName = Join-Path $ProjectDir "chaturbate-dvr-test.exe"
$psi.Arguments = "--username $Username --site chaturbate --max-duration $Duration --min-duration-before-upload 0 --framerate 30 --resolution 720 --interval 1"
$psi.WorkingDirectory = $ProjectDir

Write-Host "Starting process: $($psi.FileName) $($psi.Arguments)"
$proc = [System.Diagnostics.Process]::Start($psi)

Write-Host "Process ID: $($proc.Id)"
Write-Host ""

# Read output asynchronously
$outputBuilder = New-Object System.Text.StringBuilder
$errorBuilder = New-Object System.Text.StringBuilder

$outputHandler = Register-ObjectEvent -InputObject $proc -EventName OutputDataReceived -Action {
    $line = $EventArgs.Data
    if ($line -ne $null) {
        Write-Host "[DVR] $line"
        $Event.MessageData.AppendLine($line) | Out-Null
    }
} -MessageData $outputBuilder

$errorHandler = Register-ObjectEvent -InputObject $proc -EventName ErrorDataReceived -Action {
    $line = $EventArgs.Data
    if ($line -ne $null) {
        Write-Host "[DVR:ERR] $line"
        $Event.MessageData.AppendLine($line) | Out-Null
    }
} -MessageData $errorBuilder

$proc.BeginOutputReadLine()
$proc.BeginErrorReadLine()

# Wait for recording time + some buffer for pipeline processing
$SecondsToWait = ($Duration * 60) + 30  # duration minutes + 30s buffer
Write-Host "Waiting $SecondsToWait seconds for recording + pipeline..."
Write-Host ""

for ($i = 0; $i -lt $SecondsToWait; $i++) {
    if ($proc.HasExited) {
        Write-Host ""
        Write-Host "Process exited early (exit code: $($proc.ExitCode))"
        break
    }
    if ($i % 30 -eq 0 -and $i -gt 0) {
        Write-Host "  ... $i seconds elapsed"
    }
    Start-Sleep -Seconds 1
}

if (-not $proc.HasExited) {
    Write-Host ""
    Write-Host "=== Sending Ctrl+C to stop the recording ==="
    # Send Ctrl+C via WM_CLOSE
    $proc.CloseMainWindow() | Out-Null
    Start-Sleep -Seconds 5
    
    if (-not $proc.HasExited) {
        Write-Host "Graceful shutdown didn't work, trying harder..."
        $proc.Kill() | Out-Null
        Start-Sleep -Seconds 2
    }
}

Write-Host ""
Write-Host "=== Test Complete ==="

# Save full output to log file
$outputBuilder.ToString() | Out-File -FilePath $LogFile -Encoding UTF8
Write-Host "Full output saved to: $LogFile"

# Print summary of key pipeline events
Write-Host ""
Write-Host "=== Pipeline Events Summary ==="
$output = $outputBuilder.ToString()
$lines = $output -split "`n"
foreach ($line in $lines) {
    if ($line -match "starting to record|pipeline:|sprite:|thumb:|preview:|upload:|cleanup:|saved recording|completed|failed|error") {
        Write-Host "  $line"
    }
}
