package pool

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const testBrowseImage = "de.icr.io/x/browse:v1"

func browseRequest() BrowseJobRequest {
	return BrowseJobRequest{
		Name: "bb-1", Share: testShareA, LatestPath: testBackupPath, MomentsPath: testMomentsPath,
		SecretName: "browse-bb-1", Idle: 15 * time.Minute, MaxLife: 2 * time.Hour,
		Owner: metav1.OwnerReference{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "BackupBrowse", Name: "bb-1", UID: "uid-1"},
	}
}

func newBrowseRunner() (*ShareJobRunner, client.Client) {
	r, c := newShareJobTestRunner()
	r.SetBrowseImage(testBrowseImage)
	return r, c
}

func browseJob(t *testing.T, c client.Client, name string) *batchv1.Job {
	t.Helper()
	return getShareJob(t, c, "browse-"+name)
}

func TestStartBrowse_ArgumentsAndHardening(t *testing.T) {
	r, c := newBrowseRunner()
	req := browseRequest()
	req.At = time.Date(2026, 10, 22, 10, 30, 0, 0, time.UTC)
	if err := r.StartBrowse(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	job := browseJob(t, c, "bb-1")
	pod := job.Spec.Template.Spec
	ctr := pod.Containers[0]
	if ctr.Image != testBrowseImage {
		t.Errorf("image = %q", ctr.Image)
	}
	args := strings.Join(ctr.Command, " ")
	for _, want := range []string{"/usr/local/bin/browse", "--mode=browse-server", "--root=/latest", "--moments=/moments",
		"--at=2026-10-22T10:30:00Z", "--listen=:8443", "--token-file=/etc/browse/token",
		"--tls-cert-file=/etc/browse/tls.crt", "--tls-key-file=/etc/browse/tls.key", "--idle=15m0s", "--max-life=2h0m0s"} {
		if !strings.Contains(args, want) {
			t.Errorf("command lacks %q: %s", want, args)
		}
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the pod must not get a service account token")
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC {
		t.Error("the pod must not use the host namespaces")
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("a session must not be restarted: %v %v", pod.RestartPolicy, job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= int64((2*time.Hour).Seconds()) {
		t.Errorf("deadline = %v, want a little over the hard limit", job.Spec.ActiveDeadlineSeconds)
	}
	sc := ctr.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 ||
		(sc.Privileged != nil && *sc.Privileged) {
		t.Errorf("container security context = %+v", sc)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != 0 {
		t.Errorf("pod security context = %+v", pod.SecurityContext)
	}
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			t.Errorf("host path volume %s", v.Name)
		}
	}
	shareMounts := 0
	for _, m := range ctr.VolumeMounts {
		switch m.MountPath {
		case "/latest", "/moments":
			shareMounts++
			if !m.ReadOnly || m.SubPath == "" {
				t.Errorf("share mount %+v must be read-only and below a sub path", m)
			}
		case "/etc/browse":
			if !m.ReadOnly {
				t.Error("the Secret must be mounted read-only")
			}
		default:
			t.Errorf("unexpected mount %+v", m)
		}
	}
	if shareMounts != 2 {
		t.Errorf("%d share mounts, want 2", shareMounts)
	}
	if ctr.ReadinessProbe == nil || ctr.ReadinessProbe.HTTPGet == nil || ctr.ReadinessProbe.HTTPGet.Path != "/healthz" ||
		ctr.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS || ctr.ReadinessProbe.HTTPGet.Port.IntValue() != 8443 {
		t.Errorf("readiness probe = %+v", ctr.ReadinessProbe)
	}
	if lim := ctr.Resources.Limits; lim.Cpu().MilliValue() != 200 || lim.Memory().Value() != 256<<20 {
		t.Errorf("limits = %v", lim)
	}
	if job.Labels[browseLabel] != "bb-1" || job.Spec.Template.Labels[browseLabel] != "bb-1" {
		t.Errorf("labels = %v / %v", job.Labels, job.Spec.Template.Labels)
	}
	pvs := &corev1.PersistentVolumeList{}
	_ = c.List(context.Background(), pvs)
	if len(pvs.Items) != 1 || !pvs.Items[0].Spec.NFS.ReadOnly {
		t.Errorf("share PV = %+v", pvs.Items)
	}
	for _, o := range append(slices.Clone(job.OwnerReferences), pvs.Items[0].OwnerReferences...) {
		if o.UID != "uid-1" {
			t.Errorf("owner = %+v", o)
		}
	}
	if len(job.OwnerReferences) != 1 || len(pvs.Items[0].OwnerReferences) != 1 {
		t.Error("the Job and the PV must be owned by the BackupBrowse")
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testDriverNS, Name: shareJobName("browse-bb-1") + "-s0"}, pvc); err != nil || len(pvc.OwnerReferences) != 1 {
		t.Errorf("share claim: %v %+v", err, pvc.OwnerReferences)
	}
}

func TestStartBrowse_LatestOnly(t *testing.T) {
	r, c := newBrowseRunner()
	req := browseRequest()
	req.MomentsPath = ""
	if err := r.StartBrowse(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(browseJob(t, c, "bb-1").Spec.Template.Spec.Containers[0].Command, " ")
	if strings.Contains(args, "--moments") || strings.Contains(args, "--at") {
		t.Errorf("no moments folder, yet: %s", args)
	}
}

func TestStartBrowse_RefusesUnsafeInput(t *testing.T) {
	cases := map[string]func(*BrowseJobRequest){
		"traversal":         func(q *BrowseJobRequest) { q.LatestPath = "/backup/nightly/pvcs/../../etc" },
		"a data volume":     func(q *BrowseJobRequest) { q.LatestPath = "/pvcs/pvc-11111111-2222-3333-4444-555555555555" },
		"moments as latest": func(q *BrowseJobRequest) { q.LatestPath = testMomentsPath },
		"latest as moments": func(q *BrowseJobRequest) { q.MomentsPath = testBackupPath },
		"other policy": func(q *BrowseJobRequest) {
			q.MomentsPath = "/backup/weekly/.moments/pvcs/pvc-11111111-2222-3333-4444-555555555555"
		},
		"no secret": func(q *BrowseJobRequest) { q.SecretName = "" },
		"no name":   func(q *BrowseJobRequest) { q.Name = "" },
		"no limits": func(q *BrowseJobRequest) { q.Idle, q.MaxLife = 0, 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, c := newBrowseRunner()
			req := browseRequest()
			mutate(&req)
			if err := r.StartBrowse(context.Background(), req); err == nil {
				t.Fatal("want an error")
			}
			if n := countPVs(t, c); n != 0 {
				t.Errorf("%d PV(s) made for a refused request", n)
			}
		})
	}
	r, _ := newShareJobTestRunner() // no browse image
	if err := r.StartBrowse(context.Background(), browseRequest()); err == nil {
		t.Error("without a browse image the request must be refused")
	}
}

func TestStartBrowse_IsIdempotent(t *testing.T) {
	r, c := newBrowseRunner()
	for i := 0; i < 2; i++ {
		if err := r.StartBrowse(context.Background(), browseRequest()); err != nil {
			t.Fatal(err)
		}
	}
	jobs := &batchv1.JobList{}
	_ = c.List(context.Background(), jobs)
	if len(jobs.Items) != 1 {
		t.Errorf("%d Jobs", len(jobs.Items))
	}
}

func addBrowsePod(t *testing.T, c client.Client, name, ip string, ready bool, termMsg string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p-" + name, Namespace: testDriverNS, Labels: map[string]string{shareJobLabel: shareJobName("browse-" + name), browseLabel: name}},
		Status:     corev1.PodStatus{PodIP: ip},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	if termMsg != "" {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: termMsg}}}}
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func TestBrowseState(t *testing.T) {
	r, c := newBrowseRunner()
	ctx := context.Background()
	if st, err := r.BrowseState(ctx, "bb-1"); err != nil || st.Exists {
		t.Fatalf("no Job: %+v %v", st, err)
	}
	_ = r.StartBrowse(ctx, browseRequest())
	if st, _ := r.BrowseState(ctx, "bb-1"); !st.Exists || st.Ready || st.Finished {
		t.Errorf("just started: %+v", st)
	}
	addBrowsePod(t, c, "bb-1", "10.130.0.5", false, "")
	if st, _ := r.BrowseState(ctx, "bb-1"); st.Ready || st.PodIP != "10.130.0.5" {
		t.Errorf("pod not ready: %+v", st)
	}
	pod := &corev1.Pod{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: testDriverNS, Name: "p-bb-1"}, pod)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	_ = c.Status().Update(ctx, pod)
	if st, _ := r.BrowseState(ctx, "bb-1"); !st.Ready || st.PodIP != "10.130.0.5" {
		t.Errorf("pod ready: %+v", st)
	}
	// the helper ends by itself
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "idle"}}}}
	_ = c.Status().Update(ctx, pod)
	setJobStatus(t, c, "browse-bb-1", 1, 0)
	if st, _ := r.BrowseState(ctx, "bb-1"); !st.Finished || !st.Succeeded || st.Reason != "idle" || st.Ready {
		t.Errorf("succeeded: %+v", st)
	}
}

