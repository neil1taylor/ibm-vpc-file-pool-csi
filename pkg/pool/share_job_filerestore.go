package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// fileRestoreDeadline is the hard stop of a file restore Job.
	fileRestoreDeadline = 4 * time.Hour

	// guestFileRestoreDeadline is the hard stop of a file restore Job that reads a
	// VM disk. The disk is held (its next backup copy waits) while the Job runs,
	// and a hold is promised for 2 hours at most.
	guestFileRestoreDeadline = 2 * time.Hour

	// guestSocketDir is the folder, shared by the reader and the writer of a guest
	// restore, where the writer listens.
	guestSocketDir = "/run/restore"
	guestSocketVol = "restore-sock"

	fileRestorePathsMount = "/etc/filerestore"
	fileRestorePathsFile  = fileRestorePathsMount + "/paths"
)

// policyNamePattern is what a policy name may be in a Job's arguments: a
// Kubernetes name, nothing a shell or a flag parser would read.
var policyNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// RunFileRestore runs the Job of a file restore, or reports on the Job that
// exists. The Job runs the driver's restore-files mode. The paths to restore are
// in a ConfigMap that the restore owns, mounted as a file: a path is never an
// argument and never part of a script. The backup is mounted read-only down to
// the folders of the one volume; a Job that writes also gets the share that
// holds the target claim.
func (r *ShareJobRunner) RunFileRestore(ctx context.Context, req FileRestoreRequest) (bool, filerestore.Outcome, error) {
	spec, err := r.fileRestoreSpec(req)
	if err != nil {
		return false, filerestore.Outcome{}, err
	}
	if err = r.ensurePathsConfigMap(ctx, req); err != nil {
		return false, filerestore.Outcome{}, err
	}
	var shares []ShareMount
	var folders []folderMount
	var cosVolumes []corev1.Volume
	var cosMounts []corev1.VolumeMount
	if req.COS != nil {
		// The backup is read from COS: there is no share to mount for it, and the
		// Job holds the keys, so it gets no service account token.
		cosVolumes, cosMounts = cosSecretVolumes(req.COS.Repo)
	} else {
		shares = []ShareMount{req.Backup}
		folders = []folderMount{{share: 0, subPath: strings.TrimPrefix(req.LatestPath, "/"), at: spec.Latest, readOnly: true}}
		if req.MomentsPath != "" {
			folders = append(folders, folderMount{share: 0, subPath: strings.TrimPrefix(req.MomentsPath, "/"), at: spec.Moments, readOnly: true})
		}
	}
	hardened := true
	var caps []corev1.Capability
	var sidecars []sideContainer
	var sockVolumes []corev1.Volume
	var sockMounts []corev1.VolumeMount
	runAs := int64(0)
	if req.Mode != filerestore.ModeMeasure && req.Guest != nil {
		// The Job that parses the hostile disk only reads. Its writes go to a
		// second container, which holds the target and the capabilities and never
		// sees the disk.
		w, werr := guestWriter(&spec, len(shares))
		if werr != nil {
			return false, filerestore.Outcome{}, werr
		}
		shares = append(shares, req.Dest)
		sidecars = []sideContainer{w}
		sockSize := resource.MustParse("1Mi")
		sockVolumes = []corev1.Volume{{Name: guestSocketVol, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &sockSize}}}}
		sockMounts = []corev1.VolumeMount{{Name: guestSocketVol, MountPath: guestSocketDir}}
	} else if req.Mode != filerestore.ModeMeasure {
		// The whole share, because the claim's folder may not exist before the
		// Job makes it, and a folder that does not exist cannot be mounted. The
		// Job opens only the claim's folder below it, one name at a time.
		folders = append(folders, folderMount{share: len(shares), at: spec.Dest})
		shares = append(shares, req.Dest)
		caps = []corev1.Capability{"CHOWN", "FOWNER"}
	} else if req.COS != nil {
		// Measuring a backup in COS writes nothing and reads no share.
		runAs = cosHelperUser
	}
	owner := req.Owner
	image, program, deadline := r.driverImage, "/usr/local/bin/vpc-file-pool-csi", fileRestoreDeadline
	if req.Guest != nil {
		// The driver image has no libguestfs: the guest image runs the Job with its
		// own program, which has the same restore mode.
		image, program, deadline = r.guestImage, "/usr/local/bin/browse", guestFileRestoreDeadline
		if req.MaxRun > 0 && req.MaxRun < deadline {
			deadline = req.MaxRun
		}
	}
	// The Job's spec is made after guestWriter, which takes the target out of it.
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return false, filerestore.Outcome{}, err
	}
	shape := jobShape{
		image: image,
		command: []string{program, "--mode=restore-files",
			"--restore-files-spec=" + string(specJSON), "--restore-files-paths=" + fileRestorePathsFile},
		runAsUser: runAs,
		folders:   folders,
		readOnly:  req.Mode == filerestore.ModeMeasure,
		sidecars:  sidecars,
		volumes: append(append([]corev1.Volume{{Name: "paths", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: pathsConfigMapName(req.Key)},
			Items:                []corev1.KeyToPath{{Key: "paths", Path: "paths"}},
		}}}}, cosVolumes...), sockVolumes...),
		mounts:         append(append([]corev1.VolumeMount{{Name: "paths", MountPath: fileRestorePathsMount, ReadOnly: true}}, cosMounts...), sockMounts...),
		noToken:        req.COS != nil || req.Guest != nil, // a Job that parses a guest disk or holds keys never gets the API token
		owner:          &owner,
		hardened:       hardened,
		addCaps:        caps,
		activeDeadline: int64(deadline.Seconds()),
	}
	if req.Guest != nil {
		r.shapeGuest(&shape)
	}
	done, msgs, err := r.runMessages(ctx, req.Key, shares, shape)
	if err != nil || !done {
		return false, filerestore.Outcome{}, err
	}
	var out filerestore.Outcome
	if jerr := json.Unmarshal([]byte(msgs.main()), &out); jerr != nil {
		out = filerestore.Outcome{}
		if len(sidecars) == 0 {
			// The Job succeeded but left no readable outcome (for example the
			// message was lost). The files are written; the counts are not known.
			return true, out, nil
		}
	}
	if len(sidecars) > 0 {
		out = guestOutcome(out, msgs.of(writerContainerName))
	}
	return true, out, nil
}

