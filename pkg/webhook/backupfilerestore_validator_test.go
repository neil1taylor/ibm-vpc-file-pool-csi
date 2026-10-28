package webhook

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

func validFileRestore() *v1alpha1.BackupFileRestore {
	return &v1alpha1.BackupFileRestore{Spec: v1alpha1.BackupFileRestoreSpec{
		Policy: "nightly", SourceNamespace: "web", SourceName: "data",
		Paths:           []string{"/var/www/html/index.php", "/etc/app\\x0a.conf"},
		TargetNamespace: "web", TargetName: "data-restored",
	}}
}

func TestBackupFileRestoreValidatorCreate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*v1alpha1.BackupFileRestoreSpec)
		wantErr string
	}{
		{"valid new claim", func(*v1alpha1.BackupFileRestoreSpec) {}, ""},
		{"valid new claim with size", func(s *v1alpha1.BackupFileRestoreSpec) { s.SizeGB = 10 }, ""},
		{"valid existing claim", func(s *v1alpha1.BackupFileRestoreSpec) { s.Existing = true; s.TargetUID = "abc" }, ""},
		{"whole volume", func(s *v1alpha1.BackupFileRestoreSpec) { s.Paths = []string{"/"} }, ""},
		{"no policy", func(s *v1alpha1.BackupFileRestoreSpec) { s.Policy = "" }, "spec.policy is required"},
		{"no source", func(s *v1alpha1.BackupFileRestoreSpec) { s.SourceName = "" }, "spec.sourceName is required"},
		{"no target", func(s *v1alpha1.BackupFileRestoreSpec) { s.TargetName = "" }, "spec.targetName is required"},
		{"bad target namespace", func(s *v1alpha1.BackupFileRestoreSpec) { s.TargetNamespace = "Bad_NS" }, "spec.targetNamespace"},
		{"bad target name", func(s *v1alpha1.BackupFileRestoreSpec) { s.TargetName = "Bad Name" }, "spec.targetName"},
		{"bad source namespace", func(s *v1alpha1.BackupFileRestoreSpec) { s.SourceNamespace = "a/b" }, "spec.sourceNamespace"},
		{"no paths", func(s *v1alpha1.BackupFileRestoreSpec) { s.Paths = nil }, "spec.paths"},
		{"too many paths", func(s *v1alpha1.BackupFileRestoreSpec) {
			s.Paths = make([]string, 1001)
			for i := range s.Paths {
				s.Paths[i] = "/a"
			}
		}, "at most 1000"},
		{"path with dot dot", func(s *v1alpha1.BackupFileRestoreSpec) { s.Paths = []string{"/a/../b"} }, "spec.paths[0]"},
		{"path with bad escape", func(s *v1alpha1.BackupFileRestoreSpec) { s.Paths = []string{"/ok", "/a\\q"} }, "spec.paths[1]"},
		{"path with a nul byte", func(s *v1alpha1.BackupFileRestoreSpec) { s.Paths = []string{"/a\\x00b"} }, "spec.paths[0]"},
		{"path too long", func(s *v1alpha1.BackupFileRestoreSpec) { s.Paths = []string{"/" + strings.Repeat("a", 5000)} }, "spec.paths[0]"},
		{"paths too big in total", func(s *v1alpha1.BackupFileRestoreSpec) {
			s.Paths = nil
			for i := 0; i < 100; i++ {
				s.Paths = append(s.Paths, "/"+strings.Repeat(strings.Repeat("a", 200)+"/", 14))
			}
		}, "together"},
		{"size with existing", func(s *v1alpha1.BackupFileRestoreSpec) { s.Existing = true; s.SizeGB = 5 }, "spec.sizeGB"},
		{"class with existing", func(s *v1alpha1.BackupFileRestoreSpec) { s.Existing = true; s.StorageClassName = "x" }, "spec.storageClassName"},
		{"uid with a new claim", func(s *v1alpha1.BackupFileRestoreSpec) { s.TargetUID = "abc" }, "spec.targetUID"},
		{"negative size", func(s *v1alpha1.BackupFileRestoreSpec) { s.SizeGB = -1 }, "spec.sizeGB"},
		{"huge size", func(s *v1alpha1.BackupFileRestoreSpec) { s.SizeGB = 40000 }, "spec.sizeGB"},
	}
	v := &BackupFileRestoreValidator{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			br := validFileRestore()
			c.mutate(&br.Spec)
			_, err := v.ValidateCreate(context.Background(), br)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("error %v, want one holding %q", err, c.wantErr)
			}
		})
	}
}

func TestBackupFileRestoreValidatorRefusesAChangeToTheSpec(t *testing.T) {
	v := &BackupFileRestoreValidator{}
	old := validFileRestore()
	changed := validFileRestore()
	changed.Spec.Paths = []string{"/other"}
	if _, err := v.ValidateUpdate(context.Background(), old, changed); err == nil {
		t.Fatal("a change to the spec was allowed")
	}
	same := validFileRestore()
	same.Status.Phase = "Done"
	if _, err := v.ValidateUpdate(context.Background(), old, same); err != nil {
		t.Fatalf("a change to the status was refused: %v", err)
	}
}
