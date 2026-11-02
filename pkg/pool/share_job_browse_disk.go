package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// A session on the files inside a VM disk in COS has two pods
// (docs/specs/2026-10-29-vm-disks-in-cos.md, section 2):
//
//   - the disk server, in the driver namespace. It holds the COS and Key
//     Protect Secrets, reads COS through Kopia and hands out bytes of the disk
//     over HTTPS. It never parses the guest's file system.
//   - the guest reader, in the browse namespace. It runs the libguestfs
//     appliance on a disk it reads from the disk server. It holds no key: only
//     the token and certificate of the disk server.

const (
	diskReadMountDir = "/etc/disk"
	diskServerPort   = browsePort
)

// StartBrowseDisk starts the disk server of a session, or does nothing if it
// exists. The pod has no service account token, no share and a read-only root
// file system, and runs as an ordinary user.
func (r *ShareJobRunner) StartBrowseDisk(ctx context.Context, req BrowseJobRequest) error {
	if req.COS == nil {
		return errors.New("the disk server needs a backup in COS")
	}
	if req.DiskSecretName == "" {
		return errors.New("the disk server needs its session Secret")
	}
	src, err := checkCOSSource(req.COS.Repo, req.COS.Policy, req.COS.Volume)
	if err != nil {
		return err
	}
	srcJSON, err := json.Marshal(src)
	if err != nil {
		return err
	}
	args := []string{"/usr/local/bin/browse", "--mode=cos-disk-server", "--cos-source=" + string(srcJSON),
		"--kopia=/usr/local/bin/kopia", "--kopia-state-dir=" + cosbrowse.DefaultStateDir,
		fmt.Sprintf("--listen=:%d", diskServerPort),
		"--token-file=/etc/browse/token", "--tls-cert-file=/etc/browse/tls.crt", "--tls-key-file=/etc/browse/tls.key",
		"--idle=" + req.Idle.String(), "--max-life=" + req.MaxLife.String()}
	if !req.At.IsZero() {
		args = append(args, "--at="+req.At.UTC().Format(time.RFC3339))
	}
	vols, mounts := cosSecretVolumes(req.COS.Repo)
	vols = append(vols, corev1.Volume{Name: "session", VolumeSource: corev1.VolumeSource{
		Secret: &corev1.SecretVolumeSource{SecretName: req.DiskSecretName}}})
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
		labels:         map[string]string{browseLabel: req.ID, browseDiskLabel: "true"},
		owner:          &owner,
		hardened:       true,
		noToken:        true,
		pullSecrets:    r.browsePullSecrets,
		session:        true,
		activeDeadline: int64((req.MaxLife + 5*time.Minute).Seconds()),
		readiness: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromInt32(diskServerPort), Scheme: corev1.URISchemeHTTPS}},
			PeriodSeconds: 2,
		},
	}
	name := shareJobName(browseDiskKey(req.ID))
	err = r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: name}, &batchv1.Job{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get the disk server Job: %w", err)
	}
	return r.start(ctx, name, nil, shape)
}

// startBrowseDiskReader starts the guest reader of a session whose disk is in a
// disk server. It mounts no share and no key.
func (r *ShareJobRunner) startBrowseDiskReader(ctx context.Context, req BrowseJobRequest) error {
	if !req.Guest {
		return errors.New("a disk server is for a session on the files inside a VM disk")
	}
	if r.guestImage == "" {
		return errNoBrowseGuest
	}
	ip := net.ParseIP(req.Disk.IP)
	if ip == nil || ip.To4() == nil || ip.String() != req.Disk.IP {
		return errors.New("the address of the disk server is not an IPv4 address")
	}
	if req.Disk.SecretName == "" {
		return errors.New("the guest reader needs the Secret of the disk server")
	}
	if req.Consistency != "" && req.Consistency != "FileSystemConsistent" && req.Consistency != "CrashConsistent" {
		return errors.New("the consistency of the copy is not one that the helper knows")
	}
	args := []string{"/usr/local/bin/browse", "--mode=browse-server", fmt.Sprintf("--listen=:%d", browsePort),
		"--token-file=/etc/browse/token", "--tls-cert-file=/etc/browse/tls.crt", "--tls-key-file=/etc/browse/tls.key",
		"--idle=" + req.Idle.String(), "--max-life=" + req.MaxLife.String(),
		"--guest", "--guest-socket=/tmp/disk.sock", "--guest-appliance=/opt/guestfs-appliance",
		fmt.Sprintf("--guest-disk-url=https://%s:%d/disk", req.Disk.IP, diskServerPort),
		"--guest-disk-cert-file=" + diskReadMountDir + "/tls.crt", "--guest-disk-token-file=" + diskReadMountDir + "/token"}
	if req.Consistency != "" {
		args = append(args, "--guest-consistency="+req.Consistency)
	}
	owner := req.Owner
	shape := jobShape{
		image:     r.guestImage,
		command:   args,
		runAsUser: 0,
		volumes: []corev1.Volume{
			{Name: "session", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: req.SecretName}}},
			// The token and the certificate of the disk server, and never its key.
			{Name: "disk", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: req.Disk.SecretName,
				Items:      []corev1.KeyToPath{{Key: "token", Path: "token"}, {Key: "tls.crt", Path: "tls.crt"}},
			}}},
		},
		mounts: []corev1.VolumeMount{
			{Name: "session", MountPath: "/etc/browse", ReadOnly: true},
			{Name: "disk", MountPath: diskReadMountDir, ReadOnly: true},
		},
		labels:         map[string]string{browseLabel: req.ID, browseGuestLabel: "true"},
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
	r.shapeGuest(&shape)
	name := shareJobName(browseKey(req.ID))
	err := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: name}, &batchv1.Job{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get browse Job: %w", err)
	}
	return r.start(ctx, name, nil, shape)
}
