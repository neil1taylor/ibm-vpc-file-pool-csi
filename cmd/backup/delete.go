package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// backupRunner runs Jobs on the snapshot pool's share. The driver's own
// controller has no way to call these: only this command does, after a person
// has confirmed.
type backupRunner interface {
	MeasureBackup(ctx context.Context, key string, share pool.ShareMount, backupPath, momentsPath string) (bool, int64, error)
	RemoveBackup(ctx context.Context, key string, share pool.ShareMount, backupPath, momentsPath string) (bool, error)
}

// deleteTarget is the backup of one volume on a snapshot pool.
type deleteTarget struct {
	policy, pool    string
	namespace, name string
	subPath         string
	backup, moments string
}

// runDelete removes the backup folder and the moments folder of one volume
// after a confirmation. interactive says if a person can type an answer.
func runDelete(ctx context.Context, c client.Client, out io.Writer, in io.Reader, interactive bool, runner backupRunner, flags map[string]string) error {
	policy := flags["policy"]
	if policy == "" {
		return fmt.Errorf("--policy is required")
	}
	if flags["pvc"] == "" {
		return fmt.Errorf("--pvc is required")
	}
	ns, name, err := splitRef("pvc", flags["pvc"])
	if err != nil {
		return err
	}
	yes := flags["yes"] == "true"
	if !yes && !interactive {
		return fmt.Errorf("this is not an interactive shell, so a typed confirmation is not possible; add --yes to confirm")
	}
	timeout, poll := defaultTimeout, defaultPoll
	if v := flags["timeout"]; v != "" {
		if timeout, err = time.ParseDuration(v); err != nil {
			return fmt.Errorf("--timeout: %w", err)
		}
	}
	if v := flags["poll"]; v != "" {
		if poll, err = time.ParseDuration(v); err != nil {
			return fmt.Errorf("--poll: %w", err)
		}
	}

	t, err := resolveDeleteTarget(ctx, c, policy, ns, name, flags)
	if err != nil {
		return err
	}
	if err = util.ValidateBackupPath(t.backup); err != nil {
		return err
	}
	if err = util.ValidateBackupMomentsPath(t.moments); err != nil {
		return err
	}
	if err = refuseWhileRestoring(ctx, c, t); err != nil {
		return err
	}
	share, err := snapshotPoolShare(ctx, c, t.pool)
	if err != nil {
		return err
	}

	key := strings.TrimPrefix(t.subPath, "/pvcs/")
	var size int64
	if err = waitJob(ctx, poll, timeout, "measuring the backup", func() (bool, error) {
		done, n, merr := runner.MeasureBackup(ctx, "bkmeasure-"+key, share, t.backup, t.moments)
		size = n
		return done, merr
	}); err != nil {
		return err
	}
	if size == 0 {
		_, _ = fmt.Fprintf(out, "Nothing to remove: %s and %s are empty or do not exist on the share of pool %s.\n", t.backup, t.moments, t.pool)
		return nil
	}

	_, _ = fmt.Fprintf(out, "This removes all backups of %s/%s from policy %s, for good:\n  %s\n  %s\n  %s on the share of pool %s\n",
		ns, name, policy, t.backup, t.moments, humanBytes(size), t.pool)
	if !yes {
		_, _ = fmt.Fprintf(out, "Type %s/%s to confirm: ", ns, name)
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if strings.TrimSpace(answer) != ns+"/"+name {
			return fmt.Errorf("not confirmed: nothing was removed")
		}
	}

	if err = waitJob(ctx, poll, timeout, "removing the backup", func() (bool, error) {
		return runner.RemoveBackup(ctx, "bkdelete-"+key, share, t.backup, t.moments)
	}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Removed the backups of %s/%s (%s).\n", ns, name, humanBytes(size))
	return nil
}

// resolveDeleteTarget finds the folders of a volume's backup. While the policy
// exists, it must say it no longer backs up the volume. Once the policy is
// deleted, the caller names the pool and the volume.
func resolveDeleteTarget(ctx context.Context, c client.Client, policy, ns, name string, flags map[string]string) (*deleteTarget, error) {
	t := &deleteTarget{policy: policy, namespace: ns, name: name}
	bp := &v1alpha1.BackupPolicy{}
	err := c.Get(ctx, types.NamespacedName{Name: policy}, bp)
	switch {
	case apierrors.IsNotFound(err):
		if flags["pool"] == "" || flags["volume"] == "" {
			return nil, fmt.Errorf("backup policy %q does not exist, so its status cannot say where the backup is; add --pool and --volume (the snapshot pool and the volume name pvc-...)", policy)
		}
		t.pool, t.subPath = flags["pool"], "/pvcs/"+flags["volume"]
	case err != nil:
		return nil, fmt.Errorf("backup policy %q: %w", policy, err)
	default:
		var entry *v1alpha1.BackupVolumeStatus
		for i := range bp.Status.Volumes {
			if v := &bp.Status.Volumes[i]; v.PVCNamespace == ns && v.PVCName == name {
				entry = v
			}
		}
		if entry == nil || entry.SubPath == "" {
			return nil, fmt.Errorf("policy %s has no backup of %s/%s; `kubectl backup list --policy %s` shows what it has", policy, ns, name, policy)
		}
		if !entry.Retained {
			return nil, fmt.Errorf("policy %s still backs up %s/%s: remove the label that selects it, or delete the policy, wait for the next backup cycle, then run this again (after the policy is deleted, add --pool %s --volume %s)",
				policy, ns, name, bp.Spec.SnapshotPool, strings.TrimPrefix(entry.SubPath, "/pvcs/"))
		}
		t.pool, t.subPath = bp.Spec.SnapshotPool, entry.SubPath
	}
	t.backup = "/backup/" + policy + t.subPath
	t.moments = "/backup/" + policy + "/.moments" + t.subPath
	return t, nil
}

// refuseWhileRestoring stops the delete while a restore of the volume has not
// ended: it reads the folders that would be removed.
func refuseWhileRestoring(ctx context.Context, c client.Client, t *deleteTarget) error {
	list := &v1alpha1.BackupRestoreList{}
	if err := c.List(ctx, list); err != nil {
		return fmt.Errorf("listing restores: %w", err)
	}
	for _, br := range list.Items {
		sp := br.Spec
		if sp.Policy != t.policy || sp.SourceNamespace != t.namespace || sp.SourceName != t.name {
			continue
		}
		if br.Status.Phase != "Done" && br.Status.Phase != "Failed" {
			return fmt.Errorf("restore %s of %s/%s has not ended; wait for it, then run this again", br.Name, t.namespace, t.name)
		}
	}
	return nil
}

// snapshotPoolShare returns the share of the snapshot pool.
func snapshotPoolShare(ctx context.Context, c client.Client, poolName string) (pool.ShareMount, error) {
	p := &v1alpha1.FileSharePool{}
	if err := c.Get(ctx, types.NamespacedName{Name: poolName}, p); err != nil {
		return pool.ShareMount{}, fmt.Errorf("the snapshot pool %q: %w", poolName, err)
	}
	if len(p.Status.Shares) != 1 || p.Status.Shares[0].MountTargetIP == "" {
		return pool.ShareMount{}, fmt.Errorf("the snapshot pool %q must have exactly one share with a mount target, it has %d share(s) and the first is not ready", poolName, len(p.Status.Shares))
	}
	sh := p.Status.Shares[0]
	return pool.ShareMount{MountTargetIP: sh.MountTargetIP, ExportPath: sh.ExportPath}, nil
}

// waitJob calls step until it reports done, an error, or the time is up.
func waitJob(ctx context.Context, poll, timeout time.Duration, what string, step func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := step()
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s %s; the Job goes on in the driver namespace", timeout, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