func TestBrowseState_FailedJob(t *testing.T) {
	r, c := newBrowseRunner()
	_ = r.StartBrowse(context.Background(), browseRequest())
	setJobStatus(t, c, "browse-bb-1", 0, 1)
	job := browseJob(t, c, "bb-1")
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	_ = c.Status().Update(context.Background(), job)
	if st, _ := r.BrowseState(context.Background(), "bb-1"); !st.Finished || st.Succeeded {
		t.Errorf("failed: %+v", st)
	}
}

func TestStopBrowse_RemovesEverythingOnceTheJobIsGone(t *testing.T) {
	r, c := newBrowseRunner()
	ctx := context.Background()
	_ = r.StartBrowse(ctx, browseRequest())
	done, err := r.StopBrowse(ctx, "bb-1")
	if err != nil || done {
		t.Fatalf("first stop = %v, %v", done, err)
	}
	done, err = r.StopBrowse(ctx, "bb-1")
	if err != nil || !done {
		t.Fatalf("second stop = %v, %v", done, err)
	}
	if n := countPVs(t, c); n != 0 {
		t.Errorf("%d PV(s) left", n)
	}
}

func TestBrowseSessions(t *testing.T) {
	r, _ := newBrowseRunner()
	ctx := context.Background()
	_ = r.StartBrowse(ctx, browseRequest())
	other := browseRequest()
	other.Name = "bb-2"
	other.SecretName = "browse-bb-2"
	_ = r.StartBrowse(ctx, other)
	_, _ = r.RemoveDir(ctx, "rm-x", testShareA, testVolPath) // a share Job that is not a session
	got, err := r.BrowseSessions(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("sessions = %v, %v", got, err)
	}
}
