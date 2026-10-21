package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ExportToCOS and MaintainCOSRepo are in ShareData: the backup controller starts
// them (see docs/specs/2026-10-21-backup-cos-export.md).

// Names of the keys inside the Secrets that an export Job reads. The COS key is
// the HMAC key pair of an IBM Cloud Object Storage service credential.
const (
	ExportAccessKeyKey = "access_key_id"
	ExportSecretKeyKey = "secret_access_key"
	ExportBackupKeyKey = "backupKey"
)

// ExportLockMode is how long COS must keep exported data.
type ExportLockMode string

const (
	// ExportLockCompliance sets an object lock that nobody can shorten.
	ExportLockCompliance ExportLockMode = "Compliance"
	// ExportLockNone sets no object lock.
	ExportLockNone ExportLockMode = "None"
)

const (
	exportUser = "driver"
	exportHost = "pool-csi"
)

var (
	exportEndpointPattern = regexp.MustCompile(`^(https?://)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	exportBucketPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	exportPrefixPattern   = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)
	secretNamePattern     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

// ExportRepo says where the Kopia repository of a backup policy is and how it
// is protected. The keys are not here: the Job reads them from Secrets.
type ExportRepo struct {
	// Endpoint is the S3 host of the bucket, with an optional port. A leading
	// http:// turns TLS off (for tests); https:// is the default.
	Endpoint string
	Bucket   string
	// Prefix is the area of one policy inside the bucket.
	Prefix string
	// CredentialsSecret holds the COS HMAC key (ExportAccessKeyKey and
	// ExportSecretKeyKey) in the driver's namespace.
	CredentialsSecret string
	// BackupKeySecret holds the backup key (ExportBackupKeyKey) that encrypts
	// the repository.
	BackupKeySecret string
	LockMode        ExportLockMode
	// KeepFor is the lock time of Compliance mode. It is used only when a
	// repository is created.
	KeepFor time.Duration
	// MaintenanceInterval is the time between Kopia maintenance runs, 1 hour at
	// the least.
	MaintenanceInterval time.Duration
	// ExpireBefore is used by maintenance only: Kopia snapshots that started
	// before this time are deleted. The zero time deletes nothing.
	ExpireBefore time.Time
}

// ExportOutcome is what an export Job reports.
type ExportOutcome struct {
	// Moment is the moment that was exported.
	Moment time.Time
	// Bytes is how much the repository grew: what the export sent to COS.
	Bytes int64
}

// validate checks the repository settings.
func (e ExportRepo) validate() error {
	if !exportEndpointPattern.MatchString(e.Endpoint) {
		return fmt.Errorf("export endpoint %q is not a host name with an optional port", e.Endpoint)
	}
	if !exportBucketPattern.MatchString(e.Bucket) {
		return fmt.Errorf("export bucket %q is not a valid bucket name", e.Bucket)
	}
	if !exportPrefixPattern.MatchString(e.Prefix) || strings.Contains(e.Prefix, "..") {
		return fmt.Errorf("export prefix %q has characters that are not allowed", e.Prefix)
	}
	for what, name := range map[string]string{"credentials": e.CredentialsSecret, "backup key": e.BackupKeySecret} {
		if !secretNamePattern.MatchString(name) {
			return fmt.Errorf("the %s Secret name %q is not a valid name", what, name)
		}
	}
	if e.MaintenanceInterval < v1alpha1.MinExportMaintenanceInterval {
		return fmt.Errorf("the maintenance interval %s is shorter than %s", e.MaintenanceInterval, v1alpha1.MinExportMaintenanceInterval)
	}
	switch e.LockMode {
	case ExportLockNone:
	case ExportLockCompliance:
		if min := v1alpha1.MinComplianceKeepFor(e.MaintenanceInterval); e.KeepFor < min {
			return fmt.Errorf("the keep time %s is shorter than the maintenance interval plus 24 hours (%s)", e.KeepFor, min)
		}
	default:
		return fmt.Errorf("unknown lock mode %q (use Compliance or None)", e.LockMode)
	}
	return nil
}

// exportParams is everything the export and maintenance scripts depend on. The
// fields root, termLog, stateDir and backend exist so tests can run the scripts
// against a repository in a folder; the runner leaves them empty.
type exportParams struct {
	repo        ExportRepo
	backupPath  string
	momentsPath string
	volume      string
	moment      time.Time

	// root is where the share is mounted (default /share0).
	root string
	// termLog is the termination message file (default /dev/termination-log).
	termLog string
	// stateDir is Kopia's private folder (default /tmp/kopia).
	stateDir string
	// backend replaces the S3 backend words, for tests.
	backend string
}

func (p exportParams) rootDir() string  { return orDefault(p.root, "/share0") }
func (p exportParams) termFile() string { return orDefault(p.termLog, "/dev/termination-log") }

// backendWords returns the Kopia words that name the repository: "s3 --bucket=..".
func (p exportParams) backendWords() string {
	if p.backend != "" {
		return p.backend
	}
	e := p.repo
	endpoint := strings.TrimPrefix(strings.TrimPrefix(e.Endpoint, "https://"), "http://")
	words := []string{"s3", "--bucket=" + util.ShellQuote(e.Bucket), "--endpoint=" + util.ShellQuote(endpoint)}
	if strings.HasPrefix(e.Endpoint, "http://") {
		words = append(words, "--disable-tls")
	}
	if e.Prefix != "" {
		words = append(words, "--prefix="+util.ShellQuote(e.Prefix))
	}
	return strings.Join(words, " ")
}

// retentionWords returns the words that lock new data. Only create sets them.
func (p exportParams) retentionWords() string {
	if p.repo.LockMode != ExportLockCompliance {
		return ""
	}
	return " --retention-mode=COMPLIANCE --retention-period=" + util.ShellQuote(p.repo.KeepFor.String())
}

// scriptHead is the start both scripts share. It never turns on command
// tracing, and the keys are only in the environment, so they cannot reach the
// Job's log.
func (p exportParams) scriptHead(b *strings.Builder) {
	state := orDefault(p.stateDir, "/tmp/kopia")
	fmt.Fprintf(b, "set -eu\nstate=%s\n", util.ShellQuote(state))
	b.WriteString(`mkdir -p "$state"
export HOME="$state"
export KOPIA_CONFIG_PATH="$state/repository.config"
export KOPIA_CACHE_DIRECTORY="$state/cache"
export KOPIA_LOG_DIR="$state/logs"
export KOPIA_CHECK_FOR_UPDATES=false
`)
	fmt.Fprintf(b, "ident=\"--no-persist-credentials --override-username=%s --override-hostname=%s\"\n", exportUser, exportHost)
}

// exportScript connects to the repository (creating it on first use) and makes
// one Kopia snapshot of the backup folder and one of the moments folder.
func exportScript(p exportParams) string {
	root := p.rootDir()
	backup := util.ShellQuote(root + p.backupPath)
	moments := util.ShellQuote(root + p.momentsPath)
	var b strings.Builder
	p.scriptHead(&b)
	backend := p.backendWords()
	fmt.Fprintf(&b, "kopia repository connect %s $ident 2>/dev/null || kopia repository create %s $ident%s\n", backend, backend, p.retentionWords())
	// Kopia drops snapshots by count. Keep them all: maintenance deletes by age.
	b.WriteString(keepAllSnapshotsPolicy + "\n")
	tags := "--tags=" + util.ShellQuote("moment:"+p.moment.UTC().Format(time.RFC3339)) + " --tags=" + util.ShellQuote("volume:"+p.volume)
	fmt.Fprintf(&b, `blob_total() { kopia blob stats --raw | sed -n 's/^Total: //p'; }
before=$(blob_total)
backup=%s
moments=%s
[ -d "$backup" ] || { echo "backup folder is missing" >&2; exit 1; }
kopia snapshot create --no-progress %s "$backup"
if [ -d "$moments" ]; then
  kopia snapshot create --no-progress %s "$moments"
fi
after=$(blob_total)
sent=$(( ${after:-0} - ${before:-0} ))
[ "$sent" -ge 0 ] || sent=0
printf '{"moment":"%%s","bytes":%%s}' %s "$sent" > %s
`, backup, moments, tags, tags, util.ShellQuote(p.moment.UTC().Format(time.RFC3339)), util.ShellQuote(p.termFile()))
	return b.String()
}

// keepAllSnapshotsPolicy is the Kopia policy of the repository. It stops Kopia
// from dropping snapshots by count (its default keeps the latest 10, then
// hourly, daily, weekly, monthly and yearly ones). Snapshots go by age, in the
// maintenance Job.
const keepAllSnapshotsPolicy = "kopia policy set --global --keep-latest=1000000 --keep-hourly=0 --keep-daily=0 --keep-weekly=0 --keep-monthly=0 --keep-annual=0"

// expireScript deletes the Kopia snapshots that started before the cutoff. Kopia
// prints RFC 3339 times in UTC, which sort as text.
func expireScript(before time.Time) string {
	return fmt.Sprintf(`cutoff=%s
kopia snapshot list --all --json | sed -n 's/^ *{"id":"\([^"]*\)".*"startTime":"\([^"]*\)".*/\1 \2/p' | while read -r id started; do
  if [ "$(LC_ALL=C expr "$started" '<' "$cutoff")" = 1 ]; then
    kopia snapshot delete "$id" --delete
  fi
done
`, util.ShellQuote(before.UTC().Format("2006-01-02T15:04:05Z")))
}

// maintenanceScript connects to an existing repository and runs Kopia's full
// maintenance. With a Compliance lock it first turns on the extension of
// object locks, so chunks still in use stay locked.
func maintenanceScript(p exportParams) string {
	var b strings.Builder
	p.scriptHead(&b)
	fmt.Fprintf(&b, "kopia repository connect %s $ident\n", p.backendWords())
	if !p.repo.ExpireBefore.IsZero() {
		b.WriteString(expireScript(p.repo.ExpireBefore))
	}
	set := "kopia maintenance set --full-interval=" + util.ShellQuote(p.repo.MaintenanceInterval.String())
	if p.repo.LockMode == ExportLockCompliance {
		set += " --extend-object-locks=true"
	}
	b.WriteString(set + "\n")
	b.WriteString("kopia maintenance run --full\n")
	return b.String()
}

// exportEnv gives the Job the keys as references to Secrets.
func exportEnv(e ExportRepo) []corev1.EnvVar {
	ref := func(name, secret, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key,
		}}}
	}
	return []corev1.EnvVar{
		ref("AWS_ACCESS_KEY_ID", e.CredentialsSecret, ExportAccessKeyKey),
		ref("AWS_SECRET_ACCESS_KEY", e.CredentialsSecret, ExportSecretKeyKey),
		ref("KOPIA_PASSWORD", e.BackupKeySecret, ExportBackupKeyKey),
	}
}

// kopiaShape is a Job that runs script in the driver image, which holds Kopia.
func (r *ShareJobRunner) kopiaShape(repo ExportRepo, script string, readOnly bool) jobShape {
	return jobShape{
		image:    r.driverImage,
		command:  []string{"/bin/sh", "-c", script},
		env:      exportEnv(repo),
		readOnly: readOnly,
		resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
		},
	}
}

// ExportToCOS runs a Job that sends the backup folder of one volume and its
// moments folder to the Kopia repository in COS. The Job mounts the snapshot
// pool's share read-only. It reports how much it sent.
func (r *ShareJobRunner) ExportToCOS(ctx context.Context, key string, share ShareMount, repo ExportRepo,
	backupPath, momentsPath string, moment time.Time) (bool, ExportOutcome, error) {
	if err := checkBackupFolders(backupPath, momentsPath); err != nil {
		return false, ExportOutcome{}, err
	}
	if err := repo.validate(); err != nil {
		return false, ExportOutcome{}, err
	}
	if r.driverImage == "" {
		return false, ExportOutcome{}, errors.New("no driver image set for COS export Jobs")
	}
	script := exportScript(exportParams{
		repo: repo, backupPath: backupPath, momentsPath: momentsPath,
		volume: path.Base(backupPath), moment: moment,
	})
	done, msg, err := r.runMessage(ctx, key, []ShareMount{share}, r.kopiaShape(repo, script, true))
	if err != nil || !done {
		return false, ExportOutcome{}, err
	}
	return true, parseExportOutcome(msg), nil
}

// MaintainCOSRepo runs a Job that does Kopia's maintenance on the repository in
// COS. The Job needs no share.
func (r *ShareJobRunner) MaintainCOSRepo(ctx context.Context, key string, repo ExportRepo) (bool, error) {
	if err := repo.validate(); err != nil {
		return false, err
	}
	if r.driverImage == "" {
		return false, errors.New("no driver image set for COS maintenance Jobs")
	}
	return r.run(ctx, key, nil, r.kopiaShape(repo, maintenanceScript(exportParams{repo: repo}), false))
}

// parseExportOutcome reads the termination message of an export Job. A message
// that is missing or cannot be read gives an outcome that is not known.
func parseExportOutcome(msg string) ExportOutcome {
	var m struct {
		Moment string `json:"moment"`
		Bytes  int64  `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(msg), &m); err != nil || m.Bytes < 0 {
		return ExportOutcome{}
	}
	out := ExportOutcome{Bytes: m.Bytes}
	if t, err := time.Parse(time.RFC3339, m.Moment); err == nil {
		out.Moment = t
	}
	return out
}
