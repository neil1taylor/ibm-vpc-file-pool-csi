# result.ps1 - Turns a DiskSpd result (XML) into the fio result a Linux VM of the
# performance test prints, so one collector (collect.sh) reads both. Dot-sourced by
# load.ps1 in the VM and by test-perf.sh.

# ConvertTo-PerfJson XML - one line of JSON shaped like fio's: jobs[0].read and
# jobs[0].write, each with iops and lat_ns (max, and the 95th and 99th percentiles,
# in nanoseconds). IOPS are the I/Os of all targets over the test time. DiskSpd
# measures latency from the moment it issues an I/O, which matches fio's total latency.
# A phase with no I/O has no latency, and the collector drops such a line.
function ConvertTo-PerfJson([string] $Xml) {
    $ts = ([xml] $Xml).Results.TimeSpan
    $secs = [double] $ts.TestTimeSeconds
    $reads = 0; $writes = 0
    foreach ($target in $ts.SelectNodes('Thread/Target')) {
        $reads += [long] $target.ReadCount
        $writes += [long] $target.WriteCount
    }
    $buckets = @{}
    foreach ($b in $ts.SelectNodes('Latency/Bucket')) { $buckets[[string] $b.Percentile] = $b }
    function Side($count, $field) {
        $side = [ordered]@{ iops = if ($secs -gt 0) { [math]::Round($count / $secs, 3) } else { 0 } }
        if ($buckets.ContainsKey('95') -and $buckets['95'].$field) {
            $ns = { param($p) [long]([double] $buckets[$p].$field * 1000000) }
            $side.lat_ns = [ordered]@{
                max        = & $ns '100'
                percentile = [ordered]@{ '95.000000' = & $ns '95'; '99.000000' = & $ns '99' }
            }
        }
        $side
    }
    $job = [ordered]@{ read = (Side $reads 'ReadMilliseconds'); write = (Side $writes 'WriteMilliseconds') }
    @{ jobs = @($job) } | ConvertTo-Json -Depth 6 -Compress
}

# Format-PerfLine EPOCH PHASE JSON - the line for the serial console.
function Format-PerfLine($Epoch, $Phase, $Json) {
    "PERF $Epoch $Phase $Json"
}
