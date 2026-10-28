package pool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func cosBrowseRequest() BrowseJobRequest {
	return BrowseJobRequest{
		ID: "bb-9", SecretName: "browse-bb-9", Idle: 15 * time.Minute, MaxLife: 2 * time.Hour,
		Owner: metav1.OwnerReference{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "BackupBrowse", Name: "bb-9", UID: "uid-9"},
		COS:   &BrowseCOSRequest{Repo: testExportRepo(), Policy: "policy-nightly", Volume: testExportVolume},
	}
}

// The paths of the mounted Secrets are fixed in two packages: the pod mounts
// them where the helper reads them. They must not drift apart.
func TestBrowseCOS_MountPathsAreWhereTheHelperReads(t *testing.T) {
	for what, pair := range map[string][2]string{
		"wrapped key":  {wrappedKeyDir, cosbrowse.WrappedKeyDir},
		"API key":      {apiKeyDir, cosbrowse.APIKeyDir},
		"emergency":    {emergencyKeyDir, cosbrowse.EmergencyKeyDir},
		"wrapped file": {wrappedKeyDir + "/" + ExportWrappedKeyKey, cosbrowse.WrappedKeyFile},
		"API file":     {apiKeyDir + "/" + ExportAPIKeyKey, cosbrowse.APIKeyFile},
		"emergency f":  {emergencyKeyDir + "/" + ExportEmergencyKeyKey, cosbrowse.EmergencyKeyFile},
		"access key":   {cosbrowse.CredentialsDir + "/" + ExportAccessKeyKey, cosbrowse.AccessKeyFile},
		"secret key":   {cosbrowse.CredentialsDir + "/" + ExportSecretKeyKey, cosbrowse.SecretKeyFile},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: the pod mounts %s, the helper reads %s", what, pair[0], pair[1])
		}
	}
}

