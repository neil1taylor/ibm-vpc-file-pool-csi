# start.ps1 - Starts the load of a Windows test VM (load.ps1 on the config disk).
# Sent through the QEMU guest agent by run-tier.sh and by the PoolBench service.
# It does nothing if the load runs, and fails until the config disk is there.
if (Get-CimInstance Win32_Process -Filter "Name = 'powershell.exe'" | Where-Object CommandLine -like '*load.ps1*') { 'running'; exit 0 }
$volume = Get-Volume | Where-Object FileSystemLabel -eq 'PERFCFG' | Select-Object -First 1
if (-not ($volume -and $volume.DriveLetter)) { 'no config disk yet'; exit 1 }
Start-Process powershell.exe -WindowStyle Hidden -ArgumentList "-NoProfile -ExecutionPolicy Bypass -File $($volume.DriveLetter):\load.ps1"
'started'
