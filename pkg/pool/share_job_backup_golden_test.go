package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var updateBackupJobsGolden = flag.Bool("update-backup-jobs", false, "rewrite the backup Jobs golden file")

// backupJobsGoldenPath is read by the console plugin tests too (console-plugin/src/utils/backup-jobs.test.ts).
// Both sides build the same objects from the same input, so the Go and TypeScript builders cannot drift apart.
const backupJobsGoldenPath = "testdata/backup-jobs.golden.json"

const (
	goldenDriverImage = "icr.io/example/ibm-vpc-file-pool-csi:v1.0.0"
	goldenVolume      = "pvc-11111111-2222-3333-4444-555555555555"
	goldenPolicy      = "nightly"
)

// firstCall runs one step of a share data Job against a fresh fake cluster and returns the
// objects it made: the PV and claim (when the Job mounts a share) and the Job.
func firstCall(t *testing.T, run func(r *ShareJobRunner) error) map[string]any {
	t.Helper()
	r, c := newShareJobTestRunner()
	r.SetDriverImage(goldenDriverImage)
	if err := run(r); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out := map[string]any{}

	jobs := &batchv1.JobList{}
	if err := c.List(ctx, jobs); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("want one Job, got %d (err %v)", len(jobs.Items), err)
	}
	job := jobs.Items[0]
	out["job"] = cleanObject(t, &job, "batch/v1", "Job")

	pvs := &corev1.PersistentVolumeList{}
	if err := c.List(ctx, pvs); err != nil {
		t.Fatal(err)
	}
	if len(pvs.Items) > 0 {
		pv := pvs.Items[0]
		out["pv"] = cleanObject(t, &pv, "v1", "PersistentVolume")
		pvc := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: testDriverNS, Name: pv.Name}, pvc); err != nil {
			t.Fatal(err)
		}
		out["pvc"] = cleanObject(t, pvc, "v1", "PersistentVolumeClaim")
	}
	return out
}

// cleanObject drops what a client does not set (the status, the creation time, the version the
// fake cluster adds) and names the kind, as a client sends it.
func cleanObject(t *testing.T, obj client.Object, apiVersion, kind string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "status")
	m["apiVersion"], m["kind"] = apiVersion, kind
	if meta, ok := m["metadata"].(map[string]any); ok {
		delete(meta, "creationTimestamp")
		delete(meta, "resourceVersion")
	}
	if tmpl, ok := m["spec"].(map[string]any)["template"].(map[string]any); ok {
		if meta, ok := tmpl["metadata"].(map[string]any); ok {
			delete(meta, "creationTimestamp")
		}
	}
	return m
}

func TestBackupJobs_MatchGoldenFile(t *testing.T) {
	share := ShareMount{MountTargetIP: "10.240.0.1", ExportPath: "/share_a"}
	backup := "/backup/" + goldenPolicy + "/pvcs/" + goldenVolume
	moments := "/backup/" + goldenPolicy + "/.moments/pvcs/" + goldenVolume

	repo := ExportRepo{
		Endpoint: "s3.eu-de.cloud-object-storage.appdomain.cloud", Bucket: "backups-bucket",
		Prefix:            ExportAreaPrefix("", goldenPolicy),
		CredentialsSecret: "cos-credentials", BackupKeySecret: "cos-backup-key",
		KeyProtect: v1alpha1.KeyProtectRef{InstanceID: "11111111-2222-3333-4444-555555555555", Region: "eu-de", RootKeyID: "abcd-1234", APIKeySecret: "keyprotect-apikey"},
		LockMode:   ExportLockNone, MaintenanceInterval: v1alpha1.MinExportMaintenanceInterval,
	}
	repoEmergency := repo
	repoEmergency.Endpoint = "http://cos.example.com:9000"
	repoEmergency.Prefix = ExportAreaPrefix("team/backups", goldenPolicy)
	repoEmergency.EmergencyKeySecret = "cos-emergency-key"

	got, err := json.MarshalIndent(map[string]any{
		"input": map[string]any{
			"share":          map[string]any{"mountTargetIP": share.MountTargetIP, "exportPath": share.ExportPath},
			"namespace":      testDriverNS,
			"image":          DefaultCloneImage,
			"driverImage":    goldenDriverImage,
			"measureKey":     "bkmeasure-" + goldenVolume,
			"deleteKey":      "bkdelete-" + goldenVolume,
			"backupPath":     backup,
			"momentsPath":    moments,
			"cosListKey":     "coslist-abc",
			"cosVolume":      goldenVolume,
			"cosRepo":        exportRepoInput(repo),
			"cosRepoVariant": exportRepoInput(repoEmergency),
		},
		"measure": firstCall(t, func(r *ShareJobRunner) error {
			_, _, e := r.MeasureBackup(context.Background(), "bkmeasure-"+goldenVolume, share, backup, moments)
			return e
		}),
		"remove": firstCall(t, func(r *ShareJobRunner) error {
			_, e := r.RemoveBackup(context.Background(), "bkdelete-"+goldenVolume, share, backup, moments)
			return e
		}),
		"cosList": firstCall(t, func(r *ShareJobRunner) error {
			_, _, e := r.ListCOS(context.Background(), "coslist-abc", repo, goldenVolume)
			return e
		}),
		"cosListVariant": firstCall(t, func(r *ShareJobRunner) error {
			_, _, e := r.ListCOS(context.Background(), "coslist-abc", repoEmergency, "")
			return e
		}),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	if *updateBackupJobsGolden {
		if err = os.MkdirAll(filepath.Dir(backupJobsGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(backupJobsGoldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(backupJobsGoldenPath)
	if err != nil {
		t.Fatalf("read golden file (run: go test ./pkg/pool -run BackupJobs_MatchGolden -update-backup-jobs): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the backup Jobs changed. If that is on purpose, run: go test ./pkg/pool -run BackupJobs_MatchGolden -update-backup-jobs, and change the console builders (console-plugin/src/utils/backup-jobs.ts) to match.\n got: %s\nwant: %s", got, want)
	}
}

// exportRepoInput is the part of an ExportRepo a read-only Job depends on, as the console passes it.
func exportRepoInput(r ExportRepo) map[string]any {
	return map[string]any{
		"endpoint": r.Endpoint, "bucket": r.Bucket, "prefix": r.Prefix,
		"credentialsSecret": r.CredentialsSecret, "backupKeySecret": r.BackupKeySecret,
		"keyProtect": map[string]any{
			"instanceID": r.KeyProtect.InstanceID, "region": r.KeyProtect.Region,
			"rootKeyID": r.KeyProtect.RootKeyID, "apiKeySecret": r.KeyProtect.APIKeySecret,
		},
		"emergencyKeySecret": r.EmergencyKeySecret,
	}
}