// guestOutcome makes the outcome of a restore from a VM disk. The reader parses
// the untrusted disk, so what it says about the target is not used: the counts
// and the folder are the writer's, which holds the target. The reader gives only
// what the writer cannot know: the moment that was read, what was left out and
// what could not be read, and the reason of a failure. A writer that left no
// outcome makes the restore fail, so a forged reader message never reaches the
// status.
func guestOutcome(reader filerestore.Outcome, writerMsg string) filerestore.Outcome {
	out := filerestore.Outcome{Moment: reader.Moment, Latest: reader.Latest, Error: reader.Error}
	out.Skipped, out.Unreadable = reader.Skipped, reader.Unreadable
	var w filerestore.WriterOutcome
	if err := json.Unmarshal([]byte(writerMsg), &w); err != nil {
		if out.Error == "" {
			out.Error = "the writer of the restore left no outcome, so what was restored is not known"
		}
		return out
	}
	out.Files, out.Dirs, out.Links, out.Bytes, out.Folder = w.Files, w.Dirs, w.Links, w.Bytes, w.Folder
	return out
}

// guestWriter takes the target out of the spec of the reader and makes the
// writer that holds it. The writer mounts the claim's folder only (ModeFolder,
// where the folder exists). A new claim has no folder yet, and a folder that
// does not exist cannot be mounted, so its writer mounts the share and makes the
// claim's folder itself; it still opens nothing else.
func guestWriter(spec *filerestore.JobSpec, shareIndex int) (sideContainer, error) {
	ws := filerestore.WriterSpec{
		Mode: spec.Mode, Socket: guestSocketDir + "/writer.sock", Dest: spec.Dest,
		Staging: spec.Staging, Policy: spec.Policy,
		CapacityBytes: spec.CapacityBytes, MaxBytes: spec.MaxBytes, UID: spec.UID, GID: spec.GID,
		Permissions: spec.Permissions,
	}
	switch {
	case spec.UID != nil && spec.GID != nil:
	case spec.UID == nil && spec.GID == nil && spec.Mode == filerestore.ModeFolder:
		// The claim exists: its folder has an owner, which the writer reads
		// before it accepts any data.
		ws.OwnerFromFolder = true
	default:
		return sideContainer{}, errors.New("the owner of the target claim is not known")
	}
	dst := folderMount{share: shareIndex, at: spec.Dest}
	if spec.Mode == filerestore.ModeNewClaim {
		ws.Volume = spec.Volume
	} else {
		dst.subPath = spec.Volume
	}
	b, err := json.Marshal(ws)
	if err != nil {
		return sideContainer{}, err
	}
	spec.Writer = ws.Socket
	spec.Dest, spec.Volume, spec.Staging, spec.Policy, spec.CapacityBytes = "", "", "", "", 0
	return sideContainer{
		name:    writerContainerName,
		command: []string{"/usr/local/bin/browse", "--mode=restore-writer", "--restore-writer-spec=" + string(b)},
		folders: []folderMount{dst},
		mounts:  []corev1.VolumeMount{{Name: guestSocketVol, MountPath: guestSocketDir}},
		addCaps: []corev1.Capability{"CHOWN", "FOWNER"},
	}, nil
}

