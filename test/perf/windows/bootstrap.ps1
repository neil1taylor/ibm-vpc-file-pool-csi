# bootstrap.ps1 - The start-up task of the prepared Windows image of the
# performance test (prepare-windows-image.sh puts it at C:\perf\bootstrap.ps1).
# At every boot it waits up to two minutes for the config disk (volume label
# PERFCFG) and runs the load.ps1 on it. A VM without that disk does nothing, so
# the image itself can be started for preparation.

for ($i = 0; $i -lt 24; $i++) {
    $volume = Get-Volume | Where-Object FileSystemLabel -eq 'PERFCFG' | Select-Object -First 1
    if ($volume -and $volume.DriveLetter) {
        & powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$($volume.DriveLetter):\load.ps1"
        exit $LASTEXITCODE
    }
    Start-Sleep -Seconds 5
}
