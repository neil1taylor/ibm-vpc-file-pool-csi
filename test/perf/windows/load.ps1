# load.ps1 - The load of one Windows VM in the performance test
# (docs/specs/2026-10-24-performance-test-30-vms.md): the same steady rate and
# short bursts as the Linux VMs, run by DiskSpd. It runs from the config disk
# that run-tier.sh gives each Windows VM, started at every boot by the task that
# prepare-windows-image.sh puts in the image (bootstrap.ps1).
#
# Like a Linux VM it fills its test area once, says PERF-READY on the serial
# console, waits a random time so the bursts of many VMs do not line up, and then
# prints one line after each phase: PERF <epoch seconds> <phase> <result JSON>.

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'result.ps1')

$settings = @{}
foreach ($line in Get-Content (Join-Path $PSScriptRoot 'settings.txt')) {
    $key, $value = $line -split '=', 2
    if ($key) { $settings[$key.Trim()] = $value.Trim() }
}
$period = [int] $settings.period
$area = [long] $settings.area
$work = 'C:\perf'
New-Item -ItemType Directory -Force $work | Out-Null

function Send([string] $Text) {
    $port = New-Object System.IO.Ports.SerialPort 'COM1', 115200
    $port.Open()
    try { $port.WriteLine($Text) } finally { $port.Close() }
}

# The load disk is the one with the serial number the VM definition gives it.
$disk = Get-Disk | Where-Object SerialNumber -eq $settings.serial | Select-Object -First 1
if (-not $disk) {
    Send "PERF-ERROR no disk with serial number $($settings.serial)"
    exit 1
}
if ($disk.IsOffline) { Set-Disk -Number $disk.Number -IsOffline $false }
if ($disk.IsReadOnly) { Set-Disk -Number $disk.Number -IsReadOnly $false }

# Write the test area once, so reads are answered from data that was written and not from holes.
# The marker is on the VM's own root disk, so a restart does not fill again.
$filled = Join-Path $work 'perf-filled'
if (-not (Test-Path $filled)) {
    Add-Type -TypeDefinition @'
using System;
using System.IO;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;
public static class PerfRawDisk {
    [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
    static extern SafeFileHandle CreateFile(string name, uint access, uint share, IntPtr security, uint disposition, uint flags, IntPtr template);
    public static void Fill(string path, long bytes) {
        // GENERIC_WRITE, shared read and write, OPEN_EXISTING, FILE_FLAG_WRITE_THROUGH
        SafeFileHandle h = CreateFile(path, 0x40000000, 3, IntPtr.Zero, 3, 0x80000000, IntPtr.Zero);
        if (h.IsInvalid) throw new IOException("cannot open " + path, Marshal.GetLastWin32Error());
        using (FileStream fs = new FileStream(h, FileAccess.Write, 1 << 20, false)) {
            byte[] buf = new byte[1 << 20];
            new Random().NextBytes(buf);
            for (long done = 0; done < bytes; done += buf.Length) fs.Write(buf, 0, buf.Length);
            fs.Flush(true);
        }
    }
}
'@
    [PerfRawDisk]::Fill("\\.\PhysicalDrive$($disk.Number)", $area)
    New-Item -ItemType File $filled | Out-Null
}

Send 'PERF-READY'
Start-Sleep -Seconds (Get-Random -Maximum $period)

$diskspd = Join-Path $PSScriptRoot 'diskspd.exe'
while ($true) {
    foreach ($phase in 'steady', 'burst') {
        # Every run uses a different random pattern, so caches cannot answer from the last run.
        $job = Join-Path $work "$phase.xml"
        $jobText = (Get-Content -Raw (Join-Path $PSScriptRoot "$phase.xml")).Replace('#PERFDISK', "#$($disk.Number)")
        $jobText -replace '<RandSeed>\d+</RandSeed>', "<RandSeed>$(Get-Random -Maximum 2147483647)</RandSeed>" |
            Set-Content -Encoding ASCII $job
        $out = & $diskspd -X $job 2>&1 | Out-String
        $epoch = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
        $start = $out.IndexOf('<Results')
        try {
            Send (Format-PerfLine $epoch $phase (ConvertTo-PerfJson $out.Substring($start)))
        } catch {
            $text = ($out -replace '\s+', ' ').Trim()
            Send "PERF-ERROR $epoch $phase $($text.Substring(0, [math]::Min(400, $text.Length)))"
        }
    }
}
