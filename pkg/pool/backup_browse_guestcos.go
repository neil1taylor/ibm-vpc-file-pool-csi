package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
func (bc *BackupController) checkGuestKeep(loc cosLocation) error {
	if loc.entry == nil || loc.entry.ExportedTime == nil || loc.repo.KeepFor <= 0 {
		return nil
	}
	remaining := loc.entry.ExportedTime.Add(loc.repo.KeepFor).Sub(bc.nowFunc())
	need := bc.browse.MaxLife + guestKeepMargin
	if remaining < need {
		return restoreFailure(fmt.Sprintf(
			"this backup will be deleted from COS in %s, which is less than a session may last (%s) plus an hour. Browse a newer backup",
			remaining.Round(time.Minute), bc.browse.MaxLife))
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
	if err := bc.checkGuestKeep(loc); err != nil {
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
	return bc.browseJobs.(BrowseDiskJobs).StartBrowseDisk(ctx, req)
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

func helperFailureText(reason string) string {
	if reason == "" {
		return "no reason was given"
	}
	if len(reason) > 600 {
		reason = reason[:600] + "..."
	}
	return reason
}
