# load.ps1 - The load of one Windows VM in the performance test
# (docs/specs/2026-10-24-performance-test-30-vms.md): the same fio jobs as a
# Linux VM, run by fio for Windows. It runs from the config disk that run-tier.sh
# gives each Windows VM; run-tier.sh starts it through the guest agent.
#
# Like a Linux VM it fills its test area once, says PERF-READY on the serial
# console, waits a random time so the bursts of many VMs do not line up, and then
# prints one line after each phase: PERF <epoch seconds> <phase> <fio JSON>.

# fio and the disk cmdlets write to the error stream; that must not stop the load.
$ErrorActionPreference = 'Continue'
. (Join-Path $PSScriptRoot 'perf.ps1')

$settings = @{}
foreach ($line in Get-Content (Join-Path $PSScriptRoot 'settings.txt')) {
    $key, $value = $line -split '=', 2
    if ($key) { $settings[$key.Trim()] = $value.Trim() }
}
$work = 'C:\perf'
$fio = 'C:\Program Files\fio\fio.exe'
New-Item -ItemType Directory -Force $work | Out-Null

# Send writes one line to the serial console and, when the settings name a PoolBench
# collector, posts it there too. A failed post is dropped: the console line stays.
function Send([string] $Text) {
    $port = New-Object System.IO.Ports.SerialPort 'COM1', 115200
    $port.Open()
    try { $port.WriteLine($Text) } finally { $port.Close() }
    if ($settings.collector) {
        try {
            Invoke-WebRequest -UseBasicParsing -Method Post -Uri $settings.collector -TimeoutSec 10 `
                -Headers @{ Authorization = "Bearer $($settings.token)" } -Body $Text | Out-Null
        } catch { }
    }
}

# The load disk is the one with the serial number the VM definition gives it.
$disk = Get-Disk | Where-Object SerialNumber -eq $settings.serial | Select-Object -First 1
if (-not $disk) {
    Send "PERF-ERROR no disk with serial number $($settings.serial)"
    exit 1
}
if ($disk.IsOffline) { Set-Disk -Number $disk.Number -IsOffline $false }
if ($disk.IsReadOnly) { Set-Disk -Number $disk.Number -IsReadOnly $false }

# Get-FioArgs NAME - the arguments in NAME.args on the config disk, with the load disk's number.
function Get-FioArgs([string] $Name) {
    Get-Content (Join-Path $PSScriptRoot "$Name.args") | Where-Object { $_ } |
        ForEach-Object { $_.Replace('#PERFDISK', "$($disk.Number)") }
}

# Write the test area once, so reads are answered from data that was written and not from holes.
# The marker is on the VM's own root disk, so a restart does not fill again.
$filled = Join-Path $work 'perf-filled'
if (-not (Test-Path $filled)) {
    & $fio (Get-FioArgs 'fill') 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Send "PERF-ERROR the fill failed with exit code $LASTEXITCODE"
        exit 1
    }
    New-Item -ItemType File $filled | Out-Null
}

Send 'PERF-READY'
Start-Sleep -Seconds (Get-Random -Maximum ([int] $settings.period))

# Wait-WhilePaused holds the load while the PoolBench run is paused. If the
# collector cannot be reached, the load goes on.
function Wait-WhilePaused {
    if (-not $settings.state) { return }
    while ($true) {
        try {
            $state = (Invoke-WebRequest -UseBasicParsing -Uri $settings.state -TimeoutSec 10 `
                -Headers @{ Authorization = "Bearer $($settings.token)" }).Content.Trim()
        } catch { return }
        if ($state -ne 'paused') { return }
        Start-Sleep -Seconds 30
    }
}

while ($true) {
    foreach ($phase in 'steady', 'burst') {
        Wait-WhilePaused
        $out = & $fio (Get-FioArgs $phase) 2>$null | Out-String
        Send (Format-PerfLine ([DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) $phase $out)
    }
}
