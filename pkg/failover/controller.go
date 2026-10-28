package failover

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// failoverJobLabel marks the Job and pod of one Failover. The value is the
	// Job's name.
	failoverJobLabel = "storage.ibmcloud.io/failover-job"

	// DefaultControllerInterval is how often the controller looks at Failovers.
	DefaultControllerInterval = 5 * time.Second

	// qemuUser is the user the receiver and KubeVirt's QEMU run as. It owns the
	// replicated disks, and root cannot read them over NFS (root_squash).
	qemuUser = 107

	failoverJobNameMax = 52
	mountDir           = "/data"
	driverBinary       = "/usr/local/bin/vpc-file-pool-csi"

	planDeadline    = int64(30 * 60)
	executeDeadline = int64(6 * 60 * 60)
)

// Controller runs each Failover as a Job in the driver's namespace and copies
// the Job's result into the Failover's status. It must run on the leader only.
type Controller struct {
	client        client.Client
	namespace     string
	driverImage   string
	defaultSource string
	interval      time.Duration
	now           func() time.Time
}

// NewController makes a controller whose Jobs run in namespace with
// driverImage. c should read without a cache: the controller may only get and
// list pods, and a stale read could start a second Job. defaultSource is the
// claim to read when a Failover names none.
func NewController(c client.Client, namespace, driverImage, defaultSource string) *Controller {
	return &Controller{
		client:        c,
		namespace:     namespace,
		driverImage:   driverImage,
		defaultSource: defaultSource,
		interval:      DefaultControllerInterval,
		now:           time.Now,
	}
}

// Run looks at the Failovers until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	klog.V(2).InfoS("Failover controller started", "interval", c.interval)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			klog.V(2).InfoS("Failover controller stopped")
			return
		case <-ticker.C:
			if err := c.processOnce(ctx); err != nil {
				klog.ErrorS(err, "Failover controller pass failed")
			}
		}
	}
}

