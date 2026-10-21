package cossetup

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// Options are the choices of the person who runs the command.
type Options struct {
	Region                             string // region of the bucket and of Key Protect, such as eu-de
	ResourceGroup                      string
	InstanceName                       string // COS instance; reused when it exists
	Bucket                             string
	ExistingBucket                     bool // allow a bucket this command did not make
	RootKeyCRN                         string
	ZoneID                             string   // use this CBR zone
	VPCCRN                             string   // cluster VPC, for a new zone
	AdminAddresses                     []string // addresses for a new zone
	MyAddress                          string   // the caller's address, when it cannot be found
	CBRMode                            string   // report or enabled
	KeepFor                            string   // the keepFor of the printed spec.export block
	Lock                               string   // Compliance (default) or None
	Prefix                             string
	ExportEndpoint                     string // the endpoint of the printed block
	Namespace                          string // namespace of the Secrets
	CredentialsSecret, BackupKeySecret string
	KeyFile, SecretFile                string
	DryRun, Yes                        bool
	ConfirmKeepFor                     string
}

// Setup runs the command against the IBM Cloud APIs.
type Setup struct {
	HTTP        Doer
	Token       TokenSource
	Endpoints   Endpoints
	Address     AddressFinder
	Rand        io.Reader // source of the backup key; crypto/rand when nil
	In          io.Reader
	Out         io.Writer
	Interactive bool // a person can type answers
}

var bucketNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

const (
	savedPhrase     = "I saved the backup key"
	longKeep        = 90 * 24 * time.Hour
	cosStandardPlan = "744bfc56-d12c-4866-88d5-dac9139e0e5d"
)

// kpKey is a Key Protect root key CRN taken apart.
type kpKey struct{ service, region, instance, id string }

func parseRootKey(crn string) (kpKey, error) {
	p := strings.Split(crn, ":")
	if crn == "" {
		return kpKey{}, fmt.Errorf("--root-key (the CRN of the Key Protect root key that wraps the backup key) is required; cos-setup does not make Key Protect instances or root keys")
	}
	if len(p) < 10 || p[0] != "crn" || (p[4] != "kms" && p[4] != "hs-crypto") || p[8] != "key" || p[5] == "" || p[7] == "" || p[9] == "" {
		return kpKey{}, fmt.Errorf("--root-key must be the CRN of a Key Protect root key (crn:v1:bluemix:public:kms:REGION:a/ACCOUNT:INSTANCE:key:KEYID), got %q", crn)
	}
	return kpKey{service: p[4], region: p[5], instance: p[7], id: p[9]}, nil
}

func (o *Options) defaults() {
	if o.InstanceName == "" {
		o.InstanceName = "pool-csi-backup-cos"
	}
	if o.CredentialsSecret == "" {
		o.CredentialsSecret = "cos-credentials"
	}
	if o.BackupKeySecret == "" {
		o.BackupKeySecret = "cos-backup-key"
	}
	if o.Namespace == "" {
		o.Namespace = "kube-system"
	}
	if o.Lock == "" {
		o.Lock = v1alpha1.ExportLockCompliance
	}
	if o.KeepFor == "" {
		o.KeepFor = "30d"
	}
	if o.CBRMode == "" {
		o.CBRMode = "enabled"
	}
	if o.ExportEndpoint == "" {
		o.ExportEndpoint = "s3." + o.Region + ".cloud-object-storage.appdomain.cloud"
	}
}

func (o *Options) export() *v1alpha1.BackupExport {
	return &v1alpha1.BackupExport{
		Bucket: o.Bucket, Endpoint: o.ExportEndpoint, Prefix: o.Prefix,
		CredentialsSecret: o.CredentialsSecret, BackupKeySecret: o.BackupKeySecret,
		KeepFor: o.KeepFor, Lock: o.Lock,
	}
}

func (o *Options) validate() (time.Duration, error) {
	switch {
	case o.Region == "":
		return 0, fmt.Errorf("--region is required (the region of the bucket, such as eu-de)")
	case o.ResourceGroup == "":
		return 0, fmt.Errorf("--resource-group is required")
	case !bucketNameRE.MatchString(o.Bucket):
		return 0, fmt.Errorf("--bucket must be 3 to 63 lowercase letters, digits, dots or hyphens, got %q", o.Bucket)
	case o.CBRMode != "report" && o.CBRMode != "enabled":
		return 0, fmt.Errorf("--cbr-mode must be report or enabled, got %q", o.CBRMode)
	case !o.DryRun && (o.KeyFile == "" || o.SecretFile == ""):
		return 0, fmt.Errorf("--key-file (for the COS key) and --secret-file (for the wrapped backup key) are required")
	case o.ZoneID == "" && o.VPCCRN == "":
		return 0, fmt.Errorf("give --zone-id (a CBR zone that holds the cluster and your address), or --vpc-crn and --admin-address to make one")
	case o.ZoneID == "" && len(o.AdminAddresses) == 0:
		return 0, fmt.Errorf("--admin-address is required to make a zone: it must include your own address, or you lose access to the bucket from this machine")
	}
	if err := o.export().Validate(); err != nil {
		return 0, fmt.Errorf("--keep-for/--lock: %w", err)
	}
	keep, _ := o.export().EffectiveKeepFor()
	return keep, nil
}

