package poolbench

import (
	"embed"
	"fmt"
	"math"
	"strings"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

// LinuxDisk is the load disk of a Linux VM.
const LinuxDisk = "/dev/vdb"

// WindowsDisk is the load disk of a Windows VM. load.ps1 finds the disk number
// by the disk's serial number and puts it in place of #PERFDISK.
const WindowsDisk = `\\.\PhysicalDrive#PERFDISK`

// WindowsDiskSerial is the serial number of a Windows VM's load disk.
const WindowsDiskSerial = "perfload"

//go:embed guest/windows/load.ps1 guest/windows/perf.ps1
var windowsScripts embed.FS

type blockJob struct {
	label   string // as fio takes it, for example 4k
	kib     int64
	percent int32
}

// Profile is the load of one VM, built from a PoolBenchLoad: a steady rate,
// then a short burst, over and over. It makes the same fio jobs as
// test/perf/profile.sh.
type Profile struct {
	load    v1alpha1.PoolBenchLoad
	readPct int64
	jobs    []blockJob
}

// NewProfile checks the load and builds its profile.
func NewProfile(load v1alpha1.PoolBenchLoad) (*Profile, error) {
	mix, err := v1alpha1.ParseBlockMix(load.BlockMix)
	if err != nil {
		return nil, fmt.Errorf("block mix: %w", err)
	}
	p := &Profile{load: load, readPct: 70}
	if load.ReadPercent != nil {
		p.readPct = int64(*load.ReadPercent)
	}
	for _, b := range mix {
		label := fmt.Sprintf("%dk", b.KiB)
		if b.KiB >= 1024 && b.KiB%1024 == 0 {
			label = fmt.Sprintf("%dm", b.KiB/1024)
		}
		p.jobs = append(p.jobs, blockJob{label: label, kib: b.KiB, percent: b.Percent})
	}
	return p, nil
}

func (p *Profile) area() string { return fmt.Sprintf("%dG", p.load.AreaGiB) }

// JobList is the jobs of one phase, one argument to a line: one job for each
// block size, each with its share of total. fio takes one rate for reads and
// one for writes, so each job's rate is split by the read share; reads round
// down. A job never has a rate of zero, which fio reads as no limit.
func (p *Profile) JobList(phase string, total int64) []string {
	var out []string
	for _, j := range p.jobs {
		n := max(total*int64(j.percent)/100, 1)
		reads := n * p.readPct / 100
		out = append(out, "--name="+phase+"-"+j.label, "--bs="+j.label, fmt.Sprintf("--rate_iops=%d,%d", reads, n-reads))
	}
	return out
}

// Common is the options every load phase uses: random reads and writes at the
// read share, 4 I/Os in flight per job, no guest cache, the total latency, a
// different random pattern in each fio process, and one JSON result.
func (p *Profile) Common(device, engine string) []string {
	return []string{
		"--filename=" + device, "--direct=1", "--ioengine=" + engine, "--rw=randrw",
		fmt.Sprintf("--rwmixread=%d", p.readPct), "--iodepth=4", "--size=" + p.area(),
		"--randrepeat=0", "--lat_percentiles=1", "--time_based", "--group_reporting", "--output-format=json",
	}
}

// LinuxFill writes a Linux VM's test area once, so reads are answered from
// data that was written and not from holes.
func (p *Profile) LinuxFill() string {
	return fmt.Sprintf("fio --name=fill --filename=%s --rw=write --bs=1M --size=%s --direct=1 --ioengine=libaio --iodepth=8", LinuxDisk, p.area())
}

// WindowsFillArgs writes a Windows VM's test area once.
func (p *Profile) WindowsFillArgs() []string {
	return []string{"--name=fill", "--filename=" + WindowsDisk, "--rw=write", "--bs=1M", "--size=" + p.area(),
		"--direct=1", "--ioengine=windowsaio", "--thread", "--iodepth=8"}
}

func (p *Profile) phase(phase string) (iops int64, secs int32) {
	if phase == "burst" {
		return int64(p.load.BurstIOPS), p.load.BurstSeconds
	}
	return int64(p.load.SteadyIOPS), p.load.PeriodSeconds - p.load.BurstSeconds
}

// WindowsPhaseArgs is the fio arguments of one phase of a Windows VM: the
// same jobs as a Linux VM, with the Windows load disk and I/O engine.
func (p *Profile) WindowsPhaseArgs(phase string) []string {
	iops, secs := p.phase(phase)
	out := append(p.Common(WindowsDisk, "windowsaio"), "--thread", fmt.Sprintf("--runtime=%d", secs))
	return append(out, p.JobList(phase, iops)...)
}

// IBMOpsPerIO is how many share operations IBM counts, on average, for one I/O
// of the block mix. IBM counts one operation for every 16 KiB of an I/O, which
// is why the share can hit its IOPS limit while the VMs' own IOPS look lower.
func (p *Profile) IBMOpsPerIO() float64 {
	var total float64
	for _, j := range p.jobs {
		total += float64(j.percent) / 100 * float64((j.kib+15)/16)
	}
	return math.Round(total*10) / 10
}

// linuxLoadScript is the script a Linux VM runs for ever. Each VM waits a
// different time before its first period, so the bursts of many VMs do not
// line up. After each phase it sends one line through send.sh.
func (p *Profile) linuxLoadScript() string {
	common := strings.Join(p.Common(LinuxDisk, "libaio"), " ")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Load for one VM: a steady rate, then a short burst, over and over.\n")
	fmt.Fprintf(&b, "OFFSET=$(( $(cksum /proc/sys/kernel/random/boot_id | cut -d' ' -f1) %% %d ))\n", p.load.PeriodSeconds)
	b.WriteString("sleep \"$OFFSET\"\nwhile true; do\n")
	for _, phase := range []string{"steady", "burst"} {
		iops, secs := p.phase(phase)
		b.WriteString("  while [ \"$(sh /root/state.sh)\" = paused ]; do sleep 30; done\n")
		if phase == "steady" {
			// A patch window has only bursts.
			b.WriteString("  if [ \"$(sh /root/state.sh)\" != burst ]; then\n")
		}
		fmt.Fprintf(&b, "  out=$(fio %s --runtime=%d", common, secs)
		jobs := p.JobList(phase, iops)
		for i := 0; i+2 < len(jobs); i += 3 {
			fmt.Fprintf(&b, " \\\n    %s %s %s", jobs[i], jobs[i+1], jobs[i+2])
		}
		fmt.Fprintf(&b, " | tr -d '\\n ')\n  sh /root/send.sh \"PERF $(date +%%s) %s $out\"\n", phase)
		if phase == "steady" {
			b.WriteString("  fi\n")
		}
	}
	b.WriteString("done\n")
	return b.String()
}

// sendScript writes one line to the serial console and posts it to the
// collector. A failed post is dropped: the console line stays as a fallback.
func sendScript(url, token string) string {
	return fmt.Sprintf("#!/bin/sh\n# Sends one result line to the serial console and to the PoolBench collector.\n"+
		"echo \"$1\" > /dev/ttyS0\n"+
		"curl -s -m 10 -H 'Authorization: Bearer %s' --data-binary \"$1\" '%s' > /dev/null 2>&1 || true\n", token, url)
}

// stateURL is where a VM asks whether the run is paused: the results URL
// with /state in place of /results.
func stateURL(resultsURL string) string {
	return strings.TrimSuffix(resultsURL, "/results") + "/state"
}

// stateScript prints "paused" while the run is paused. If the collector
// cannot be reached it prints nothing, so the load goes on.
func stateScript(url, token string) string {
	return fmt.Sprintf("#!/bin/sh\n# Prints the run's state from the PoolBench collector: run or paused.\n"+
		"curl -s -m 10 -H 'Authorization: Bearer %s' '%s' 2>/dev/null\n", token, stateURL(url))
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	return prefix + strings.Join(lines, "\n"+prefix) + "\n"
}

// loadService runs the load as a system service, so it starts again after a
// restart of the VM (the boot storm event).
const loadService = `[Unit]
Description=PoolBench load
After=network-online.target

[Service]
ExecStart=/bin/sh /root/load.sh
Restart=always

[Install]
WantedBy=multi-user.target
`

// LinuxUserData is the cloud-init file of a Linux VM: it writes the load and
// send scripts, installs fio, fills the test area, says PERF-READY, and starts
// the load. It holds the run's token, so it is kept in a Secret.
func (p *Profile) LinuxUserData(url, token string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\nwrite_files:\n")
	b.WriteString("  - path: /root/send.sh\n    permissions: \"0700\"\n    content: |\n")
	b.WriteString(indent(sendScript(url, token), "      "))
	b.WriteString("  - path: /root/state.sh\n    permissions: \"0700\"\n    content: |\n")
	b.WriteString(indent(stateScript(url, token), "      "))
	b.WriteString("  - path: /root/load.sh\n    permissions: \"0755\"\n    content: |\n")
	b.WriteString(indent(p.linuxLoadScript(), "      "))
	b.WriteString("  - path: /etc/systemd/system/poolbench-load.service\n    permissions: \"0644\"\n    content: |\n")
	b.WriteString(indent(loadService, "      "))
	fmt.Fprintf(&b, "runcmd:\n  - |\n    set -e\n    dnf install -y -q fio\n    %s > /dev/null\n    sh /root/send.sh PERF-READY\n", p.LinuxFill())
	b.WriteString("  - systemctl daemon-reload\n  - systemctl enable --now poolbench-load\n")
	return b.String()
}

func lines(args []string) string { return strings.Join(args, "\n") + "\n" }

// WindowsFiles is the config disk of a Windows VM, by file name: the
// settings, the fio arguments of the fill and the two phases, and the load
// scripts. It holds the run's token, so it is kept in a Secret.
func (p *Profile) WindowsFiles(url, token string) map[string]string {
	files := map[string]string{
		"settings.txt": fmt.Sprintf("period=%d\nserial=%s\ncollector=%s\nstate=%s\ntoken=%s\n",
			p.load.PeriodSeconds, WindowsDiskSerial, url, stateURL(url), token),
		"fill.args":   lines(p.WindowsFillArgs()),
		"steady.args": lines(p.WindowsPhaseArgs("steady")),
		"burst.args":  lines(p.WindowsPhaseArgs("burst")),
	}
	for _, name := range []string{"load.ps1", "perf.ps1"} {
		data, err := windowsScripts.ReadFile("guest/windows/" + name)
		if err != nil {
			panic(err) // embedded at build time
		}
		files[name] = string(data)
	}
	return files
}
