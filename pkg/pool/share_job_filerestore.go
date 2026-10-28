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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// fileRestoreDeadline is the hard stop of a file restore Job.
	fileRestoreDeadline = 4 * time.Hour

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
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return false, filerestore.Outcome{}, err
	}
	shares := []ShareMount{req.Backup}
	folders := []folderMount{{share: 0, subPath: strings.TrimPrefix(req.LatestPath, "/"), at: spec.Latest, readOnly: true}}
	if req.MomentsPath != "" {
		folders = append(folders, folderMount{share: 0, subPath: strings.TrimPrefix(req.MomentsPath, "/"), at: spec.Moments, readOnly: true})
	}
	hardened := true
	var caps []corev1.Capability
	if req.Mode != filerestore.ModeMeasure {
		// The whole share, because the claim's folder may not exist before the
		// Job makes it, and a folder that does not exist cannot be mounted. The
		// Job opens only the claim's folder below it, one name at a time.
		shares = append(shares, req.Dest)
		folders = append(folders, folderMount{share: 1, at: spec.Dest})
		caps = []corev1.Capability{"CHOWN", "FOWNER"}
	}
	owner := req.Owner
	shape := jobShape{
		image: r.driverImage,
		command: []string{"/usr/local/bin/vpc-file-pool-csi", "--mode=restore-files",
			"--restore-files-spec=" + string(specJSON), "--restore-files-paths=" + fileRestorePathsFile},
		runAsUser: 0,
		folders:   folders,
		readOnly:  req.Mode == filerestore.ModeMeasure,
		volumes: []corev1.Volume{{Name: "paths", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: pathsConfigMapName(req.Key)},
			Items:                []corev1.KeyToPath{{Key: "paths", Path: "paths"}},
		}}}},
		mounts:         []corev1.VolumeMount{{Name: "paths", MountPath: fileRestorePathsMount, ReadOnly: true}},
		owner:          &owner,
		hardened:       hardened,
		addCaps:        caps,
		activeDeadline: int64(fileRestoreDeadline.Seconds()),
	}
	done, msg, err := r.runMessage(ctx, req.Key, shares, shape)
	if err != nil || !done {
		return false, filerestore.Outcome{}, err
	}
	var out filerestore.Outcome
	if jerr := json.Unmarshal([]byte(msg), &out); jerr != nil {
		// The Job succeeded but left no readable outcome (for example the
		// message was lost). The files are written; the counts are not known.
		return true, filerestore.Outcome{}, nil
	}
	return true, out, nil
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
	if req.MomentsPath != "" {
		if err := checkBackupFolders(req.LatestPath, req.MomentsPath); err != nil {
			return spec, err
		}
	} else if err := util.ValidateBackupPath(req.LatestPath); err != nil {
		return spec, err
	}
	if req.Permissions != "" && !validPermissionsPattern.MatchString(req.Permissions) {
		return spec, fmt.Errorf("invalid permissions %q", req.Permissions)
	}
	spec = filerestore.JobSpec{
		Mode: req.Mode, Latest: "/src", MaxBytes: req.MaxBytes,
		UID: req.UID, GID: req.GID, Permissions: req.Permissions, CapacityBytes: req.CapacityBytes,
	}
	if req.MomentsPath != "" {
		spec.Moments = "/moments"
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
