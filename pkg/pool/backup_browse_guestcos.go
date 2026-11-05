package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// A session on the files inside a VM disk in COS has two pods: the disk server
// in the driver namespace, which holds the keys, and the guest reader in the
// browse namespace, which holds none. The controller starts the disk server
// first, and starts the reader once the disk server is ready and has an address.
// See docs/specs/2026-10-29-vm-disks-in-cos.md, section 2.

// BrowseDiskJobs is the part of the runner that starts and follows the disk
// server of such a session. A runner without it cannot run these sessions.
type BrowseDiskJobs interface {
	StartBrowseDisk(ctx context.Context, req BrowseJobRequest) error
	DiskState(ctx context.Context, name string) (BrowseJobState, error)
}

func browseDiskSecretName(session string) string { return "browsedisk-" + session }

// guestKeepMargin is how much longer than the longest session an export must be
// kept in COS for a session to be allowed: the session must not lose its disk.
const guestKeepMargin = time.Hour

// checkGuestKeep refuses a session on an export that the maintenance Job will
// delete before the session can end.
func (bc *BackupController) checkGuestKeep(loc cosLocation, at time.Time) error {
	return bc.checkKeepFor(loc, at, bc.browse.MaxLife, "a session may last", "Browse a newer backup")
}

// keepStart is the time from which the keep time of the export that covers the
// restore point at is counted. That export is the oldest one made at or after
// at. The policy records only the time of the newest export, so for an older
// restore point the count starts at the restore point itself: the covering
// export is no older than that, and so is deleted no sooner. The check can
// refuse a restore point whose export is a little newer than the restore point;
// it never allows one whose export will be deleted too soon.
func keepStart(exported, at time.Time) time.Time {
	if !at.IsZero() && at.Before(exported) {
		return at
	}
	return exported
}

// checkKeepFor refuses a backup that will be deleted from COS before the work
// (lasting at most life) can end, with a margin of an hour. at is the restore
// point asked for; the zero time means the newest.
func (bc *BackupController) checkKeepFor(loc cosLocation, at time.Time, life time.Duration, what, advice string) error {
	if loc.entry == nil || loc.entry.ExportedTime == nil || loc.repo.KeepFor <= 0 {
		return nil
	}
	remaining := keepStart(loc.entry.ExportedTime.Time, at).Add(loc.repo.KeepFor).Sub(bc.nowFunc())
	if remaining < life+guestKeepMargin {
		return restoreFailure(fmt.Sprintf(
			"this backup will be deleted from COS in %s, which is less than %s (%s) plus an hour. %s",
			remaining.Round(time.Minute), what, life, advice))
	}
	return nil
}

// guestCOSRequest adds what the disk server of a session on a VM disk needs.
func (bc *BackupController) guestCOSRequest(req BrowseJobRequest, loc cosLocation) (BrowseJobRequest, error) {
	if _, ok := bc.browseJobs.(BrowseDiskJobs); !ok {
		return BrowseJobRequest{}, restoreFailure("browsing the files inside a VM disk in COS is not set up in this installation")
	}
	if loc.entry != nil && loc.entry.VMName == "" {
		return BrowseJobRequest{}, restoreFailure(fmt.Sprintf("%s/%s is not a VM disk, so it has no guest file systems to browse", loc.entry.PVCNamespace, loc.entry.PVCName))
	}
	if err := bc.checkGuestKeep(loc, req.At); err != nil {
		return BrowseJobRequest{}, err
	}
	req.Guest = true
	req.DiskSecretName = browseDiskSecretName(req.ID)
	if loc.entry != nil {
		if c := loc.entry.Consistency; c == "FileSystemConsistent" || c == "CrashConsistent" {
			req.Consistency = c
		}
	}
	return req, nil
}

