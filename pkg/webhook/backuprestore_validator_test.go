package webhook

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validBackupRestore() *v1alpha1.BackupRestore {
	return &v1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-1"},
		Spec: v1alpha1.BackupRestoreSpec{
			Policy: "nightly", SourceNamespace: "apps", SourceName: "db",
			TargetNamespace: "apps", TargetName: "db-restored",
		},
	}
}

func TestBackupRestoreValidator_Valid(t *testing.T) {
	if _, err := (&BackupRestoreValidator{}).ValidateCreate(context.Background(), validBackupRestore()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBackupRestoreValidator_Refuses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha1.BackupRestore)
		want   string
	}{
		{"no policy", func(r *v1alpha1.BackupRestore) { r.Spec.Policy = "" }, "policy"},
		{"no source namespace", func(r *v1alpha1.BackupRestore) { r.Spec.SourceNamespace = "" }, "sourceNamespace"},
		{"no source name", func(r *v1alpha1.BackupRestore) { r.Spec.SourceName = "" }, "sourceName"},
		{"no target namespace", func(r *v1alpha1.BackupRestore) { r.Spec.TargetNamespace = "" }, "targetNamespace"},
		{"no target name", func(r *v1alpha1.BackupRestore) { r.Spec.TargetName = "" }, "targetName"},
		{"target name that is not a name", func(r *v1alpha1.BackupRestore) { r.Spec.TargetName = "Bad Name" }, "targetName"},
		{"target namespace that is not a name", func(r *v1alpha1.BackupRestore) { r.Spec.TargetNamespace = "a/b" }, "targetNamespace"},
	}
	for _, tc := range cases {
		r := validBackupRestore()
		tc.mutate(r)
		_, err := (&BackupRestoreValidator{}).ValidateCreate(context.Background(), r)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one that names %q", tc.name, err, tc.want)
		}
	}
}

// A restore asks for one thing. Changing it later would act on a PVC that the
// first request made.
func TestBackupRestoreValidator_SpecCannotChange(t *testing.T) {
	old, changed := validBackupRestore(), validBackupRestore()
	changed.Spec.TargetName = "other"
	v := &BackupRestoreValidator{}
	if _, err := v.ValidateUpdate(context.Background(), old, changed); err == nil {
		t.Error("want an error when the spec changes")
	}
	same := validBackupRestore()
	same.Status.Phase = "Done"
	if _, err := v.ValidateUpdate(context.Background(), old, same); err != nil {
		t.Errorf("a status change is not a spec change: %v", err)
	}
}
