package webhook

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func int32p(v int32) *int32 { return &v }

// validPoolBench is the 10,000 IOPS tier of the performance test spec, with
// the defaults the API server fills in.
func validPoolBench() *v1alpha1.PoolBench {
	return &v1alpha1.PoolBench{
		ObjectMeta: metav1.ObjectMeta{Name: "t10k"},
		Spec: v1alpha1.PoolBenchSpec{
			Profile:        "dp2",
			ShareSizeGB:    3900,
			IOPS:           10000,
			StepVMs:        15,
			MaxVMs:         90,
			WindowsEvery:   int32p(3),
			SettleMinutes:  int32p(15),
			MeasureMinutes: 45,
			Load: v1alpha1.PoolBenchLoad{
				SteadyIOPS: 30, BurstIOPS: 300, BurstSeconds: 45, PeriodSeconds: 600,
				ReadPercent: int32p(70), BlockMix: "4k:50 16k:30 64k:20", AreaGiB: 3,
			},
			Knee:   v1alpha1.PoolBenchKnee{LatencyLimitMs: 20, SlowVMPercent: 25, StallSeconds: 5},
			Events: []v1alpha1.PoolBenchEvent{{Type: v1alpha1.PoolBenchEventBootStorm, AtVMs: 30}},
		},
	}
}