// ensureDiskSecrets makes the Secret of the disk server (token, certificate and
// key) in the driver namespace, and a copy without the key in the browse
// namespace for the reader. Both are owned by the BackupBrowse and labelled, so
// the sweeps remove them. An existing Secret is left alone.
func (bc *BackupController) ensureDiskSecrets(ctx context.Context, bb *v1alpha1.BackupBrowse) error {
	name := browseDiskSecretName(string(bb.UID))
	labels := map[string]string{browseLabel: string(bb.UID)}
	owner := []metav1.OwnerReference{browseOwner(bb)}
	full := &corev1.Secret{}
	err := bc.client.Get(ctx, types.NamespacedName{Namespace: bc.browse.COSNamespace, Name: name}, full)
	switch {
	case apierrors.IsNotFound(err):
		raw := make([]byte, 32)
		if _, rerr := rand.Read(raw); rerr != nil {
			return rerr
		}
		cert, key, cerr := browse.NewSessionCert(bc.browse.MaxLife + 30*time.Minute)
		if cerr != nil {
			return cerr
		}
		full = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: bc.browse.COSNamespace, Name: name, Labels: labels, OwnerReferences: owner},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"token": []byte(hex.EncodeToString(raw)), "tls.crt": cert, "tls.key": key},
		}
		if cerr = bc.client.Create(ctx, full); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return cerr
		}
	case err != nil:
		return err
	}
	if bc.browse.COSNamespace == bc.browse.Namespace {
		return nil
	}
	err = bc.client.Get(ctx, types.NamespacedName{Namespace: bc.browse.Namespace, Name: name}, &corev1.Secret{})
	if err == nil || !apierrors.IsNotFound(err) {
		return err
	}
	cp := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: bc.browse.Namespace, Name: name, Labels: labels, OwnerReferences: owner},
		Type:       corev1.SecretTypeOpaque,
		// The reader gets what it needs to trust the disk server and to ask it;
		// the private key stays in the driver namespace.
		Data: map[string][]byte{"token": full.Data["token"], "tls.crt": full.Data["tls.crt"]},
	}
	if err = bc.client.Create(ctx, cp); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// deleteDiskSecrets removes both Secrets of a session's disk server.
func (bc *BackupController) deleteDiskSecrets(ctx context.Context, session string) {
	name := browseDiskSecretName(session)
	for _, ns := range []string{bc.browse.COSNamespace, bc.browse.Namespace} {
		if ns == "" {
			continue
		}
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		if err := bc.client.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete a disk server Secret", "secret", name, "namespace", ns)
		}
	}
}

func isGuestCOS(bb *v1alpha1.BackupBrowse) bool { return bb.Spec.Guest && browseIsCOS(&bb.Spec) }

// startGuestCOS starts the disk server of a session. The reader follows once
// the disk server is ready (watchGuestCOS).
func (bc *BackupController) startGuestCOS(ctx context.Context, bb *v1alpha1.BackupBrowse, req BrowseJobRequest) error {
	if err := bc.ensureDiskSecrets(ctx, bb); err != nil {
		return fmt.Errorf("make the Secret of the disk server: %w", err)
	}
	jobs, ok := bc.browseJobs.(BrowseDiskJobs)
	if !ok {
		return errors.New("the browse job runner cannot start a disk server")
	}
	return jobs.StartBrowseDisk(ctx, req)
}

