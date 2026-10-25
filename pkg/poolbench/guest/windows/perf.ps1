# perf.ps1 - Helpers of the Windows load script (load.ps1). Dot-sourced by
# load.ps1 in the VM and by test-perf.sh.

# Format-PerfLine EPOCH PHASE OUTPUT - the serial console line of one phase, as a
# Linux VM prints it: PERF <epoch> <phase> <fio JSON on one line, without spaces>.
# fio's JSON starts at the first "{", after any warning it printed. With no JSON the
# line ends after the phase, and the collector drops it.
function Format-PerfLine($Epoch, $Phase, [string] $Output) {
    $start = $Output.IndexOf('{')
    $json = if ($start -ge 0) { $Output.Substring($start) -replace '\s', '' } else { '' }
    "PERF $Epoch $Phase $json"
}
