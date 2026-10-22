// Command backup is a CLI tool (and kubectl plugin) for the backups that a
// BackupPolicy makes.
//
// Usage:
//
//	kubectl backup list    [--policy NAME]
//	kubectl backup restore --policy NAME --pvc NAMESPACE/NAME --to NAMESPACE/NAME [--moment TIME] [--storage-class CLASS] [--wait]
//	kubectl backup delete  --policy NAME --pvc NAMESPACE/NAME [--yes]
//	kubectl backup cos-setup --region R --resource-group G --bucket B --root-key CRN ... [--dry-run]
//	kubectl backup restore --from-cos --policy NAME --volume pvc-... --to NAMESPACE/NAME --bucket B --endpoint HOST --storage-class CLASS --size-gb N ...
//	kubectl backup cos-list --policy NAME --bucket B --endpoint HOST ... [--volume pvc-...]
//
// A restore makes a BackupRestore resource. The driver makes the new PVC and
// copies the backup into it; it never writes over a volume that exists.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var version = "dev"

const (
	defaultPoll    = 5 * time.Second
	defaultTimeout = 2 * time.Hour
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "version":
		fmt.Printf("kubectl-backup %s\n", version)
		return
	case "--help", "-h", "help":
		printUsage(os.Stdout)
		return
	case "cos-setup":
		// It needs the caller's IBM Cloud login, not a cluster.
		if err := runCOSSetup(context.Background(), os.Stdout, os.Stdin, stdinIsTerminal(), parseFlags(os.Args[2:])); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	case "ls", "get", "browse":
		// They talk to the backup gateway with the caller's own token.
		if err := runBrowseCommand(context.Background(), os.Args[1], os.Stdout, os.Args[2:]); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	case "list", "restore", "delete", "cos-list":
	default:
		_, _ = fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n\n", os.Args[1])
		printUsage(os.Stderr)
		os.Exit(1)
	}
	flags := parseFlags(os.Args[2:])
	c, err := buildClient(flags["kubeconfig"])
	switch {
	case err != nil:
	case os.Args[1] == "delete":
		namespace := flags["driver-namespace"]
		if namespace == "" {
			namespace = pool.DefaultDriverNamespace
		}
		err = runDelete(context.Background(), c, os.Stdout, os.Stdin, stdinIsTerminal(), pool.NewShareJobRunner(c, namespace, ""), flags)
	case os.Args[1] == "cos-list":
		err = runCOSList(context.Background(), c, os.Stdout, newCOSAdmin(), newCOSLister(c, driverNamespace(flags)), flags)
	case os.Args[1] == "restore" && flags["from-cos"] == "true":
		err = runRestoreFromCOS(context.Background(), c, os.Stdout, newCOSAdmin(), flags, time.Now)
	default:
		err = run(context.Background(), c, os.Stdout, os.Args[1:], time.Now)
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// stdinIsTerminal says if a person can type an answer.
func stdinIsTerminal() bool { return isTerminal(os.Stdin) }

// isTerminal says if f is a terminal. A character device is not enough:
// /dev/null is one, and scripts and CI often run with it as input.
func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `kubectl-backup — backups of volumes and VMs

Usage:
  kubectl backup list    [--policy NAME]            List the backed-up volumes and their moments
  kubectl backup ls  [flags] [PATH]                 List a folder inside a backup (your own rights; see below)
  kubectl backup get [flags] PATH                   Download a file, or a folder as a tar, from a backup
  kubectl backup browse [flags]                     Open a browse session by hand, or --close NAME
  kubectl backup restore [flags]                    Make a new PVC from a backup
  kubectl backup delete  [flags]                    Remove all backups of one volume, for good
  kubectl backup cos-setup [flags]                  Make the COS bucket, keys and lock for the export (your own IBM Cloud login)
  kubectl backup cos-list [flags]                   List the volumes and moments in the COS bucket of a policy
  kubectl backup restore --from-cos [flags]         Make a new PVC from a backup in COS, in any cluster that runs the driver
  kubectl backup version                            Print version

ls, get and browse flags (they use your own kubeconfig token, and need the right "get backupfiles" in the namespace of the claim):
  --policy          The BackupPolicy that holds the backup
  --pvc             The backed-up claim, as NAMESPACE/NAME
  --moment          A time like 2026-10-21T09:30:00Z: the newest backup at or before it. Default: the latest copy
  --moments         ls only: list the moments that can be browsed
  --session         Use an open session by name (from browse) instead of --policy and --pvc
  --out             get only: a folder or file name, or - for standard output. A file is never replaced
  --extract         get only: unpack a folder into this folder. Links and special files are skipped
  --gateway, --gateway-ca-file   The gateway address and its CA, if not found in the cluster
  --driver-namespace   Where the driver runs (default kube-system)
  --timeout         How long to wait for a session to be ready (default 5m)
  A name that is not text is shown as \xNN, and you can type it the same way. Sessions end after 15 minutes without use.

Delete flags:
  --policy          The BackupPolicy that holds the backup (required)
  --pvc             The backed-up claim, as NAMESPACE/NAME (required)
  --yes             Do not ask for a typed confirmation. Required in a non-interactive shell
  --pool, --volume  Only when the policy is deleted: the snapshot pool, and the volume name (pvc-...)
  --share           Only when the policy is deleted and the pool has more than one share: the share ID
  --driver-namespace  Where the driver runs its Jobs (default kube-system)
  --timeout         How long each Job may take (default 2h)
  The policy must no longer back up the volume (remove the label first), and no
  restore of the volume may be running. It removes the volume's backup folder and
  its earlier moments from the snapshot pool's share, after showing how much.

cos-setup flags (run on your machine; it needs no cluster; it deletes only the instance-wide policy that IBM Cloud adds for the new HMAC key):
  --region, --resource-group, --bucket   Where to make the bucket (required)
  --root-key        CRN of the Key Protect root key that wraps the backup key (required; not made here)
  --vpc-crn, --admin-address   The cluster VPC and your admin addresses (comma separated), for a new CBR zone.
                    Your own address must be in the list. Or use --zone-id for an existing zone
  --cbr-mode        report or enabled (default enabled; refused if your address is not in the zone)
  --key-file        Where to write the COS key as a Secret manifest (mode 0600, never printed)
  --secret-file     Where to write the wrapped backup key as a Secret manifest
  --keep-for, --lock, --prefix, --endpoint   Go into the printed spec.export block (keep-for default 30d, lock default Compliance)
  --instance        COS instance name (default pool-csi-backup-cos; reused if it exists)
  --existing-bucket Use a bucket this command did not make (never changed; needs versioning and object lock)
  --my-address      Your public address, when it cannot be found
  --dry-run         Show each step and the exact objects, change nothing
  --yes             Skip the typed confirmation. A keep time over 90 days needs --confirm-keep-for=VALUE
  --namespace, --credentials-secret, --backup-key-secret   Names in the Secret manifests
  Login: IBMCLOUD_IAM_TOKEN, or IBMCLOUD_API_KEY, or your ibmcloud CLI login. Never stored or printed.

Restore flags:
  --policy         The BackupPolicy that holds the backup (required)
  --pvc             The backed-up claim, as NAMESPACE/NAME (required). It may be gone
  --to              The new PVC, as NAMESPACE/NAME (required). It must not exist
  --moment          Restore the newest backup at or before this time (RFC 3339). Default: the latest
  --storage-class   Class of the new PVC. Default: the class of the backed-up claim
  --wait            Wait until the restore is done, and report the moment that was restored
  --timeout         How long --wait waits (default 2h)

Restore from COS (restore --from-cos). It needs no BackupPolicy and no snapshot pool in this cluster:
  --policy, --bucket, --endpoint   The policy's area in the bucket, and where the bucket is (required)
  --prefix          The prefix set in the policy's spec.export (default: none)
  --volume          The backed-up volume, pvc-<uuid>, as cos-list shows it (required). COS does not keep claim names
  --to              The new PVC, as NAMESPACE/NAME (required). It must not exist
  --storage-class, --size-gb   Class and size of the new PVC (required): COS does not keep them
  --moment          Restore the newest backup at or before this time (RFC 3339). Default: the latest
  --credentials-secret   Secret with the COS key in the driver namespace (default cos-credentials)
  --backup-key-secret    Secret with the wrapped backup key (default cos-backup-key). Made from the bucket if missing
  --kp-instance, --kp-region, --kp-root-key   The Key Protect root key that wraps the backup key (required)
  --kp-api-key-secret    Secret with the IBM Cloud API key for Key Protect (default keyprotect-apikey)
  --backup-key-file      Use this readable backup key and never call Key Protect (for when Key Protect is gone).
                    Its Secret is removed when the restore ends; the command waits for that
  --skip-marker-cleanup  Do not remove delete markers in the policy's area (this needs your IBM Cloud login)
  --driver-namespace, --wait, --timeout   As for the other restore
  The command removes delete markers with your own IBM Cloud login (IBMCLOUD_IAM_TOKEN, IBMCLOUD_API_KEY or the ibmcloud CLI login).
  cos-list takes the same source flags and --volume (optional) and --driver-image (default: the controller's image).
  It runs a read-only Job in the cluster, so the cluster must be able to reach the bucket

All commands:
  --kubeconfig      Path to kubeconfig (default: ~/.kube/config)
`)
}

