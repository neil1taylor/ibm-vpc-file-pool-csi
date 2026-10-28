package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosrestore"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cossetup"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// `kubectl backup restore --from-cos` and `kubectl backup cos-list` read the
// backups of a policy from its COS bucket (docs/specs/2026-10-21-backup-cos-export.md,
// part 5). They run on a person's machine. They use the person's own IBM Cloud
// login for the bucket (to remove delete markers and to read the wrapped
// backup key) and the cluster for everything else: the Jobs that open the
// repository run in the driver's namespace and read the keys from Secrets.

// cosAdmin is what the commands do in the bucket with the caller's login.
type cosAdmin interface {
	RemoveDeleteMarkers(ctx context.Context, endpoint, bucket, prefix string) (int, error)
	ReadWrappedKey(ctx context.Context, endpoint, bucket string) (string, error)
}

// cosLister runs the Job that lists the repository.
type cosLister interface {
	ListCOS(ctx context.Context, key string, repo pool.ExportRepo, volume string) (bool, pool.COSListing, error)
}

// newCOSAdmin gives the real calls, made with the caller's login.
func newCOSAdmin() cosAdmin {
	httpClient := &http.Client{Timeout: 2 * time.Minute}
	eps := cossetup.DefaultEndpoints()
	return &cosrestore.Client{HTTP: httpClient, Token: cossetup.EnvironmentToken{HTTP: httpClient, IAM: eps.IAM}}
}

func driverNamespace(flags map[string]string) string {
	if ns := flags["driver-namespace"]; ns != "" {
		return ns
	}
	return pool.DefaultDriverNamespace
}

// cosSource reads the flags that say where the repository is and how its key is
// reached. keyFile is the path of a readable backup key, if one is given.
func cosSource(flags map[string]string) (block *v1alpha1.RestoreFromCOS, keyFile string, err error) {
	for _, f := range []string{"policy", "bucket", "endpoint"} {
		if flags[f] == "" {
			return nil, "", fmt.Errorf("--%s is required", f)
		}
	}
	block = &v1alpha1.RestoreFromCOS{
		Bucket: flags["bucket"], Endpoint: flags["endpoint"], Prefix: flags["prefix"],
		CredentialsSecret: orFlag(flags, "credentials-secret", "cos-credentials"),
	}
	keyFile = flags["backup-key-file"]
	if keyFile != "" {
		block.EmergencyKeyOnly = true
		return block, keyFile, nil
	}
	if err = keyProtectFlags(flags, block); err != nil {
		return nil, "", err
	}
	return block, "", nil
}

// keyProtectFlags reads the flags that say which Key Protect root key unwraps
// the backup key and which Secrets hold the wrapped key and the API key.
func keyProtectFlags(flags map[string]string, block *v1alpha1.RestoreFromCOS) error {
	for _, f := range []string{"kp-instance", "kp-region", "kp-root-key"} {
		if flags[f] == "" {
			return fmt.Errorf("--%s is required (or give a readable backup key instead, to use it without Key Protect)", f)
		}
	}
	block.BackupKeySecret = orFlag(flags, "backup-key-secret", "cos-backup-key")
	block.KeyProtect = &v1alpha1.KeyProtectRef{
		InstanceID: flags["kp-instance"], Region: flags["kp-region"], RootKeyID: flags["kp-root-key"],
		APIKeySecret: orFlag(flags, "kp-api-key-secret", "keyprotect-apikey"),
	}
	return nil
}

func orFlag(flags map[string]string, name, def string) string {
	if v := flags[name]; v != "" {
		return v
	}
	return def
}

// checkSecretsExist stops early, with the name of what is missing, before
// anything is changed.
func checkSecretsExist(ctx context.Context, c client.Client, ns string, names ...string) error {
	for _, name := range names {
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &corev1.Secret{}); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("the Secret %s/%s does not exist. `kubectl backup cos-setup` writes manifests for the COS key and the Key Protect API key; apply them first", ns, name)
			}
			return fmt.Errorf("reading Secret %s/%s: %w", ns, name, err)
		}
	}
	return nil
}

