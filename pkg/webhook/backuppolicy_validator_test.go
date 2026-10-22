package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validBackupPolicy() *v1alpha1.BackupPolicy {
	return &v1alpha1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly"},
		Spec: v1alpha1.BackupPolicySpec{
			SnapshotPool: "backups",
			Schedule:     "6h",
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"backup": "yes"}},
			KeepFor:      "7d",
		},
	}
}

func TestBackupPolicyValidator_Valid(t *testing.T) {
	v := &BackupPolicyValidator{}
	if _, err := v.ValidateCreate(context.Background(), validBackupPolicy()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := v.ValidateUpdate(context.Background(), validBackupPolicy(), validBackupPolicy()); err != nil {
		t.Fatalf("unexpected error on update: %v", err)
	}
}

func TestBackupPolicyValidator_Refuses(t *testing.T) {
	zero := int32(0)
	neg := int32(-1)
	cases := []struct {
		name   string
		mutate func(*v1alpha1.BackupPolicy)
		want   string
	}{
		{"no pool", func(p *v1alpha1.BackupPolicy) { p.Spec.SnapshotPool = "" }, "snapshotPool"},
		{"no schedule", func(p *v1alpha1.BackupPolicy) { p.Spec.Schedule = "" }, "schedule"},
		{"bad schedule", func(p *v1alpha1.BackupPolicy) { p.Spec.Schedule = "often" }, "schedule"},
		{"too frequent", func(p *v1alpha1.BackupPolicy) { p.Spec.Schedule = "30s" }, "schedule"},
		{"no selector", func(p *v1alpha1.BackupPolicy) { p.Spec.Selector = nil }, "selector"},
		{"empty selector selects everything", func(p *v1alpha1.BackupPolicy) { p.Spec.Selector = &metav1.LabelSelector{} }, "selector"},
		{"bad selector", func(p *v1alpha1.BackupPolicy) {
			p.Spec.Selector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "a", Operator: "Nope"}}}
		}, "selector"},
		{"bad keep", func(p *v1alpha1.BackupPolicy) { p.Spec.KeepFor = "soon" }, "keepFor"},
		{"keep under an hour", func(p *v1alpha1.BackupPolicy) { p.Spec.KeepFor = "30m" }, "keepFor"},
		{"negative days", func(p *v1alpha1.BackupPolicy) { p.Spec.KeepFor = "-3d" }, "keepFor"},
		{"negative speed", func(p *v1alpha1.BackupPolicy) { p.Spec.VMCopySpeedLimitMBps = &neg }, "vmCopySpeedLimitMBps"},
		{"empty namespace name", func(p *v1alpha1.BackupPolicy) { p.Spec.Namespaces = []string{""} }, "namespaces"},
	}
	for _, tc := range cases {
		p := validBackupPolicy()
		tc.mutate(p)
		_, err := (&BackupPolicyValidator{}).ValidateCreate(context.Background(), p)
		if err == nil {
			t.Errorf("%s: want an error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q should name %q", tc.name, err, tc.want)
		}
	}
	// A speed of 0 means no limit and is allowed.
	p := validBackupPolicy()
	p.Spec.VMCopySpeedLimitMBps = &zero
	if _, err := (&BackupPolicyValidator{}).ValidateCreate(context.Background(), p); err != nil {
		t.Errorf("speed 0 (no limit): %v", err)
	}
}

func TestBackupPolicyValidator_Export(t *testing.T) {
	good := func() *v1alpha1.BackupExport {
		return &v1alpha1.BackupExport{Bucket: "b-ucket", Endpoint: "s3.example.com", CredentialsSecret: "cos", BackupKeySecret: "key",
			KeyProtect: v1alpha1.KeyProtectRef{InstanceID: "11111111-2222-3333-4444-555555555555", Region: "eu-de", RootKeyID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", APIKeySecret: "kp-apikey"}}
	}
	p := validBackupPolicy()
	p.Spec.Export = good()
	if _, err := (&BackupPolicyValidator{}).ValidateCreate(context.Background(), p); err != nil {
		t.Fatalf("a valid export block: %v", err)
	}
	cases := map[string]func(*v1alpha1.BackupExport){
		"no bucket":               func(e *v1alpha1.BackupExport) { e.Bucket = "" },
		"no endpoint":             func(e *v1alpha1.BackupExport) { e.Endpoint = "" },
		"no credentials":          func(e *v1alpha1.BackupExport) { e.CredentialsSecret = "" },
		"no key secret":           func(e *v1alpha1.BackupExport) { e.BackupKeySecret = "" },
		"no key protect instance": func(e *v1alpha1.BackupExport) { e.KeyProtect.InstanceID = "" },
		"no key protect region":   func(e *v1alpha1.BackupExport) { e.KeyProtect.Region = "" },
		"no root key":             func(e *v1alpha1.BackupExport) { e.KeyProtect.RootKeyID = "" },
		"no api key secret":       func(e *v1alpha1.BackupExport) { e.KeyProtect.APIKeySecret = "" },
		"bad lock":                func(e *v1alpha1.BackupExport) { e.Lock = "Governance" },
		"24h compliance":          func(e *v1alpha1.BackupExport) { e.KeepFor = "24h" },
	}
	for name, mutate := range cases {
		bad := validBackupPolicy()
		bad.Spec.Export = good()
		mutate(bad.Spec.Export)
		if _, err := (&BackupPolicyValidator{}).ValidateCreate(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "spec.export") {
			t.Errorf("%s: err = %v, want an error that names spec.export", name, err)
		}
		if _, err := (&BackupPolicyValidator{}).ValidateUpdate(context.Background(), validBackupPolicy(), bad); err == nil {
			t.Errorf("%s: an update must be refused too", name)
		}
	}
	p = validBackupPolicy()
	p.Spec.Export = good()
	p.Spec.Export.KeepFor = "25h"
	if _, err := (&BackupPolicyValidator{}).ValidateCreate(context.Background(), p); err != nil {
		t.Errorf("25h with Compliance: %v", err)
	}
	p.Spec.Export.KeepFor = "3650d"
	if _, err := (&BackupPolicyValidator{}).ValidateCreate(context.Background(), p); err != nil {
		t.Errorf("a long keep time needs no check in the webhook: %v", err)
	}
}

func TestBackupPolicyValidator_NameMustFitALabelValue(t *testing.T) {
	p := validBackupPolicy()
	p.Name = strings.Repeat("a", 64)
	if _, err := (&BackupPolicyValidator{}).ValidateCreate(context.Background(), p); err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("err = %v: the name goes into a label value and may have 63 characters at most", err)
	}
	p.Name = strings.Repeat("a", 63)
	if _, err := (&BackupPolicyValidator{}).ValidateCreate(context.Background(), p); err != nil {
		t.Errorf("a 63-character name: %v", err)
	}
}

func TestBackupPolicyKeepDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"":      7 * 24 * time.Hour, // the default
		"7d":    7 * 24 * time.Hour,
		"30d":   30 * 24 * time.Hour,
		"36h":   36 * time.Hour,
		"90m":   90 * time.Minute,
		"1d12h": 0, // not accepted: use "36h"
	}
	for in, want := range cases {
		got, err := v1alpha1.ParseKeepFor(in)
		if want == 0 {
			if err == nil {
				t.Errorf("ParseKeepFor(%q) = %v, want an error", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("ParseKeepFor(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}