// parseFlags handles --key=value and --key value; --wait takes no value.
func parseFlags(args []string) map[string]string {
	flags := make(map[string]string)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		arg = strings.TrimLeft(arg, "-")
		if idx := strings.Index(arg, "="); idx >= 0 {
			flags[arg[:idx]] = arg[idx+1:]
			continue
		}
		if arg == "wait" || arg == "yes" || arg == "dry-run" || arg == "existing-bucket" || arg == "from-cos" || arg == "skip-marker-cleanup" || arg == "moments" {
			flags[arg] = "true"
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags[arg] = args[i+1]
			i++
		} else {
			flags[arg] = "true"
		}
	}
	return flags
}

func buildClient(kubeconfig string) (client.Client, error) {
	// Without --kubeconfig, use the KUBECONFIG variable, then ~/.kube/config, as kubectl does.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
	}
	return client.New(config, client.Options{Scheme: newScheme()})
}

// newScheme holds every type the backup command reads or writes.
func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	return scheme
}

// run does one command. args[0] is the subcommand.
func run(ctx context.Context, c client.Client, out io.Writer, args []string, now func() time.Time) error {
	flags := parseFlags(args[1:])
	switch args[0] {
	case "list":
		return runList(ctx, c, out, flags["policy"])
	case "restore":
		return runRestore(ctx, c, out, flags, now)
	}
	return fmt.Errorf("unknown subcommand %q", args[0])
}

