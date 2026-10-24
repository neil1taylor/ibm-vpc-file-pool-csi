#!/usr/bin/env bash
# profile.sh — The load one VM puts on its disk in the performance test
# (docs/specs/2026-10-24-performance-test-30-vms.md): a steady rate, then a
# short burst, over and over. This file only builds the text of the script a
# VM runs. Sourced by the other perf scripts and by test-perf.sh.

# Share of reads in the mix, in percent. The spec fixes it at 70/30.
PERF_READ_PCT=70

# The block sizes of the load and how much of the I/O each one takes, in percent.
# They stand for a general-purpose mix between 4k and 64k. They are separate jobs
# with one fixed block size each because fio counts every I/O of a job with a
# range of block sizes as the smallest size when it applies rate_iops: with 4k-64k
# the real rate came out about 8 times too low (first smoke run, 2026-10-24).
PERF_BLOCK_MIX="4k:50 16k:30 64k:20"

# perf_split_iops TOTAL — fio takes one rate for reads and one for writes, so
# a total rate is split by the mix. Reads round down; writes get the rest.
perf_split_iops() {
    local total="$1" reads
    reads=$((total * PERF_READ_PCT / 100))
    echo "$reads,$((total - reads))"
}

# perf_job_iops TOTAL WEIGHT — the share of a total rate that one block size takes,
# at least 1 so a job never has a rate of zero (which fio reads as no limit).
perf_job_iops() {
    local n=$(($1 * $2 / 100))
    [ "$n" -ge 1 ] || n=1
    echo "$n"
}

# perf_fio_job_list PHASE TOTAL_IOPS — the jobs of one phase, one argument to a line:
# one job for each block size in PERF_BLOCK_MIX, each with its share of TOTAL_IOPS.
perf_fio_job_list() {
    local phase="$1" total="$2" entry bs weight
    for entry in $PERF_BLOCK_MIX; do
        bs=${entry%%:*}
        weight=${entry##*:}
        printf '%s\n' "--name=$phase-$bs" "--bs=$bs" "--rate_iops=$(perf_split_iops "$(perf_job_iops "$total" "$weight")")"
    done
}

# perf_fio_jobs PHASE TOTAL_IOPS — the "--name ... --bs ... --rate_iops" part of a fio
# command in a shell script, one job to a line.
perf_fio_jobs() {
    local name bs rate
    while read -r name && read -r bs && read -r rate; do
        printf ' \\\n    %s %s %s' "$name" "$bs" "$rate"
    done < <(perf_fio_job_list "$1" "$2")
}

# perf_ibm_ops_per_io — how many share operations IBM counts, on average, for one I/O of the
# block mix. IBM counts one operation for every 16 KiB of an I/O (so a 64k I/O is four), which
# is why the share can hit its IOPS limit while the VMs' own IOPS look lower. One decimal.
perf_ibm_ops_per_io() {
    local entry bs weight kib
    for entry in $PERF_BLOCK_MIX; do
        bs=${entry%%:*}
        weight=${entry##*:}
        case "$bs" in
            *[kK]) kib=${bs%[kK]} ;;
            *[mM]) kib=$((${bs%[mM]} * 1024)) ;;
            *) kib=$((bs / 1024)) ;;
        esac
        echo "$weight $(((kib + 15) / 16))"
    done | awk '{ total += $1 / 100 * $2 } END { printf "%.1f\n", total }'
}

# perf_prefill_command DEVICE SIZE — writes the test area once, before any
# measuring, so reads are answered from data that was written and not from holes.
perf_prefill_command() {
    echo "fio --name=fill --filename=$1 --rw=write --bs=1M --size=$2 --direct=1 --ioengine=libaio --iodepth=8"
}

# perf_fio_common DEVICE SIZE ENGINE — the options every load phase of every VM uses, one to a
# line: random reads and writes at the read share of the mix, 4 I/Os in flight per job, no guest
# cache, the total latency, and one JSON result for all the jobs.
perf_fio_common() {
    printf '%s\n' "--filename=$1" --direct=1 "--ioengine=$3" --rw=randrw "--rwmixread=$PERF_READ_PCT" --iodepth=4 \
        "--size=$2" --randrepeat=0 --lat_percentiles=1 --time_based --group_reporting --output-format=json
}

# perf_linux_load_script AVG_IOPS BURST_IOPS BURST_SECS PERIOD_SECS DEVICE [SIZE] —
# the shell script a Linux VM runs for ever. A period is the steady phase and
# then the burst. Each VM waits a different time before its first period, so
# the bursts of many VMs do not line up. Every fio process uses a different random
# pattern (randrepeat=0): by default fio repeats the same offsets in each process, which
# would let caches answer after the first period. Latency is the total latency
# (lat_percentiles=1), which includes waiting before submission, not only completion.
# After each phase the script prints one line to the serial console:
# PERF <epoch seconds> <phase> <fio result as one line of JSON>.
perf_linux_load_script() {
    local avg="$1" burst="$2" burst_secs="$3" period="$4" device="$5" size="${6:-3G}"
    local steady_secs common
    steady_secs=$((period - burst_secs))
    common=$(perf_fio_common "$device" "$size" libaio | paste -sd' ' -)
    cat <<EOF
#!/bin/sh
# Load for one VM: a steady rate, then a short burst, over and over.
OFFSET=\$(( \$(cksum /proc/sys/kernel/random/boot_id | cut -d' ' -f1) % $period ))
sleep "\$OFFSET"
while true; do
  out=\$(fio $common --runtime=$steady_secs$(perf_fio_jobs steady "$avg") | tr -d '\\n ')
  echo "PERF \$(date +%s) steady \$out" > /dev/ttyS0
  out=\$(fio $common --runtime=$burst_secs$(perf_fio_jobs burst "$burst") | tr -d '\\n ')
  echo "PERF \$(date +%s) burst \$out" > /dev/ttyS0
done
EOF
}

# The load disk of a Windows VM. The VM finds the disk number by the disk's serial
# number and puts it in place of #PERFDISK.
PERF_WINDOWS_DISK='\\.\PhysicalDrive#PERFDISK'

# perf_windows_fio_args PHASE TOTAL_IOPS SECS SIZE — the fio arguments of one phase of a
# Windows VM, one to a line: the same jobs as a Linux VM, with the Windows load disk and
# the Windows I/O engine, so the two kinds of VM compare like with like.
perf_windows_fio_args() {
    perf_fio_common "$PERF_WINDOWS_DISK" "$4" windowsaio
    printf '%s\n' --thread "--runtime=$3"
    perf_fio_job_list "$1" "$2"
}

# perf_windows_fill_args SIZE — the fio arguments that write a Windows VM's test area once.
perf_windows_fill_args() {
    printf '%s\n' --name=fill "--filename=$PERF_WINDOWS_DISK" --rw=write --bs=1M "--size=$1" --direct=1 --ioengine=windowsaio --thread --iodepth=8
}

