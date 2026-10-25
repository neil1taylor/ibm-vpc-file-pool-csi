package poolbench

import (
	"fmt"
	"os"
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

func defaultLoad() v1alpha1.PoolBenchLoad {
	read := int32(70)
	return v1alpha1.PoolBenchLoad{
		SteadyIOPS: 30, BurstIOPS: 300, BurstSeconds: 45, PeriodSeconds: 600,
		ReadPercent: &read, BlockMix: "4k:50 16k:30 64k:20", AreaGiB: 3,
	}
}

// TestProfile_MatchesBash builds the same text as print-golden.sh profile.
func TestProfile_MatchesBash(t *testing.T) {
	p, err := NewProfile(defaultLoad())
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	section := func(name string, lines ...string) {
		b.WriteString("== " + name + "\n")
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	section("steady jobs", p.JobList("steady", 30)...)
	section("burst jobs", p.JobList("burst", 300)...)
	section("linux common", p.Common(LinuxDisk, "libaio")...)
	section("linux fill", p.LinuxFill())
	section("windows fill", p.WindowsFillArgs()...)
	section("windows steady", p.WindowsPhaseArgs("steady")...)
	section("windows burst", p.WindowsPhaseArgs("burst")...)
	section("ibm ops per io", fmt.Sprintf("%.1f", p.IBMOpsPerIO()))
	want, err := os.ReadFile("testdata/profile.golden")
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestProfile_RefusesBadMix(t *testing.T) {
	l := defaultLoad()
	l.BlockMix = "4k:60"
	if _, err := NewProfile(l); err == nil {
		t.Fatal("want an error for a mix that is not 100 percent")
	}
}

func TestProfile_IBMOps(t *testing.T) {
	l := defaultLoad()
	l.BlockMix = "64k:100"
	p, _ := NewProfile(l)
	if got := p.IBMOpsPerIO(); got != 4 {
		t.Fatalf("a 64k I/O is 4 IBM operations, got %v", got)
	}
}

// TestUserData checks the cloud-init a Linux VM gets: the load as the Bash
// tooling writes it, and each result line also sent to the collector.
func TestUserData(t *testing.T) {
	p, _ := NewProfile(defaultLoad())
	ud := p.LinuxUserData("http://collector.example:8080/v1/runs/t10k/results", "TOKEN123")
	for _, want := range []string{
		"#cloud-config",
		"dnf install -y -q fio",
		p.LinuxFill(),
		"OFFSET=$(( $(cksum /proc/sys/kernel/random/boot_id | cut -d' ' -f1) % 600 ))",
		"--runtime=555",
		"--runtime=45",
		"--name=burst-64k --bs=64k --rate_iops=42,18",
		`> /dev/ttyS0`,
		"curl ",
		`Authorization: Bearer TOKEN123`,
		"http://collector.example:8080/v1/runs/t10k/results",
		"sh /root/send.sh PERF-READY",
		"nohup sh /root/load.sh",
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("user data lacks %q", want)
		}
	}
	// The ready line and both phases go through the send script.
	if strings.Count(ud, "/root/send.sh") < 4 {
		t.Errorf("want the send script written and used for the ready line and both phases:\n%s", ud)
	}
}

// TestWindowsFiles checks the config disk of a Windows VM: the settings with
// the collector, the fio arguments, and the two scripts.
func TestWindowsFiles(t *testing.T) {
	p, _ := NewProfile(defaultLoad())
	files := p.WindowsFiles("http://collector.example:8080/v1/runs/t10k/results", "TOKEN123")
	for _, name := range []string{"settings.txt", "fill.args", "steady.args", "burst.args", "load.ps1", "perf.ps1"} {
		if files[name] == "" {
			t.Errorf("missing %s", name)
		}
	}
	for _, want := range []string{"period=600", "serial=perfload", "collector=http://collector.example:8080/v1/runs/t10k/results", "token=TOKEN123"} {
		if !strings.Contains(files["settings.txt"], want) {
			t.Errorf("settings.txt lacks %q", want)
		}
	}
	if files["steady.args"] != strings.Join(p.WindowsPhaseArgs("steady"), "\n")+"\n" {
		t.Errorf("steady.args is not the steady phase:\n%s", files["steady.args"])
	}
	if !strings.Contains(files["load.ps1"], "Invoke-WebRequest") {
		t.Error("load.ps1 does not send results to the collector")
	}
}