// ensureWrappedKeySecret makes the Secret with the wrapped backup key if the
// cluster has none, from the copy that cos-setup stored in the bucket.
func ensureWrappedKeySecret(ctx context.Context, c client.Client, out io.Writer, admin cosAdmin, ns string, b *v1alpha1.RestoreFromCOS) error {
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: b.BackupKeySecret}, &corev1.Secret{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading Secret %s/%s: %w", ns, b.BackupKeySecret, err)
	}
	wrapped, err := admin.ReadWrappedKey(ctx, b.Endpoint, b.Bucket)
	if err != nil {
		return err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: b.BackupKeySecret, Labels: map[string]string{"app.kubernetes.io/managed-by": "kubectl-backup"}},
		Data:       map[string][]byte{pool.ExportWrappedKeyKey: []byte(wrapped)},
	}
	if err = c.Create(ctx, s); err != nil {
		return fmt.Errorf("making Secret %s/%s: %w", ns, b.BackupKeySecret, err)
	}
	_, _ = fmt.Fprintf(out, "Made Secret %s/%s from the wrapped backup key in the bucket (it can only be unwrapped by Key Protect)\n", ns, b.BackupKeySecret)
	return nil
}

// makeEmergencySecret stores the readable backup key from a file in a Secret
// for the Jobs. The commands remove it when the restore has ended.
func makeEmergencySecret(ctx context.Context, c client.Client, ns, name, file string) error {
	raw, err := os.ReadFile(file) //nolint:gosec // the path is the person's own flag
	if err != nil {
		return fmt.Errorf("reading the backup key file: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return errors.New("the backup key file is empty")
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "kubectl-backup"}},
		Data:       map[string][]byte{pool.ExportEmergencyKeyKey: []byte(key)},
	}
	if err = c.Create(ctx, s); err != nil {
		return fmt.Errorf("making Secret %s/%s: %w", ns, name, err)
	}
	return nil
}

func deleteSecret(c client.Client, ns, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}})
}

