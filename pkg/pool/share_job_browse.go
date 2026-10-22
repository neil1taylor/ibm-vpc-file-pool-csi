package pool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// browseLabel is on every object of a browse session. Its value is the name of
// the BackupBrowse.
const browseLabel = "storage.ibmcloud.io/browse"

// BrowseJobRequest asks for the helper pod of one browse session.
type BrowseJobRequest struct {
	// Name is the name of the BackupBrowse.
	Name  string
	Share ShareMount
	// LatestPath and MomentsPath are the backup folder of the volume and its
	// earlier moments, on the share. MomentsPath may be empty.
	LatestPath  string
	MomentsPath string
	// At picks the moment to read; the zero time reads the latest copy.
	At time.Time
	// SecretName is the Secret in the driver namespace with the keys token,
	// tls.crt and tls.key.
	SecretName string
	Idle       time.Duration
	MaxLife    time.Duration
	// Owner is set on every object made, so deleting the BackupBrowse removes them.
	Owner metav1.OwnerReference
}

// BrowseJobState says what the helper pod of a session is doing.
type BrowseJobState struct {
	Exists    bool
	Ready     bool
	PodIP     string
	Finished  bool
	Succeeded bool
	// Reason is the end reason the helper wrote (idle, max-life, stopped).
	Reason string
}

// BrowseJobs runs the helper pods of browse sessions.
type BrowseJobs interface {
	StartBrowse(ctx context.Context, req BrowseJobRequest) error
	BrowseState(ctx context.Context, name string) (BrowseJobState, error)
	StopBrowse(ctx context.Context, name string) (bool, error)
	// BrowseSessions lists the names of the sessions that have a helper Job.
	BrowseSessions(ctx context.Context) ([]string, error)
	Namespace() string
}

// SetBrowseImage sets the image of the browse helper. Without it, sessions
// are refused.
func (r *ShareJobRunner) SetBrowseImage(image string) { r.browseImage = image }

// Namespace is where the helper pods run.
func (r *ShareJobRunner) Namespace() string { return r.namespace }

// browsePort is where the helper answers, over TLS.
const browsePort = 8443

func browseKey(name string) string { return "browse-" + name }

// StartBrowse starts the helper pod of a session, or does nothing if it exists.
// The pod mounts only the volume's backup folders, read-only, down to the NFS
// export. It has no service account token, runs as root with every capability
// dropped on a read-only root file system, and gets the session token and the
// session certificate from a Secret.
func (r *ShareJobRunner) StartBrowse(ctx context.Context, req BrowseJobRequest) error {
	if r.browseImage == "" {
		return errors.New("no browse image set")
	}
	if req.Name == "" || req.SecretName == "" {
		return errors.New("a browse session needs a name and a Secret")
	}
	if req.Idle <= 0 || req.MaxLife <= 0 {
		return errors.New("a browse session needs an idle limit and a hard limit")
	}
	if err := util.ValidateBackupPath(req.LatestPath); err != nil {
		return err
	}
	if req.MomentsPath != "" {
		if err := checkBackupFolders(req.LatestPath, req.MomentsPath); err != nil {
			return err
		}
	}
	args := []string{"/usr/local/bin/browse", "--mode=browse-server", "--root=/latest", fmt.Sprintf("--listen=:%d", browsePort),
		"--token-file=/etc/browse/token", "--tls-cert-file=/etc/browse/tls.crt", "--tls-key-file=/etc/browse/tls.key",
		"--idle=" + req.Idle.String(), "--max-life=" + req.MaxLife.String()}
	folders := []folderMount{{share: 0, subPath: strings.TrimPrefix(req.LatestPath, "/"), at: "/latest"}}
	if req.MomentsPath != "" {
		args = append(args, "--moments=/moments")
		if !req.At.IsZero() {
			args = append(args, "--at="+req.At.UTC().Format(time.RFC3339))
		}
		folders = append(folders, folderMount{share: 0, subPath: strings.TrimPrefix(req.MomentsPath, "/"), at: "/moments"})
	}
	owner := req.Owner
	shape := jobShape{
		image:     r.browseImage,
		command:   args,
		runAsUser: 0,
		folders:   folders,
		readOnly:  true,
		volumes: []corev1.Volume{{Name: "session", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: req.SecretName}}}},
		mounts: []corev1.VolumeMount{{Name: "session", MountPath: "/etc/browse", ReadOnly: true}},
		resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		labels:         map[string]string{browseLabel: req.Name},
		owner:          &owner,
		hardened:       true,
		session:        true,
		activeDeadline: int64((req.MaxLife + 5*time.Minute).Seconds()),
		readiness: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromInt32(browsePort), Scheme: corev1.URISchemeHTTPS}},
			PeriodSeconds: 2,
		},
	}
	key := browseKey(req.Name)
	err := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: shareJobName(key)}, &batchv1.Job{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get browse Job: %w", err)
	}
	return r.start(ctx, shareJobName(key), []ShareMount{req.Share}, shape)
}

// BrowseState reports on the helper pod of a session.
func (r *ShareJobRunner) BrowseState(ctx context.Context, name string) (BrowseJobState, error) {
	jobName := shareJobName(browseKey(name))
	job := &batchv1.Job{}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: jobName}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return BrowseJobState{}, nil
		}
		return BrowseJobState{}, fmt.Errorf("get browse Job: %w", err)
	}
	st := BrowseJobState{Exists: true}
	pods := &corev1.PodList{}
	if err := r.client.List(ctx, pods, client.InNamespace(r.namespace), client.MatchingLabels{shareJobLabel: jobName}); err != nil {
		return BrowseJobState{}, fmt.Errorf("list browse pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Status.PodIP != "" {
			st.PodIP = p.Status.PodIP
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				st.Ready = true
			}
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				st.Reason = cs.State.Terminated.Message
			}
		}
	}
	switch {
	case job.Status.Succeeded > 0:
		st.Finished, st.Succeeded, st.Ready = true, true, false
	case job.Status.Failed > 0 || jobHasFailed(job):
		st.Finished, st.Ready = true, false
	}
	return st, nil
}

// StopBrowse ends a session's helper pod and removes its share objects. It
// reports done once the pod is gone.
func (r *ShareJobRunner) StopBrowse(ctx context.Context, name string) (bool, error) {
	return r.Stop(ctx, browseKey(name))
}

// BrowseSessions lists the sessions that have a helper Job.
func (r *ShareJobRunner) BrowseSessions(ctx context.Context) ([]string, error) {
	jobs := &batchv1.JobList{}
	if err := r.client.List(ctx, jobs, client.InNamespace(r.namespace), client.HasLabels{browseLabel}); err != nil {
		return nil, fmt.Errorf("list browse Jobs: %w", err)
	}
	var names []string
	for _, j := range jobs.Items {
		names = append(names, j.Labels[browseLabel])
	}
	sort.Strings(names)
	return names, nil
}
