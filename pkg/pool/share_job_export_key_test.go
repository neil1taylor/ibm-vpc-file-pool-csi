package pool

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// exportJobs starts one export Job and one maintenance Job and returns them.
func exportJobs(t *testing.T, repo ExportRepo) []*batchv1.Job {
	t.Helper()
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	if _, _, err := r.ExportToCOS(context.Background(), "exp-1", testShareA, repo, testBackupPath, testMomentsPath, testExportMoment); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MaintainCOSRepo(context.Background(), "mnt-1", repo); err != nil {
		t.Fatal(err)
	}
	return []*batchv1.Job{getShareJob(t, c, "exp-1"), getShareJob(t, c, "mnt-1")}
}

func volumeByName(spec corev1.PodSpec, name string) *corev1.Volume {
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == name {
			return &spec.Volumes[i]
		}
	}
	return nil
}

func TestExportJob_TheBackupKeyIsNotInTheJobSpec(t *testing.T) {
	for _, job := range exportJobs(t, testExportRepo()) {
		spec := job.Spec.Template.Spec
		ctr := spec.Containers[0]
		for _, e := range ctr.Env {
			if e.Name == "KOPIA_PASSWORD" {
				t.Errorf("%s: KOPIA_PASSWORD is in the Job environment", job.Name)
			}
			if e.Value != "" {
				t.Errorf("%s: %s has a literal value; secrets must be references", job.Name, e.Name)
			}
			if ref := e.ValueFrom; ref != nil && ref.SecretKeyRef != nil &&
				(ref.SecretKeyRef.Name == "cos-backup-key" || ref.SecretKeyRef.Name == "kp-apikey") {
				t.Errorf("%s: %s hands the key Secret to the environment of every process; use a file", job.Name, e.Name)
			}
		}
		raw, _ := json.Marshal(job)
		if strings.Contains(string(raw), "backupKey") {
			t.Errorf("%s: the Job refers to a readable backupKey", job.Name)
		}
	}
}

func TestExportJob_WrappedKeyAndAPIKeyAreSecretVolumesAndTheKeyGoesToMemory(t *testing.T) {
	for _, job := range exportJobs(t, testExportRepo()) {
		spec := job.Spec.Template.Spec
		for volume, secret := range map[string]string{"backup-key-wrapped": "cos-backup-key", "keyprotect-apikey": "kp-apikey"} {
			v := volumeByName(spec, volume)
			if v == nil || v.Secret == nil || v.Secret.SecretName != secret {
				t.Errorf("%s: volume %s = %+v; want a Secret volume of %s", job.Name, volume, v, secret)
			}
		}
		if volumeByName(spec, "backup-key-emergency") != nil {
			t.Errorf("%s: an emergency Secret volume without an emergency Secret", job.Name)
		}
		mem := volumeByName(spec, "backup-key-memory")
		if mem == nil || mem.EmptyDir == nil || mem.EmptyDir.Medium != corev1.StorageMediumMemory {
			t.Errorf("%s: the readable key needs a memory-backed volume, got %+v", job.Name, mem)
		}
		mounted := map[string]corev1.VolumeMount{}
		for _, m := range spec.Containers[0].VolumeMounts {
			mounted[m.Name] = m
		}
		for _, name := range []string{"backup-key-wrapped", "keyprotect-apikey"} {
			if m, ok := mounted[name]; !ok || !m.ReadOnly {
				t.Errorf("%s: %s must be mounted read-only, got %+v", job.Name, name, m)
			}
		}
		if _, ok := mounted["backup-key-memory"]; !ok {
			t.Errorf("%s: the memory volume is not mounted", job.Name)
		}
	}
}

func TestExportJob_EmergencySecretIsMountedOnlyWhenSet(t *testing.T) {
	repo := testExportRepo()
	repo.EmergencyKeySecret = "cos-emergency-key"
	for _, job := range exportJobs(t, repo) {
		v := volumeByName(job.Spec.Template.Spec, "backup-key-emergency")
		if v == nil || v.Secret == nil || v.Secret.SecretName != "cos-emergency-key" {
			t.Errorf("%s: emergency volume = %+v", job.Name, v)
		}
		if script := jobScript(job); !strings.Contains(script, "--emergency-key-file=") {
			t.Errorf("%s: the unwrap step does not get the emergency key file:\n%s", job.Name, script)
		}
	}
	for _, job := range exportJobs(t, testExportRepo()) {
		if script := jobScript(job); strings.Contains(script, "emergency") {
			t.Errorf("%s: no emergency Secret is set but the script mentions one:\n%s", job.Name, script)
		}
	}
}

func TestExportJob_ScriptUnwrapsTheKeyBeforeKopiaAndKeepsItOutOfTheArguments(t *testing.T) {
	for _, job := range exportJobs(t, testExportRepo()) {
		script := jobScript(job)
		unwrap := strings.Index(script, "--mode=unwrap-backup-key")
		kopia := strings.Index(script, "kopia ")
		if unwrap < 0 || kopia < 0 || unwrap > kopia {
			t.Fatalf("%s: the unwrap step must come before the first kopia command:\n%s", job.Name, script)
		}
		for _, want := range []string{"--kp-region='eu-de'", "--kp-instance='11111111-2222-3333-4444-555555555555'",
			"--kp-root-key='aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee'", "--wrapped-key-file=", "--api-key-file=", "--key-out="} {
			if !strings.Contains(script, want) {
				t.Errorf("%s: script lacks %s:\n%s", job.Name, want, script)
			}
		}
		if strings.Contains(script, "set -x") || strings.Contains(script, "--password") {
			t.Errorf("%s: the script traces commands or passes a password argument:\n%s", job.Name, script)
		}
	}
}