// runRestoreFromCOS asks for a restore of one volume from a COS bucket.
func runRestoreFromCOS(ctx context.Context, c client.Client, out io.Writer, admin cosAdmin, flags map[string]string, now func() time.Time) error {
	block, keyFile, err := cosSource(flags)
	if err != nil {
		return err
	}
	for _, f := range []string{"volume", "to", "storage-class", "size-gb"} {
		if flags[f] == "" {
			return fmt.Errorf("--%s is required", f)
		}
	}
	size, err := strconv.ParseInt(flags["size-gb"], 10, 32)
	if err != nil || size < 1 {
		return fmt.Errorf("--size-gb must be a whole number of at least 1, got %q", flags["size-gb"])
	}
	block.Volume, block.SizeGB = flags["volume"], int32(size)
	dstNS, dstName, err := splitRef("to", flags["to"])
	if err != nil {
		return err
	}
	var moment *metav1.Time
	if m := flags["moment"]; m != "" {
		t, perr := time.Parse(time.RFC3339, m)
		if perr != nil {
			return fmt.Errorf("--moment must be an RFC 3339 time such as 2026-10-20T09:00:00Z: %w", perr)
		}
		mt := metav1.NewTime(t)
		moment = &mt
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
	name := restoreName(block.Volume, now())
	emergency := ""
	if keyFile != "" {
		emergency = name + "-key"
		block.EmergencyKeySecret = emergency
	}
	if err = block.Validate(); err != nil {
		return err
	}

	// Look before changing anything.
	ns := driverNamespace(flags)
	needed := []string{block.CredentialsSecret}
	if block.KeyProtect != nil {
		needed = append(needed, block.KeyProtect.APIKeySecret)
	}
	if err = checkSecretsExist(ctx, c, ns, needed...); err != nil {
		return err
	}
	if block.KeyProtect != nil {
		if err = ensureWrappedKeySecret(ctx, c, out, admin, ns, block); err != nil {
			return err
		}
	}
	if flags["skip-marker-cleanup"] != "true" {
		area := pool.ExportAreaPrefix(block.Prefix, flags["policy"])
		n, cerr := admin.RemoveDeleteMarkers(ctx, block.Endpoint, block.Bucket, area)
		if cerr != nil {
			return fmt.Errorf("removing delete markers in %s/%s: %w. If your login cannot reach the bucket, check it can (the bucket may allow only the cluster and the admin addresses), or restore without the clean-up with --skip-marker-cleanup", block.Bucket, area, cerr)
		}
		_, _ = fmt.Fprintf(out, "Removed %d delete marker(s) in %s/%s\n", n, block.Bucket, area)
	}

	wait := flags["wait"] == "true"
	if emergency != "" {
		// The Secret with the readable key must not outlive the restore.
		wait = true
		if err = makeEmergencySecret(ctx, c, ns, emergency, keyFile); err != nil {
			return err
		}
	}
	br := &v1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.BackupRestoreSpec{
			Policy: flags["policy"], Moment: moment, TargetNamespace: dstNS, TargetName: dstName,
			StorageClassName: flags["storage-class"], FromCOS: block,
		},
	}
	if err = c.Create(ctx, br); err != nil {
		if emergency != "" {
			deleteSecret(c, ns, emergency)
		}
		return fmt.Errorf("making the restore request: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Restore %s asked: volume %s from bucket %s into new PVC %s/%s\n", br.Name, block.Volume, block.Bucket, dstNS, dstName)
	if !wait {
		_, _ = fmt.Fprintf(out, "Follow it with: kubectl get backuprestore %s\n", br.Name)
		return nil
	}
	err = waitRestore(ctx, c, out, br.Name, poll, timeout)
	if emergency != "" {
		if ended(ctx, c, br.Name) {
			deleteSecret(c, ns, emergency)
		} else {
			err = fmt.Errorf("%w. The Secret %s/%s still holds the readable backup key for the running restore; remove it when the restore has ended: kubectl delete secret -n %s %s", err, ns, emergency, ns, emergency)
		}
	}
	return err
}

// ended says if the restore has reached Done or Failed.
func ended(ctx context.Context, c client.Client, name string) bool {
	br := &v1alpha1.BackupRestore{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, br); err != nil {
		return false
	}
	return br.Status.Phase == "Done" || br.Status.Phase == "Failed"
}

// findDriverImage returns the image of the controller container.
func findDriverImage(ctx context.Context, c client.Client, ns string) (string, error) {
	list := &appsv1.DeploymentList{}
	err := c.List(ctx, list, client.InNamespace(ns), client.MatchingLabels{
		"app.kubernetes.io/name": "ibm-vpc-file-pool-csi", "app.kubernetes.io/component": "controller"})
	if err != nil {
		return "", fmt.Errorf("looking for the driver's controller: %w", err)
	}
	for _, d := range list.Items {
		for _, ctr := range d.Spec.Template.Spec.Containers {
			if ctr.Name == "csi-controller" {
				return ctr.Image, nil
			}
		}
	}
	return "", fmt.Errorf("no driver controller found in namespace %s; pass --driver-image and --driver-namespace", ns)
}

// runCOSList lists the volumes and the export moments in the bucket of a
// policy. It runs a read-only Job in the driver's namespace; the Job opens the
// repository, so the cluster must be able to reach the bucket.
func runCOSList(ctx context.Context, c client.Client, out io.Writer, admin cosAdmin, newLister func(image string) cosLister, flags map[string]string) error {
	block, keyFile, err := cosSource(flags)
	if err != nil {
		return err
	}
	if keyFile != "" {
		return errors.New("--backup-key-file works with restore only: a listing uses Key Protect")
	}
	if flags["volume"] != "" {
		block.Volume = flags["volume"]
	}
	if err = block.ValidateSource(); err != nil {
		return err
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
	ns := driverNamespace(flags)
	if err = checkSecretsExist(ctx, c, ns, block.CredentialsSecret, block.KeyProtect.APIKeySecret); err != nil {
		return err
	}
	if err = ensureWrappedKeySecret(ctx, c, out, admin, ns, block); err != nil {
		return err
	}
	image := flags["driver-image"]
	if image == "" {
		if image, err = findDriverImage(ctx, c, ns); err != nil {
			return err
		}
	}
	repo, err := pool.COSRepoOf(block, flags["policy"])
	if err != nil {
		return err
	}
	lister := newLister(image)
	key := "coslist-" + strconv.FormatInt(time.Now().Unix(), 36)
	var listing pool.COSListing
	if err = waitJob(ctx, poll, timeout, "listing the bucket", func() (bool, error) {
		done, l, lerr := lister.ListCOS(ctx, key, repo, flags["volume"])
		listing = l
		return done, lerr
	}); err != nil {
		return err
	}
	if len(listing.Volumes) == 0 {
		_, _ = fmt.Fprintf(out, "No backups of policy %s found in bucket %s\n", flags["policy"], block.Bucket)
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "VOLUME\tMOMENT")
	for _, v := range listing.Volumes {
		for _, m := range v.Moments {
			_, _ = fmt.Fprintf(w, "%s\t%s\n", v.Name, m.UTC().Format(time.RFC3339))
		}
		if len(v.Moments) == 0 {
			_, _ = fmt.Fprintf(w, "%s\t-\n", v.Name)
		}
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if listing.Truncated {
		_, _ = fmt.Fprintln(out, "The list is cut: there are more moments. Add --volume pvc-... to see the moments of one volume.")
	}
	return nil
}

// newCOSLister gives the real list Job runner.
func newCOSLister(c client.Client, namespace string) func(image string) cosLister {
	return func(image string) cosLister {
		r := pool.NewShareJobRunner(c, namespace, "")
		r.SetDriverImage(image)
		return r
	}
}