func (c *Controller) processOnce(ctx context.Context) error {
	list := &v1alpha1.FailoverList{}
	if err := c.client.List(ctx, list); err != nil {
		return fmt.Errorf("list Failovers: %w", err)
	}
	var firstErr error
	for i := range list.Items {
		f := &list.Items[i]
		var err error
		switch f.Status.Phase {
		case "":
			err = c.begin(ctx, f, list.Items)
		case v1alpha1.FailoverPhasePlanning, v1alpha1.FailoverPhaseExecuting:
			err = c.check(ctx, f, list.Items)
		}
		if err != nil {
			klog.ErrorS(err, "Failed to reconcile Failover", "failover", f.Name)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// failoverJobName makes a Job name that fits Kubernetes limits.
func failoverJobName(name string) string {
	jobName := "failover-" + name
	if len(jobName) <= failoverJobNameMax {
		return jobName
	}
	sum := sha256.Sum256([]byte(name))
	return jobName[:failoverJobNameMax-9] + "-" + hex.EncodeToString(sum[:])[:8]
}

// fail ends a Failover with a reason.
func (c *Controller) fail(ctx context.Context, f *v1alpha1.Failover, reason string) error {
	now := metav1.NewTime(c.now())
	f.Status.Phase = v1alpha1.FailoverPhaseFailed
	f.Status.Message = reason
	f.Status.FinishedAt = &now
	if err := c.client.Status().Update(ctx, f); err != nil {
		return fmt.Errorf("update Failover %s: %w", f.Name, err)
	}
	c.cleanup(ctx, failoverJobName(f.Name))
	return nil
}

// shareAddress says where the destination claim lives, for the PV of each
// failed-over volume. Fields set in the spec win; the rest come from the NFS
// volume behind the claim.
func (c *Controller) shareAddress(ctx context.Context, f *v1alpha1.Failover, source string) (ip, export string, err error) {
	ip, export = f.Spec.DRShareIP, f.Spec.DRExportPath
	if ip != "" {
		return ip, export, nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err = c.client.Get(ctx, types.NamespacedName{Namespace: c.namespace, Name: source}, pvc); err != nil {
		return "", "", fmt.Errorf("get claim %s: %w", source, err)
	}
	pv := &corev1.PersistentVolume{}
	if err = c.client.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return "", "", fmt.Errorf("get the volume of claim %s: %w", source, err)
	}
	if pv.Spec.NFS == nil {
		return "", "", fmt.Errorf("the volume of claim %s is not an NFS volume; set spec.drShareIP and spec.drExportPath", source)
	}
	if export == "" {
		export = strings.TrimRight(pv.Spec.NFS.Path, "/")
	}
	return pv.Spec.NFS.Server, export, nil
}

// begin checks a new Failover and starts its Job.
func (c *Controller) begin(ctx context.Context, f *v1alpha1.Failover, all []v1alpha1.Failover) error {
	source := EffectiveSource(f, c.defaultSource)
	if source == "" {
		return c.fail(ctx, f, "spec.source is required: no default source is set")
	}
	if c.driverImage == "" {
		return c.fail(ctx, f, "the controller has no driver image for the failover Job")
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := c.client.Get(ctx, types.NamespacedName{Namespace: c.namespace, Name: source}, pvc)
	if apierrors.IsNotFound(err) {
		return c.fail(ctx, f, fmt.Sprintf("claim %q not found in namespace %s", source, c.namespace))
	}
	if err != nil {
		return fmt.Errorf("get claim %s: %w", source, err)
	}

	args := []string{
		"--mode=failover",
		"--nfs-mount-path=" + mountDir,
		"--failover-action=" + f.Spec.Action,
	}
	if f.Spec.RewindTo != nil {
		args = append(args, "--rewind-to="+f.Spec.RewindTo.UTC().Format(time.RFC3339))
	}
	if vms := NormalizeVMs(f.Spec.VMs); vms != nil {
		args = append(args, "--vms="+strings.Join(vms, ","))
	}
	if f.Spec.Force {
		args = append(args, "--force")
	}

	phase := v1alpha1.FailoverPhasePlanning
	if f.Spec.Action == v1alpha1.FailoverActionExecute {
		phase = v1alpha1.FailoverPhaseExecuting
		if f.Spec.DRPool == "" {
			return c.fail(ctx, f, "spec.drPool is required for Execute")
		}
		plan, perr := FindPlan(all, f, c.defaultSource, c.now())
		if perr != nil {
			return c.fail(ctx, f, perr.Error())
		}
		if ferr := AlreadyFailedOver(ctx, c.client, plan); ferr != nil {
			return c.fail(ctx, f, ferr.Error())
		}
		if _, _, aerr := c.shareAddress(ctx, f, source); aerr != nil {
			return c.fail(ctx, f, aerr.Error())
		}
		expect, merr := json.Marshal(ExpectationFromPlan(plan))
		if merr != nil {
			return merr
		}
		args = append(args, "--failover-expect="+string(expect))
	}

	if err = c.createJob(ctx, failoverJobName(f.Name), source, f.Spec.Action, args); err != nil {
		return err
	}
	now := metav1.NewTime(c.now())
	f.Status.Phase = phase
	f.Status.Source = source
	f.Status.Message = ""
	if f.Status.StartedAt == nil {
		f.Status.StartedAt = &now
	}
	if err = c.client.Status().Update(ctx, f); err != nil {
		return fmt.Errorf("update Failover %s: %w", f.Name, err)
	}
	return nil
}

func (c *Controller) createJob(ctx context.Context, jobName, claim, action string, args []string) error {
	labels := map[string]string{failoverJobLabel: jobName}
	backoff := int32(0)
	deadline := planDeadline
	if action == v1alpha1.FailoverActionExecute {
		deadline = executeDeadline
	}
	uid := int64(qemuUser)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: c.namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &uid},
					Containers: []corev1.Container{{
						Name:    "failover",
						Image:   c.driverImage,
						Command: []string{driverBinary},
						Args:    args,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "destination", MountPath: mountDir}},
					}},
					Volumes: []corev1.Volume{{
						Name: "destination",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
						},
					}},
				},
			},
		},
	}
	if err := c.client.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create failover Job %s: %w", jobName, err)
	}
	klog.V(2).InfoS("Started failover Job", "job", jobName, "namespace", c.namespace)
	return nil
}

// check looks at the Job of a Failover that is running.
func (c *Controller) check(ctx context.Context, f *v1alpha1.Failover, all []v1alpha1.Failover) error {
	jobName := failoverJobName(f.Name)
	job := &batchv1.Job{}
	err := c.client.Get(ctx, types.NamespacedName{Namespace: c.namespace, Name: jobName}, job)
	if apierrors.IsNotFound(err) {
		// Someone removed the Job. Start it again.
		return c.begin(ctx, f, all)
	}
	if err != nil {
		return fmt.Errorf("get failover Job %s: %w", jobName, err)
	}
	switch {
	case job.Status.Succeeded > 0:
		return c.finish(ctx, f, jobName)
	case job.Status.Failed > 0 || jobFailed(job):
		out, derr := DecodeOutcome(c.terminationMessage(ctx, jobName))
		reason := out.Error
		if derr != nil || reason == "" {
			reason = "the failover Job failed without a reason; see the pod logs in namespace " + c.namespace
		}
		return c.fail(ctx, f, reason)
	default:
		return nil
	}
}