// Run does the setup. It reads first, shows what it would make, asks for a
// confirmation, and only then makes it. It never deletes anything.
func (s *Setup) Run(ctx context.Context, o Options) error {
	o.defaults()
	if _, err := parseRootKey(o.RootKeyCRN); err != nil {
		return err
	}
	keep, err := o.validate()
	if err != nil {
		return err
	}
	token, err := s.Token.Token(ctx)
	if err != nil {
		return err
	}
	acct, err := accountID(token)
	if err != nil {
		return err
	}
	in := bufio.NewReader(s.In)
	if s.In == nil {
		in = bufio.NewReader(strings.NewReader(""))
	}
	newRun := func(apply bool) *run {
		return &run{s: s, o: o, token: token, acct: acct, apply: apply, in: in, out: s.Out}
	}

	_, _ = fmt.Fprintf(s.Out, "Plan for bucket %s (region %s, account %s):\n", o.Bucket, o.Region, acct)
	plan := newRun(false)
	if err = plan.all(ctx); err != nil {
		return err
	}
	if o.DryRun {
		_, _ = fmt.Fprintf(s.Out, "\nDry run: nothing was changed. %d object(s) would be made.\n", plan.creates)
		return nil
	}
	if plan.creates > 0 {
		if err = s.confirm(plan, &o, keep, in); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(s.Out, "\nMaking it:")
		if err = newRun(true).all(ctx); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(s.Out, "\nDone.")
	} else {
		_, _ = fmt.Fprintln(s.Out, "\nNothing to change: everything is already there.")
	}
	return printExport(s.Out, &o)
}

// confirm asks for the typed answers before anything is made.
func (s *Setup) confirm(plan *run, o *Options, keep time.Duration, in *bufio.Reader) error {
	if !o.Yes && !s.Interactive {
		return fmt.Errorf("this is not an interactive shell, so a typed confirmation is not possible; add --yes to confirm")
	}
	if plan.keyNeeded && !s.Interactive {
		return fmt.Errorf("the readable backup key must be shown to a person: run this in an interactive shell")
	}
	if keep > longKeep && o.Lock == v1alpha1.ExportLockCompliance && o.ConfirmKeepFor != o.KeepFor {
		if !s.Interactive {
			return fmt.Errorf("a keep time of %s is longer than 90 days and a Compliance lock cannot be undone, even by IBM; add --confirm-keep-for=%s to confirm", o.KeepFor, o.KeepFor)
		}
		phrase := "keep for " + o.KeepFor
		_, _ = fmt.Fprintf(s.Out, "\nThe keep time %s is longer than 90 days. A Compliance lock cannot be shortened or removed, by anyone, and COS bills the data until it ends.\nType %q to confirm: ", o.KeepFor, phrase)
		if line, _ := in.ReadString('\n'); strings.TrimSpace(line) != phrase {
			return fmt.Errorf("not confirmed: the keep time %s was not accepted and nothing was made", o.KeepFor)
		}
	}
	if !o.Yes {
		_, _ = fmt.Fprintf(s.Out, "\nThis makes %d object(s) in your IBM Cloud account.\nType the bucket name %s to confirm: ", plan.creates, o.Bucket)
		if line, _ := in.ReadString('\n'); strings.TrimSpace(line) != o.Bucket {
			return fmt.Errorf("not confirmed: nothing was made")
		}
	}
	return nil
}

func printExport(out io.Writer, o *Options) error {
	block, err := yaml.Marshal(map[string]any{"spec": map[string]any{"export": o.export()}})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\nPaste this into your BackupPolicy:\n\n%s\n", indent(string(block), "  "))
	_, _ = fmt.Fprintf(out, "Then make the two Secrets in the driver namespace:\n  kubectl apply -f %s -f %s\n", o.KeyFile, o.SecretFile)
	_, _ = fmt.Fprintf(out, "The COS key file holds a secret: keep it private and delete it after you apply it.\n")
	_, _ = fmt.Fprintf(out, "For real backups, also make a virtual private endpoint gateway for COS in the cluster's VPC (TUTORIAL-09-COS-BACKUP-BUCKET.md).\n")
	return nil
}

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

func (s *Setup) random() io.Reader {
	if s.Rand != nil {
		return s.Rand
	}
	return rand.Reader
}

func newBackupKey(r io.Reader) (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("making the backup key: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}