func TestStartBrowseCOS_ShapeAndHardening(t *testing.T) {
	r, c := newBrowseRunner()
	if err := r.StartBrowse(context.Background(), cosBrowseRequest()); err != nil {
		t.Fatal(err)
	}
	job := browseJob(t, c, "bb-9")
	pod := job.Spec.Template.Spec
	ctr := pod.Containers[0]
	if ctr.Image != testBrowseImage {
		t.Errorf("image = %q", ctr.Image)
	}
	args := strings.Join(ctr.Command, " ")
	for _, want := range []string{"/usr/local/bin/browse", "--mode=browse-server", "--cos-source=", "--kopia=/usr/local/bin/kopia",
		"--kopia-state-dir=/tmp/kopia", "--listen=:8443", "--token-file=/etc/browse/token", "--idle=15m0s", "--max-life=2h0m0s"} {
		if !strings.Contains(args, want) {
			t.Errorf("command lacks %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--root") || strings.Contains(args, "--moments") {
		t.Errorf("a COS session must not name a folder: %s", args)
	}
	var src cosbrowse.Source
	for _, a := range ctr.Command {
		if v, ok := strings.CutPrefix(a, "--cos-source="); ok {
			if err := json.Unmarshal([]byte(v), &src); err != nil {
				t.Fatal(err)
			}
		}
	}
	if src.Bucket != "pool-backups" || src.Prefix != "policy-nightly/" || src.Policy != "policy-nightly" || src.Volume != testExportVolume ||
		src.KeyProtect == nil || src.KeyProtect.Region != "eu-de" || src.EmergencyKey || src.EmergencyOnly {
		t.Errorf("source = %+v", src)
	}
	// No secret value, and no Secret name, is in the command.
	for _, secret := range []string{"cos-credentials", "cos-backup-key", "kp-apikey"} {
		if strings.Contains(args, secret) {
			t.Errorf("the name of Secret %s is in the command", secret)
		}
	}
	if len(ctr.Env) != 0 {
		t.Errorf("the helper has environment variables: %v", ctr.Env)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("a pod that holds the backup key must not get a service account token")
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC {
		t.Error("the pod must not use the host namespaces")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser == 0 {
		t.Errorf("the COS helper must not run as root: %+v", pod.SecurityContext)
	}
	sc := ctr.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 {
		t.Errorf("container security context = %+v", sc)
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("a session must not be restarted")
	}
	if lim := ctr.Resources.Limits; lim.Cpu().Value() != 1 || lim.Memory().Value() != 1<<30 {
		t.Errorf("limits = %v", lim)
	}

	mounts := map[string]corev1.VolumeMount{}
	for _, m := range ctr.VolumeMounts {
		mounts[m.MountPath] = m
	}
	for _, path := range []string{cosbrowse.CredentialsDir, cosbrowse.WrappedKeyDir, cosbrowse.APIKeyDir, "/etc/browse"} {
		if m, ok := mounts[path]; !ok || !m.ReadOnly {
			t.Errorf("%s must be mounted read-only: %+v", path, m)
		}
	}
	if _, ok := mounts[cosbrowse.EmergencyKeyDir]; ok {
		t.Error("the emergency key is mounted though the policy has none")
	}
	if m := mounts[cosbrowse.DefaultStateDir]; m.Name != cosStateVolume || m.ReadOnly {
		t.Errorf("Kopia's folder = %+v", m)
	}
	for _, v := range pod.Volumes {
		switch {
		case v.HostPath != nil || v.PersistentVolumeClaim != nil:
			t.Errorf("volume %s: a COS helper mounts no share and no host path", v.Name)
		case v.Name == cosStateVolume:
			if v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil || v.EmptyDir.Medium == corev1.StorageMediumMemory {
				t.Errorf("Kopia's folder = %+v", v.VolumeSource)
			}
		case v.Name == memoryKeyVolume:
			t.Error("the helper unwraps the key in its own memory and needs no key folder")
		}
	}
	// The COS key is a file with exactly its two keys.
	for _, v := range pod.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "cos-credentials" {
			if len(v.Secret.Items) != 2 {
				t.Errorf("credentials items = %+v", v.Secret.Items)
			}
		}
	}
	if job.Labels[browseLabel] != "bb-9" {
		t.Errorf("labels = %v", job.Labels)
	}
	pvs := &corev1.PersistentVolumeList{}
	_ = c.List(context.Background(), pvs)
	if len(pvs.Items) != 0 {
		t.Errorf("share PVs were made: %d", len(pvs.Items))
	}
}

func TestStartBrowseCOS_EmergencyKeys(t *testing.T) {
	r, c := newBrowseRunner()
	req := cosBrowseRequest()
	req.COS.Repo.EmergencyKeySecret = "cos-emergency"
	req.At = time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	if err := r.StartBrowse(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	ctr := browseJob(t, c, "bb-9").Spec.Template.Spec.Containers[0]
	args := strings.Join(ctr.Command, " ")
	if !strings.Contains(args, `"emergencyKey":true`) || !strings.Contains(args, "--at=2026-10-20T08:00:00Z") {
		t.Errorf("command = %s", args)
	}
	found := false
	for _, m := range ctr.VolumeMounts {
		if m.MountPath == cosbrowse.EmergencyKeyDir && m.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Error("the emergency key is not mounted")
	}

	r2, c2 := newBrowseRunner()
	only := cosBrowseRequest()
	only.ID, only.SecretName = "bb-10", "browse-bb-10"
	only.COS.Repo = ExportRepo{Endpoint: only.COS.Repo.Endpoint, Bucket: only.COS.Repo.Bucket, Prefix: only.COS.Repo.Prefix,
		CredentialsSecret: "cos-credentials", EmergencyKeySecret: "cos-emergency", EmergencyOnly: true}
	if err := r2.StartBrowse(context.Background(), only); err != nil {
		t.Fatal(err)
	}
	ctr = browseJob(t, c2, "bb-10").Spec.Template.Spec.Containers[0]
	args = strings.Join(ctr.Command, " ")
	if !strings.Contains(args, `"emergencyOnly":true`) || strings.Contains(args, "keyProtect") {
		t.Errorf("command = %s", args)
	}
	for _, m := range ctr.VolumeMounts {
		if m.MountPath == cosbrowse.WrappedKeyDir || m.MountPath == cosbrowse.APIKeyDir {
			t.Errorf("an emergency-only session mounts %s", m.MountPath)
		}
	}
}

func TestStartBrowseCOS_RefusesWhatItCannotCheck(t *testing.T) {
	cases := map[string]func(*BrowseJobRequest){
		"a bad endpoint":      func(r *BrowseJobRequest) { r.COS.Repo.Endpoint = "s3.example; ls" },
		"a traversing prefix": func(r *BrowseJobRequest) { r.COS.Repo.Prefix = "a/../b/" },
		"a bad volume":        func(r *BrowseJobRequest) { r.COS.Volume = "pvc-1" },
		"a bad policy":        func(r *BrowseJobRequest) { r.COS.Policy = "../x" },
		"a bad Secret name":   func(r *BrowseJobRequest) { r.COS.Repo.CredentialsSecret = "Bad Name" },
		"no limits":           func(r *BrowseJobRequest) { r.Idle = 0 },
		"no session Secret":   func(r *BrowseJobRequest) { r.SecretName = "" },
	}
	for name, mutate := range cases {
		r, c := newBrowseRunner()
		req := cosBrowseRequest()
		mutate(&req)
		if err := r.StartBrowse(context.Background(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
		jobs := batchJobs(t, c)
		if jobs != 0 {
			t.Errorf("%s: %d Jobs were made", name, jobs)
		}
	}
}

func TestStartBrowseCOS_NeedsTheBrowseImage(t *testing.T) {
	r, _ := newShareJobTestRunner()
	if err := r.StartBrowse(context.Background(), cosBrowseRequest()); err == nil {
		t.Fatal("a runner with no browse image started a helper")
	}
}

func TestStartBrowseCOS_StartsOnceAndHonoursTheLimits(t *testing.T) {
	r, c := newBrowseRunner()
	if err := r.SetBrowseCOSResources("2", "3Gi"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBrowseCOSResources("lots", ""); err == nil {
		t.Error("a bad quantity was accepted")
	}
	for i := 0; i < 2; i++ {
		if err := r.StartBrowse(context.Background(), cosBrowseRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if n := batchJobs(t, c); n != 1 {
		t.Errorf("%d Jobs", n)
	}
	lim := browseJob(t, c, "bb-9").Spec.Template.Spec.Containers[0].Resources.Limits
	if lim.Cpu().Value() != 2 || lim.Memory().Value() != 3<<30 {
		t.Errorf("limits = %v", lim)
	}
}

func batchJobs(t *testing.T, c client.Client) int {
	t.Helper()
	jobs := &batchv1.JobList{}
	if err := c.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	return len(jobs.Items)
}