func formatTime(t *metav1.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func runList(ctx context.Context, c client.Client, out io.Writer, policy string) error {
	list := &v1alpha1.BackupPolicyList{}
	if err := c.List(ctx, list); err != nil {
		return fmt.Errorf("listing backup policies: %w", err)
	}
	var policies []v1alpha1.BackupPolicy
	for _, p := range list.Items {
		if policy == "" || p.Name == policy {
			policies = append(policies, p)
		}
	}
	if policy != "" && len(policies) == 0 {
		return fmt.Errorf("backup policy %q does not exist", policy)
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "POLICY\tVOLUME\tVM\tLAST BACKUP\tOLDEST\tPOINTS\tCONSISTENCY\tSTATE\tERROR")
	for _, p := range policies {
		if p.Status.Phase == "Error" || p.Status.Message != "" {
			_, _ = fmt.Fprintf(w, "%s\t(policy %s)\t\t\t\t\t\t\t%s\n", p.Name, strings.ToLower(p.Status.Phase), p.Status.Message)
		}
		for _, v := range p.Status.Volumes {
			state := "active"
			if v.Retained {
				state = "retained"
			}
			_, _ = fmt.Fprintf(w, "%s\t%s/%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", p.Name, v.PVCNamespace, v.PVCName, dash(v.VMName),
				formatTime(v.LastBackupTime), formatTime(v.OldestBackupTime), v.RestorePoints, dash(v.Consistency), state, v.LastError)
		}
	}
	return w.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// splitRef splits NAMESPACE/NAME.
func splitRef(flag, v string) (ns, name string, err error) {
	ns, name, ok := strings.Cut(v, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("--%s must be NAMESPACE/NAME, got %q", flag, v)
	}
	return ns, name, nil
}

func runRestore(ctx context.Context, c client.Client, out io.Writer, flags map[string]string, now func() time.Time) error {
	policy := flags["policy"]
	if policy == "" {
		return fmt.Errorf("--policy is required")
	}
	if flags["pvc"] == "" {
		return fmt.Errorf("--pvc is required")
	}
	if flags["to"] == "" {
		return fmt.Errorf("--to is required")
	}
	srcNS, srcName, err := splitRef("pvc", flags["pvc"])
	if err != nil {
		return err
	}
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

	// Look before asking, so a wrong name gives an answer now, not a failed
	// request later.
	bp := &v1alpha1.BackupPolicy{}
	if err = c.Get(ctx, types.NamespacedName{Name: policy}, bp); err != nil {
		return fmt.Errorf("backup policy %q: %w", policy, err)
	}
	found := false
	for _, v := range bp.Status.Volumes {
		if v.PVCNamespace == srcNS && v.PVCName == srcName {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("policy %s has no backup of %s/%s; `kubectl backup list --policy %s` shows what it has", policy, srcNS, srcName, policy)
	}

	br := &v1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: restoreName(srcName, now())},
		Spec: v1alpha1.BackupRestoreSpec{
			Policy: policy, SourceNamespace: srcNS, SourceName: srcName, Moment: moment,
			TargetNamespace: dstNS, TargetName: dstName, StorageClassName: flags["storage-class"],
		},
	}
	if err = c.Create(ctx, br); err != nil {
		return fmt.Errorf("making the restore request: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Restore %s asked: %s/%s from policy %s into new PVC %s/%s\n", br.Name, srcNS, srcName, policy, dstNS, dstName)
	if flags["wait"] != "true" {
		_, _ = fmt.Fprintf(out, "Follow it with: kubectl get backuprestore %s\n", br.Name)
		return nil
	}
	return waitRestore(ctx, c, out, br.Name, poll, timeout)
}

// restoreName makes the name of a request: restore-<claim>-<time in base 36>.
func restoreName(claim string, t time.Time) string {
	name := "restore-" + claim + "-" + strconv.FormatInt(t.Unix(), 36)
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-.")
}

func waitRestore(ctx context.Context, c client.Client, out io.Writer, name string, poll, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	lastPhase := ""
	for {
		br := &v1alpha1.BackupRestore{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, br); err != nil {
			return fmt.Errorf("reading the restore: %w", err)
		}
		if br.Status.Phase != lastPhase {
			lastPhase = br.Status.Phase
			_, _ = fmt.Fprintf(out, "  %s %s\n", dash(br.Status.Phase), br.Status.Message)
		}
		switch br.Status.Phase {
		case "Done":
			moment := "an unknown moment (the first backup)"
			if br.Status.RestoredMoment != nil {
				moment = formatTime(br.Status.RestoredMoment)
			}
			_, _ = fmt.Fprintf(out, "Done: %s/%s holds the backup of %s\n", br.Spec.TargetNamespace, br.Spec.TargetName, moment)
			return nil
		case "Failed":
			return fmt.Errorf("the restore failed: %s", br.Status.Message)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s; the restore goes on: kubectl get backuprestore %s", timeout, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// runBrowseCommand runs ls, get or browse through the gateway.
func runBrowseCommand(ctx context.Context, cmd string, out io.Writer, args []string) error {
	gw, err := newGatewayClient(ctx, parseBrowseArgs(args))
	if err != nil {
		return err
	}
	switch cmd {
	case "ls":
		return runLs(ctx, gw, out, args)
	case "get":
		return runGet(ctx, gw, out, args)
	}
	return runBrowse(ctx, gw, out, args)
}