func TestExportRepo_ValidateRefusesBadKeyProtectSettings(t *testing.T) {
	cases := map[string]func(*ExportRepo){
		"no region":          func(r *ExportRepo) { r.KeyProtect.Region = "" },
		"region with slash":  func(r *ExportRepo) { r.KeyProtect.Region = "eu-de/x" },
		"no instance":        func(r *ExportRepo) { r.KeyProtect.InstanceID = "" },
		"no root key":        func(r *ExportRepo) { r.KeyProtect.RootKeyID = "" },
		"no api key secret":  func(r *ExportRepo) { r.KeyProtect.APIKeySecret = "" },
		"bad api key secret": func(r *ExportRepo) { r.KeyProtect.APIKeySecret = "A B" },
		"bad emergency name": func(r *ExportRepo) { r.EmergencyKeySecret = "A B" },
	}
	for name, mutate := range cases {
		repo := testExportRepo()
		mutate(&repo)
		if err := repo.validate(); err == nil {
			t.Errorf("%s: validate accepted it", name)
		}
	}
}

// The controller side never reads a Secret and never calls Key Protect.
func TestExportJobs_TheControllerNeverReadsASecret(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					t.Errorf("the controller read Secret %s", key)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.SecretList); ok {
					t.Error("the controller listed Secrets")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	r := NewShareJobRunner(c, testDriverNS, DefaultCloneImage)
	r.SetDriverImage("registry.example/driver:test")
	_, _, _ = r.ExportToCOS(context.Background(), "exp-1", testShareA, testExportRepo(), testBackupPath, testMomentsPath, testExportMoment)
	_, _ = r.MaintainCOSRepo(context.Background(), "mnt-1", testExportRepo())
}

func TestController_DoesNotImportKeyProtect(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			if strings.Contains(imp.Path.Value, "pkg/keyprotect") {
				t.Errorf("%s imports %s: only the Jobs may call Key Protect", f, imp.Path.Value)
			}
		}
	}
}

// Script-level tests: the unwrap step is replaced by a stand-in program that
// writes a key file the way the real mode does.

const unwrapStub = `#!/bin/sh
for a in "$@"; do case "$a" in --key-out=*) out="${a#--key-out=}";; esac; done
umask 077
printf %s "` + exportSecretSentinel + `" > "$out"
chmod 0400 "$out"
`

func (l *localExport) withUnwrap(t *testing.T, stub string) string {
	t.Helper()
	dir := t.TempDir()
	stubPath := filepath.Join(dir, "unwrap.sh")
	if err := os.WriteFile(stubPath, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	l.params.useKeyProtect = true
	l.params.unwrapCmd = "/bin/sh " + stubPath
	l.params.keyDir = filepath.Join(dir, "mem")
	if err := os.MkdirAll(l.params.keyDir, 0o777); err != nil {
		t.Fatal(err)
	}
	// Kopia must get its password from the unwrap step, not from the environment.
	l.env = nil
	return l.params.keyDir
}

func TestExportScript_UnwrappedKeyOpensTheRepositoryAndIsGoneAfterwards(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	keyDir := l.withUnwrap(t, unwrapStub)
	script := exportScript(l.params)
	msg, out, err := runScript(t, script, l.termLog, l.env)
	if err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	if parseExportOutcome(msg).Bytes < 1000 {
		t.Errorf("nothing was exported: %q", msg)
	}
	for name, text := range map[string]string{"script": script, "termination message": msg, "output": out} {
		if strings.Contains(text, exportSecretSentinel) {
			t.Errorf("the %s holds the key:\n%s", name, text)
		}
	}
	if entries, _ := os.ReadDir(keyDir); len(entries) != 0 {
		t.Errorf("the key file is still in the memory volume: %v", entries)
	}
}

func TestExportScript_AnotherKeyCannotOpenTheRepository(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	l.withUnwrap(t, unwrapStub)
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	l.withUnwrap(t, strings.Replace(unwrapStub, exportSecretSentinel, "another-key", 1))
	if _, _, err := runScript(t, exportScript(l.params), l.termLog, l.env); err == nil {
		t.Error("a different key opened the repository")
	}
}

func TestExportScript_FailedUnwrapStopsBeforeKopiaTouchesTheRepository(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	l.withUnwrap(t, "#!/bin/sh\necho 'Key Protect cannot be reached and no emergency key is set' >&2\nexit 1\n")
	_, out, err := runScript(t, exportScript(l.params), l.termLog, l.env)
	if err == nil {
		t.Fatal("the export went on after the unwrap failed")
	}
	if !strings.Contains(out, "no emergency key") {
		t.Errorf("the reason is not in the output: %s", out)
	}
	if _, statErr := os.Stat(filepath.Join(l.repoDir)); statErr == nil {
		t.Error("a repository was created without a key")
	}
}

func TestMaintenanceScript_UnwrappedKeyOpensTheRepository(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	l.withUnwrap(t, unwrapStub)
	if msg, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("export failed: %v\n%s\n%s", err, msg, out)
	}
	script := maintenanceScript(l.params)
	_, out, err := runScript(t, script, l.termLog, l.env)
	if err != nil {
		t.Fatalf("maintenance failed: %v\n%s", err, out)
	}
	if strings.Contains(out, exportSecretSentinel) {
		t.Errorf("the output holds the key:\n%s", out)
	}
}
