package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Browsing a backup in COS (docs/specs/2026-10-22-backup-file-restore.md,
// section 4). The helper pod mounts nothing from NFS. It gets the COS key, the
// wrapped backup key and the Key Protect API key as read-only files, as the
// export Job does, and unwraps the backup key in its own memory. The Secrets are
// in the driver namespace, and a pod can only mount Secrets of its own
// namespace, so these helper pods run in the driver namespace and not in the
// browse namespace. The keys are never copied into the browse namespace: the
// gateway may read Secrets there, and must not be able to reach the keys.

const (
	// cosStateVolume is Kopia's private scratch folder in the pod.
	cosStateVolume = "kopia-state"

	// Defaults for the limits of a helper pod that reads COS. Kopia and its
	// cache need more than a helper that reads a folder.
	defaultBrowseCOSCPU    = "1"
	defaultBrowseCOSMemory = "1Gi"
	cosStateLimit          = "2Gi"

	// cosHelperUser is the user the pod runs as. It reads no backup folder, so
	// it has no reason to be root.
	cosHelperUser = int64(65532)
)

// BrowseCOSRequest says which backup in COS a browse session reads.
type BrowseCOSRequest struct {
	// Repo is where the repository is and which Secrets open it. Only the names
	// of the Secrets are here.
	Repo ExportRepo
	// Policy is the area of the bucket, and Volume the backed-up volume.
	Policy string
	Volume string
}

// SetBrowseCOSResources sets the limits of the helper pod that reads a backup
// in COS. An empty value keeps the default.
func (r *ShareJobRunner) SetBrowseCOSResources(cpu, memory string) error {
	for _, q := range []string{cpu, memory} {
		if q == "" {
			continue
		}
		if _, err := resource.ParseQuantity(q); err != nil {
			return fmt.Errorf("%q is not a quantity: %w", q, err)
		}
	}
	r.browseCOSCPU, r.browseCOSMemory = cpu, memory
	return nil
}

// cosSourceOf is what the helper is told about the repository: names and IDs,
// never a key.
func cosSourceOf(repo ExportRepo, policy, volume string) cosbrowse.Source {
	src := cosbrowse.Source{
		Endpoint: repo.Endpoint, Bucket: repo.Bucket, Prefix: repo.Prefix, Policy: policy, Volume: volume,
		EmergencyKey: repo.EmergencyKeySecret != "", EmergencyOnly: repo.EmergencyOnly,
	}
	if !repo.EmergencyOnly {
		src.KeyProtect = &cosbrowse.KeyProtect{Region: repo.KeyProtect.Region, InstanceID: repo.KeyProtect.InstanceID, RootKeyID: repo.KeyProtect.RootKeyID}
	}
	return src
}

// cosSecretVolumes are the Secrets the helper reads, as files, and the folder
// Kopia writes to. The COS key is a file here and not an environment variable:
// the helper passes it on to its Kopia child and to nothing else.
func cosSecretVolumes(repo ExportRepo) ([]corev1.Volume, []corev1.VolumeMount) {
	const credentialsVolume = "cos-credentials"
	creds := corev1.Volume{Name: credentialsVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: repo.CredentialsSecret,
		Items: []corev1.KeyToPath{
			{Key: ExportAccessKeyKey, Path: ExportAccessKeyKey},
			{Key: ExportSecretKeyKey, Path: ExportSecretKeyKey},
		},
	}}}
	vols := []corev1.Volume{creds}
	mounts := []corev1.VolumeMount{{Name: credentialsVolume, MountPath: cosbrowse.CredentialsDir, ReadOnly: true}}
	keyVols, keyMounts := keyVolumes(repo), keyMounts(repo)
	for _, v := range keyVols {
		if v.Name != memoryKeyVolume { // the helper unwraps the key in its own memory
			vols = append(vols, v)
		}
	}
	for _, m := range keyMounts {
		if m.Name != memoryKeyVolume {
			mounts = append(mounts, m)
		}
	}
	limit := resource.MustParse(cosStateLimit)
	vols = append(vols, corev1.Volume{Name: cosStateVolume, VolumeSource: corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit}}})
	mounts = append(mounts, corev1.VolumeMount{Name: cosStateVolume, MountPath: cosbrowse.DefaultStateDir})
	return vols, mounts
}

// checkCOSSource checks a request for a helper or a Job that reads COS.
func checkCOSSource(repo ExportRepo, policy, volume string) (cosbrowse.Source, error) {
	if err := repo.validateRead(); err != nil {
		return cosbrowse.Source{}, err
	}
	src := cosSourceOf(repo, policy, volume)
	if err := src.Validate(); err != nil {
		return cosbrowse.Source{}, err
	}
	return src, nil
}

