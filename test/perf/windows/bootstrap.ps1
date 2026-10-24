# bootstrap.ps1 - The start-up task of the prepared Windows image of the
# performance test (prepare-windows-image.sh puts it at C:\perf\bootstrap.ps1).
# At every boot it waits for the config disk (volume label PERFCFG) and runs the
# load.ps1 on it. Early in a boot the storage service can list no volumes for
# minutes, so it waits up to 10 minutes. A VM without the disk does nothing, so
# the image itself can be started for preparation. What it did is in
# C:\perf\bootstrap.log, and the load script's own output in C:\perf\load.log.

$log = 'C:\perf\bootstrap.log'
function Note([string] $Text) { Add-Content -Path $log -Value "$(Get-Date -Format o) $Text" }

Note 'start'
for ($i = 0; $i -lt 120; $i++) {
    $volume = Get-Volume -ErrorAction SilentlyContinue | Where-Object FileSystemLabel -eq 'PERFCFG' | Select-Object -First 1
    if ($volume -and $volume.DriveLetter) {
        Note "config disk at $($volume.DriveLetter):, starting the load"
        & powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$($volume.DriveLetter):\load.ps1" *> 'C:\perf\load.log'
        Note "the load ended with exit code $LASTEXITCODE"
        exit $LASTEXITCODE
    }
    Start-Sleep -Seconds 5
}
Note 'no config disk after 10 minutes'