func jobFailed(job *batchv1.Job) bool {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// finish copies the result of a Job that succeeded into the status. For an
// Execute it also makes the SubVolumes, PVs and PVCs.
func (c *Controller) finish(ctx context.Context, f *v1alpha1.Failover, jobName string) error {
	out, err := DecodeOutcome(c.terminationMessage(ctx, jobName))
	if err != nil {
		return c.fail(ctx, f, "could not read the result of the failover Job: "+err.Error())
	}
	if out.Error != "" {
		return c.fail(ctx, f, out.Error)
	}
	if out.Result == nil {
		return c.fail(ctx, f, "the failover Job left no result")
	}
	res := out.Result
	f.Status.VMs = res.VMs
	f.Status.Volumes = res.Volumes
	f.Status.MomentsTruncated = res.MomentsTruncated
	f.Status.RPO = &metav1.Duration{Duration: time.Duration(res.RPOSeconds) * time.Second}
	f.Status.Message = ""

	if f.Spec.Action == v1alpha1.FailoverActionExecute {
		created, xerr := c.createObjects(ctx, f, res)
		if xerr != nil {
			return c.fail(ctx, f, xerr.Error())
		}
		f.Status.CreatedPVCs = created
		f.Status.Phase = v1alpha1.FailoverPhaseDone
	} else {
		f.Status.Phase = v1alpha1.FailoverPhasePlanned
	}
	now := metav1.NewTime(c.now())
	f.Status.FinishedAt = &now
	if err = c.client.Status().Update(ctx, f); err != nil {
		return fmt.Errorf("update Failover %s: %w", f.Name, err)
	}
	c.cleanup(ctx, jobName)
	return nil
}

func (c *Controller) createObjects(ctx context.Context, f *v1alpha1.Failover, res *Result) ([]v1alpha1.FailoverPVC, error) {
	ip, export, err := c.shareAddress(ctx, f, f.Status.Source)
	if err != nil {
		return nil, err
	}
	plan := PlanFromVolumes(res.Volumes)
	opts := ExecuteOptions{
		DRPoolName:        f.Spec.DRPool,
		DRShareIP:         ip,
		DRExportPath:      export,
		NamespaceOverride: f.Spec.Namespace,
	}
	if err = NewExecutor(c.client, io.Discard).Execute(ctx, plan, opts); err != nil {
		return nil, err
	}
	var created []v1alpha1.FailoverPVC
	for _, sv := range plan.SubVolumes {
		ns := sv.PVCNamespace
		if f.Spec.Namespace != "" {
			ns = f.Spec.Namespace
		}
		created = append(created, v1alpha1.FailoverPVC{Namespace: ns, Name: sv.PVCName, PV: "dr-" + sv.SubVolumeName, SubVolume: sv.SubVolumeName})
	}
	return created, nil
}

// terminationMessage returns the message the Job's pod left, or "".
func (c *Controller) terminationMessage(ctx context.Context, jobName string) string {
	pods := &corev1.PodList{}
	if err := c.client.List(ctx, pods, client.InNamespace(c.namespace), client.MatchingLabels{failoverJobLabel: jobName}); err != nil {
		klog.V(4).InfoS("Could not list the pods of a failover Job", "job", jobName, "err", err)
		return ""
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				return cs.State.Terminated.Message
			}
		}
	}
	return ""
}

// cleanup removes the Job and its pods. A failure is logged; a Failover that
// has ended is not looked at again, so a leftover Job is harmless and can be
// removed by hand.
func (c *Controller) cleanup(ctx context.Context, jobName string) {
	propagation := metav1.DeletePropagationBackground
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: c.namespace}}
	if err := c.client.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete failover Job", "job", jobName)
	}
}

// AlreadyFailedOver refuses a plan if a volume or claim in it was failed over
// on this cluster before. Failing over twice would leave two copies of a VM's
// disks, or write over a claim that is in use.
func AlreadyFailedOver(ctx context.Context, r client.Reader, plan *v1alpha1.Failover) error {
	existing := &v1alpha1.SubVolumeList{}
	if err := r.List(ctx, existing, client.HasLabels{FailoverSourceLabel}); err != nil {
		return fmt.Errorf("list failed-over SubVolumes: %w", err)
	}
	done := map[string]bool{}
	for _, sv := range existing.Items {
		done[sv.Name] = true
	}
	vmOf := map[string]string{}
	for _, vm := range plan.Status.VMs {
		for _, d := range vm.Disks {
			vmOf[d.SubVolume] = vm.Name
		}
	}
	for _, v := range plan.Status.Volumes {
		if !done[v.SubVolume] {
			continue
		}
		if vm := vmOf[v.SubVolume]; vm != "" {
			return fmt.Errorf("VM %s was failed over on this cluster already (volume %s); remove its failed-over PVCs and SubVolumes first if you want to do it again", vm, v.SubVolume)
		}
		return fmt.Errorf("volume %s (claim %s/%s) was failed over on this cluster already; remove its failed-over PVC and SubVolume first if you want to do it again", v.SubVolume, v.PVCNamespace, v.PVCName)
	}
	return nil
}