// startBrowseCOS starts the helper pod of a session that reads COS, or does
// nothing if it exists. The pod has no service account token, no share, and a
// read-only root file system; it runs as an ordinary user.
func (r *ShareJobRunner) startBrowseCOS(ctx context.Context, req BrowseJobRequest) error {
	src, err := checkCOSSource(req.COS.Repo, req.COS.Policy, req.COS.Volume)
	if err != nil {
		return err
	}
	srcJSON, err := json.Marshal(src)
	if err != nil {
		return err
	}
	args := []string{"/usr/local/bin/browse", "--mode=browse-server", "--cos-source=" + string(srcJSON),
		"--kopia=/usr/local/bin/kopia", "--kopia-state-dir=" + cosbrowse.DefaultStateDir,
		fmt.Sprintf("--listen=:%d", browsePort),
		"--token-file=/etc/browse/token", "--tls-cert-file=/etc/browse/tls.crt", "--tls-key-file=/etc/browse/tls.key",
		"--idle=" + req.Idle.String(), "--max-life=" + req.MaxLife.String()}
	if !req.At.IsZero() {
		args = append(args, "--at="+req.At.UTC().Format(time.RFC3339))
	}
	vols, mounts := cosSecretVolumes(req.COS.Repo)
	vols = append(vols, corev1.Volume{Name: "session", VolumeSource: corev1.VolumeSource{
		Secret: &corev1.SecretVolumeSource{SecretName: req.SecretName}}})
	mounts = append(mounts, corev1.VolumeMount{Name: "session", MountPath: "/etc/browse", ReadOnly: true})
	cpu, mem := orDefault(r.browseCOSCPU, defaultBrowseCOSCPU), orDefault(r.browseCOSMemory, defaultBrowseCOSMemory)
	owner := req.Owner
	shape := jobShape{
		image:     r.browseImage,
		command:   args,
		runAsUser: cosHelperUser,
		volumes:   vols,
		mounts:    mounts,
		resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
		},
		labels:         map[string]string{browseLabel: req.ID},
		owner:          &owner,
		hardened:       true,
		noToken:        true,
		pullSecrets:    r.browsePullSecrets,
		session:        true,
		activeDeadline: int64((req.MaxLife + 5*time.Minute).Seconds()),
		readiness: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromInt32(browsePort), Scheme: corev1.URISchemeHTTPS}},
			PeriodSeconds: 2,
		},
	}
	key := browseKey(req.ID)
	err = r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: shareJobName(key)}, &batchv1.Job{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get browse Job: %w", err)
	}
	return r.start(ctx, shareJobName(key), nil, shape)
}

var errNoBrowseCOS = errors.New("browsing a backup in COS is not set up")

// BrowseRouter sends each browse session to the runner that suits it. A session
// on a snapshot pool runs in the browse namespace, because it needs only a
// token and a certificate. A session on COS runs in the driver namespace,
// because it needs the COS and Key Protect Secrets, which the gateway must not
// be able to read.
type BrowseRouter struct {
	// Folder runs sessions that read a folder on a share.
	Folder BrowseJobs
	// COS runs sessions that read a backup in COS. It may be nil.
	COS BrowseJobs
}

var _ BrowseJobs = (*BrowseRouter)(nil)

// StartBrowse starts the helper where the request belongs.
func (b *BrowseRouter) StartBrowse(ctx context.Context, req BrowseJobRequest) error {
	if req.COS == nil {
		return b.Folder.StartBrowse(ctx, req)
	}
	if b.COS == nil {
		return errNoBrowseCOS
	}
	return b.COS.StartBrowse(ctx, req)
}

// BrowseState reports on the helper of a session, wherever it runs.
func (b *BrowseRouter) BrowseState(ctx context.Context, name string) (BrowseJobState, error) {
	st, err := b.Folder.BrowseState(ctx, name)
	if err != nil || st.Exists || b.COS == nil {
		return st, err
	}
	return b.COS.BrowseState(ctx, name)
}

// StopBrowse stops the helper of a session in both places, and reports done
// when it is gone from both.
func (b *BrowseRouter) StopBrowse(ctx context.Context, name string) (bool, error) {
	done, err := b.Folder.StopBrowse(ctx, name)
	if err != nil || b.COS == nil {
		return done, err
	}
	cosDone, cerr := b.COS.StopBrowse(ctx, name)
	if cerr != nil {
		return false, cerr
	}
	return done && cosDone, nil
}

// BrowseSessions lists the sessions that have a helper in either place.
func (b *BrowseRouter) BrowseSessions(ctx context.Context) ([]string, error) {
	names, err := b.Folder.BrowseSessions(ctx)
	if err != nil || b.COS == nil {
		return names, err
	}
	more, err := b.COS.BrowseSessions(ctx)
	if err != nil {
		return names, err
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range append(names, more...) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Namespace is the browse namespace.
func (b *BrowseRouter) Namespace() string { return b.Folder.Namespace() }

// GuestEnabled says whether sessions on the file systems inside VM disks may
// start. They run with the folder runner, in the browse namespace.
func (b *BrowseRouter) GuestEnabled() bool { return b.Folder.GuestEnabled() }