// watchGuestCOS moves a session on a VM disk in COS along: it restarts a disk
// server that is gone, starts the reader when the disk server is ready, and
// ends the session when the disk server ends. It reports true when it has dealt
// with this pass.
func (bc *BackupController) watchGuestCOS(ctx context.Context, bb *v1alpha1.BackupBrowse, counts *browseCounts, reader BrowseJobState) bool {
	disks, ok := bc.browseJobs.(BrowseDiskJobs)
	if !ok {
		bc.endBrowse(ctx, bb, counts, browsePhaseFailed, "", "browsing the files inside a VM disk in COS is not set up in this installation")
		return true
	}
	ds, err := disks.DiskState(ctx, string(bb.UID))
	if err != nil {
		klog.ErrorS(err, "Failed to read the state of a disk server", "session", bb.Name)
		return true
	}
	switch {
	case ds.Finished && !ds.Succeeded:
		bc.endBrowse(ctx, bb, counts, browsePhaseFailed, "", "the disk server failed: "+helperFailureText(ds.Reason))
		return true
	case ds.Finished:
		reason := ds.Reason
		if reason == "" {
			reason = "stopped"
		}
		bc.endBrowse(ctx, bb, counts, browsePhaseClosed, reason, "")
		return true
	case !ds.Exists && bb.Status.Phase == browsePhaseReady:
		bc.endBrowse(ctx, bb, counts, browsePhaseFailed, "", "the disk server is gone")
		return true
	case !ds.Exists:
		req, _, rerr := bc.browseRequest(ctx, bb)
		if rerr == nil {
			rerr = bc.startGuestCOS(ctx, bb, req)
		}
		if rerr != nil {
			bc.setBrowseStatus(ctx, bb, browsePhaseStarting, fmt.Sprintf("start the disk server: %v", rerr))
		}
		return true
	case reader.Exists:
		return false // the reader is up: the usual watch goes on
	case (!ds.Ready || ds.PodIP == "") && bb.Status.Phase == browsePhaseStarting && bb.Status.StartedTime != nil &&
		bc.nowFunc().Sub(bb.Status.StartedTime.Time) >= bc.diskStartTimeout(ctx, bb):
		limit := bc.diskStartTimeout(ctx, bb)
		bc.endBrowse(ctx, bb, counts, browsePhaseFailed, "", fmt.Sprintf(
			"the disk server was not ready after %s. Check that the Secrets named by the export block exist in namespace %s and hold the keys access_key_id, secret_access_key, wrappedKey and apikey, and that the image can be pulled. An older restore point is read from the undo files in the backup, which takes longer",
			limit, bc.browse.COSNamespace))
		return true
	case !ds.Ready || ds.PodIP == "":
		bc.setBrowseStatus(ctx, bb, browsePhaseStarting, "starting the disk server")
		return true
	}
	req, _, rerr := bc.browseRequest(ctx, bb)
	if rerr == nil {
		req.COS = nil // the reader holds no key
		req.Disk = &BrowseDisk{IP: ds.PodIP, SecretName: browseDiskSecretName(string(bb.UID))}
		rerr = bc.browseJobs.StartBrowse(ctx, req)
	}
	if rerr != nil {
		bc.setBrowseStatus(ctx, bb, browsePhaseStarting, fmt.Sprintf("start the reader: %v", rerr))
		return true
	}
	bc.setBrowseStatus(ctx, bb, browsePhaseStarting, "starting the reader of the VM disk")
	return true
}

// diskStartTimeout is how long the disk server of a session may take to be
// ready. A restore point older than the newest export is read from the undo
// files and gets twice the start time of a COS session (10 minutes by default).
// When the policy cannot say, an asked-for restore point is counted as older.
func (bc *BackupController) diskStartTimeout(ctx context.Context, bb *v1alpha1.BackupBrowse) time.Duration {
	rp := bb.Spec.EffectiveRestorePoint()
	if rp == nil || rp.IsZero() {
		return bc.cosStartTimeout()
	}
	loc, err := bc.cosLocationOf(ctx, bb.Spec.Policy, bb.Spec.PVCNamespace, bb.Spec.PVCName, bb.Spec.FromCOS)
	if err == nil && loc.entry != nil && loc.entry.ExportedTime != nil && !rp.Time.Before(loc.entry.ExportedTime.Time) {
		return bc.cosStartTimeout()
	}
	return 2 * bc.cosStartTimeout()
}

func helperFailureText(reason string) string {
	if reason == "" {
		return "no reason was given"
	}
	if len(reason) > 600 {
		reason = reason[:600] + "..."
	}
	return reason
}