func TestPoolBenchValidator_Valid(t *testing.T) {
	v := &PoolBenchValidator{}
	if _, err := v.ValidateCreate(context.Background(), validPoolBench()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := v.ValidateUpdate(context.Background(), validPoolBench(), validPoolBench()); err != nil {
		t.Fatalf("unexpected error on update: %v", err)
	}
}

func TestPoolBenchValidator_RefusesCreate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha1.PoolBench)
		want   string
	}{
		{"name too long for the run namespace", func(p *v1alpha1.PoolBench) { p.Name = strings.Repeat("a", 41) }, "name"},
		{"not dp2", func(p *v1alpha1.PoolBench) { p.Spec.Profile = "tier-3iops" }, "profile"},
		{"share too small for IBM", func(p *v1alpha1.PoolBench) { p.Spec.ShareSizeGB = 5 }, "shareSizeGB"},
		{"share too large for IBM", func(p *v1alpha1.PoolBench) { p.Spec.ShareSizeGB = 40000 }, "shareSizeGB"},
		{"IOPS outside the dp2 table", func(p *v1alpha1.PoolBench) { p.Spec.IOPS = 50000 }, "IOPS"},
		{"no VMs per step", func(p *v1alpha1.PoolBench) { p.Spec.StepVMs = 0 }, "stepVMs"},
		{"max below one step", func(p *v1alpha1.PoolBench) { p.Spec.MaxVMs = 10 }, "maxVMs"},
		{"max not a whole number of steps", func(p *v1alpha1.PoolBench) { p.Spec.MaxVMs = 100 }, "maxVMs"},
		{"too many VMs", func(p *v1alpha1.PoolBench) { p.Spec.StepVMs = 50; p.Spec.MaxVMs = 550 }, "maxVMs"},
		{"negative windows", func(p *v1alpha1.PoolBench) { p.Spec.WindowsEvery = int32p(-1) }, "windowsEvery"},
		{"negative settle", func(p *v1alpha1.PoolBench) { p.Spec.SettleMinutes = int32p(-1) }, "settleMinutes"},
		{"measure shorter than one period", func(p *v1alpha1.PoolBench) { p.Spec.MeasureMinutes = 5 }, "measureMinutes"},
		{"measure cannot hold a whole steady phase", func(p *v1alpha1.PoolBench) { p.Spec.MeasureMinutes = 19 }, "measureMinutes"},
		{"no steady rate", func(p *v1alpha1.PoolBench) { p.Spec.Load.SteadyIOPS = 0 }, "steadyIOPS"},
		{"burst below steady", func(p *v1alpha1.PoolBench) { p.Spec.Load.BurstIOPS = 10 }, "burstIOPS"},
		{"burst as long as the period", func(p *v1alpha1.PoolBench) { p.Spec.Load.BurstSeconds = 600 }, "burstSeconds"},
		{"burst too short", func(p *v1alpha1.PoolBench) { p.Spec.Load.BurstSeconds = 5 }, "burstSeconds"},
		{"read share over 100", func(p *v1alpha1.PoolBench) { p.Spec.Load.ReadPercent = int32p(101) }, "readPercent"},
		{"block mix not 100", func(p *v1alpha1.PoolBench) { p.Spec.Load.BlockMix = "4k:50 16k:30" }, "blockMix"},
		{"block mix bad size", func(p *v1alpha1.PoolBench) { p.Spec.Load.BlockMix = "4q:100" }, "blockMix"},
		{"block mix bad weight", func(p *v1alpha1.PoolBench) { p.Spec.Load.BlockMix = "4k:x" }, "blockMix"},
		{"no test area", func(p *v1alpha1.PoolBench) { p.Spec.Load.AreaGiB = 0 }, "areaGiB"},
		{"no latency limit", func(p *v1alpha1.PoolBench) { p.Spec.Knee.LatencyLimitMs = 0 }, "latencyLimitMs"},
		{"slow share over 100", func(p *v1alpha1.PoolBench) { p.Spec.Knee.SlowVMPercent = 101 }, "slowVMPercent"},
		{"no stall time", func(p *v1alpha1.PoolBench) { p.Spec.Knee.StallSeconds = 0 }, "stallSeconds"},
		{"event not at a step", func(p *v1alpha1.PoolBench) { p.Spec.Events[0].AtVMs = 20 }, "events"},
		{"event past the last step", func(p *v1alpha1.PoolBench) { p.Spec.Events[0].AtVMs = 105 }, "events"},
		{"event of no known type", func(p *v1alpha1.PoolBench) { p.Spec.Events[0].Type = "Reboot" }, "events"},
		{"disks do not fit the share", func(p *v1alpha1.PoolBench) { p.Spec.ShareSizeGB = 1000; p.Spec.IOPS = 10000 }, "do not fit"},
		{"negative extend", func(p *v1alpha1.PoolBench) { p.Spec.Control.ExtendMinutes = -5 }, "extendMinutes"},
		{"waits for itself", func(p *v1alpha1.PoolBench) { p.Spec.StartAfter = "t10k" }, "startAfter"},
		{"waits for a bad name", func(p *v1alpha1.PoolBench) { p.Spec.StartAfter = "Not_A_Name" }, "startAfter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPoolBench()
			tc.mutate(p)
			_, err := (&PoolBenchValidator{}).ValidateCreate(context.Background(), p)
			if err == nil {
				t.Fatalf("want an error containing %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestPoolBenchValidator_DiskNeed checks the disk sum: 90 VMs of a 30 GiB root
// disk and a 4 GiB load disk need about 3,285 GB, so 3,300 GB is enough and
// 3,200 GB is not.
func TestPoolBenchValidator_DiskNeed(t *testing.T) {
	p := validPoolBench()
	p.Spec.ShareSizeGB = 3300
	if _, err := (&PoolBenchValidator{}).ValidateCreate(context.Background(), p); err != nil {
		t.Fatalf("3,300 GB should hold 90 VMs: %v", err)
	}
	p.Spec.ShareSizeGB = 3200
	if _, err := (&PoolBenchValidator{}).ValidateCreate(context.Background(), p); err == nil {
		t.Fatal("3,200 GB should not hold 90 VMs")
	}
}

func TestPoolBenchValidator_Update(t *testing.T) {
	started := func() *v1alpha1.PoolBench {
		p := validPoolBench()
		p.Status.Phase = v1alpha1.PoolBenchRunning
		return p
	}
	t.Run("control may change while running", func(t *testing.T) {
		p := started()
		p.Spec.Control = v1alpha1.PoolBenchControl{Paused: true, Hold: true, SkipStep: 2, ExtendStep: 2, ExtendMinutes: 30}
		if _, err := (&PoolBenchValidator{}).ValidateUpdate(context.Background(), started(), p); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("an event may be added while running", func(t *testing.T) {
		p := started()
		p.Spec.Events = append(p.Spec.Events, v1alpha1.PoolBenchEvent{Type: v1alpha1.PoolBenchEventSnapshot, AtVMs: 45})
		if _, err := (&PoolBenchValidator{}).ValidateUpdate(context.Background(), started(), p); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("an event may not be removed once started", func(t *testing.T) {
		p := started()
		p.Spec.Events = nil
		if _, err := (&PoolBenchValidator{}).ValidateUpdate(context.Background(), started(), p); err == nil {
			t.Fatal("want an error, got none")
		}
	})
	t.Run("settings are fixed once started", func(t *testing.T) {
		p := started()
		p.Spec.IOPS = 20000
		_, err := (&PoolBenchValidator{}).ValidateUpdate(context.Background(), started(), p)
		if err == nil || !strings.Contains(err.Error(), "started") {
			t.Fatalf("want an error about a started run, got %v", err)
		}
	})
	t.Run("settings may change before the start", func(t *testing.T) {
		p := validPoolBench()
		p.Spec.IOPS = 20000
		if _, err := (&PoolBenchValidator{}).ValidateUpdate(context.Background(), validPoolBench(), p); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("a bad update is refused", func(t *testing.T) {
		p := validPoolBench()
		p.Spec.StepVMs = 0
		if _, err := (&PoolBenchValidator{}).ValidateUpdate(context.Background(), validPoolBench(), p); err == nil {
			t.Fatal("want an error, got none")
		}
	})
}