func pathsConfigMapName(key string) string { return shareJobName(key) + "-paths" }

// fileRestoreSpec checks a request and works out what the Job is told.
func (r *ShareJobRunner) fileRestoreSpec(req FileRestoreRequest) (filerestore.JobSpec, error) {
	var spec filerestore.JobSpec
	if r.driverImage == "" {
		return spec, errors.New("no driver image set for file restore Jobs")
	}
	if req.Key == "" {
		return spec, errors.New("a file restore Job needs a key")
	}
	switch req.Mode {
	case filerestore.ModeMeasure, filerestore.ModeNewClaim, filerestore.ModeFolder:
	default:
		return spec, fmt.Errorf("unknown file restore mode %q", req.Mode)
	}
	if _, err := filerestore.CheckPaths(req.Paths); err != nil {
		return spec, err
	}
	switch {
	case req.Guest != nil:
		if r.guestImage == "" {
			return spec, errors.New("restoring files from inside a VM disk needs the guest image, which is not set")
		}
		if req.COS != nil {
			return spec, errors.New("a VM disk is read from its backup folder only, not from COS")
		}
		if req.MomentsPath != "" {
			if err := checkBackupFolders(req.LatestPath, req.MomentsPath); err != nil {
				return spec, err
			}
		}
		if c := req.Guest.Consistency; c != "" && c != "FileSystemConsistent" && c != "CrashConsistent" {
			return spec, errors.New("the consistency of the copy is not one that the Job knows")
		}
		if req.MomentsPath == "" {
			if err := util.ValidateBackupPath(req.LatestPath); err != nil {
				return spec, err
			}
		}
	case req.COS != nil:
		if req.LatestPath != "" || req.MomentsPath != "" {
			return spec, errors.New("a restore from COS names no backup folder")
		}
	case req.MomentsPath != "":
		if err := checkBackupFolders(req.LatestPath, req.MomentsPath); err != nil {
			return spec, err
		}
	default:
		if err := util.ValidateBackupPath(req.LatestPath); err != nil {
			return spec, err
		}
	}
	if req.Permissions != "" && !validPermissionsPattern.MatchString(req.Permissions) {
		return spec, fmt.Errorf("invalid permissions %q", req.Permissions)
	}
	spec = filerestore.JobSpec{
		Mode: req.Mode, MaxBytes: req.MaxBytes,
		UID: req.UID, GID: req.GID, Permissions: req.Permissions, CapacityBytes: req.CapacityBytes,
	}
	if req.COS != nil {
		src, err := checkCOSSource(req.COS.Repo, req.COS.Policy, req.COS.Volume)
		if err != nil {
			return spec, err
		}
		spec.COS = &src
	} else {
		spec.Latest = "/src"
		if req.MomentsPath != "" {
			spec.Moments = "/moments"
		}
		if req.Guest != nil {
			spec.Guest = &filerestore.GuestSource{Socket: "/tmp/disk.sock", Appliance: "/opt/guestfs-appliance", Consistency: req.Guest.Consistency}
		}
	}
	if !req.At.IsZero() {
		spec.At = req.At.UTC().Format(time.RFC3339)
	}
	if req.Mode == filerestore.ModeMeasure {
		return spec, nil
	}
	if err := util.ValidateSubDir(req.Volume); err != nil {
		return spec, fmt.Errorf("the target volume: %w", err)
	}
	spec.Dest, spec.Volume = "/dst", strings.TrimPrefix(req.Volume, "/")
	if req.Mode == filerestore.ModeFolder {
		if !policyNamePattern.MatchString(req.Policy) {
			return spec, fmt.Errorf("policy %q is not a policy name", req.Policy)
		}
		if req.Owner.UID == "" {
			return spec, errors.New("a file restore Job needs the UID of its restore")
		}
		spec.Policy, spec.Staging = req.Policy, filerestore.StagingName(string(req.Owner.UID))
	}
	return spec, nil
}

// ensurePathsConfigMap keeps the list of paths of a restore in a ConfigMap. A
// ConfigMap that exists is left alone, so a Job that starts again reads the
// list it started with.
func (r *ShareJobRunner) ensurePathsConfigMap(ctx context.Context, req FileRestoreRequest) error {
	name := pathsConfigMapName(req.Key)
	err := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: name}, &corev1.ConfigMap{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get the ConfigMap of paths: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: r.namespace, Name: name,
			Labels:          map[string]string{shareJobLabel: shareJobName(req.Key)},
			OwnerReferences: []metav1.OwnerReference{req.Owner},
		},
		Data: map[string]string{"paths": strings.Join(req.Paths, "\n") + "\n"},
	}
	if err := r.client.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("make the ConfigMap of paths: %w", err)
	}
	return nil
}
