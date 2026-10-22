package replication

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateCheckJobGolden = flag.Bool("update-check-job", false, "rewrite the check Job golden file")

// checkJobGoldenPath is read by the console plugin tests too (console-plugin/src/utils/receiver-objects.test.ts).
// Both sides build the same Job from the same input, so the Go and TypeScript builders cannot drift apart.
const checkJobGoldenPath = "testdata/check-job.golden.json"

func goldenCheckJobSpec() CheckJobSpec {
	return CheckJobSpec{
		Name:       "repl-check-dr",
		Namespace:  "kube-system",
		Image:      "icr.io/example/ibm-vpc-file-pool-csi:v1.0.0",
		Endpoint:   "https://repl.example.com",
		AuthSecret: "dr-receiver-token",
		CASecret:   "dr-receiver-ca",
	}
}

func TestBuildCheckJob_MatchesGoldenFile(t *testing.T) {
	spec := goldenCheckJobSpec()
	job := BuildCheckJob(spec)

	rawJob, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var jobMap map[string]any
	if err = json.Unmarshal(rawJob, &jobMap); err != nil {
		t.Fatal(err)
	}
	// Drop what a client does not set: the zero creation times and the empty status.
	delete(jobMap, "status")
	if meta, ok := jobMap["metadata"].(map[string]any); ok {
		delete(meta, "creationTimestamp")
	}
	if tmpl, ok := jobMap["spec"].(map[string]any)["template"].(map[string]any); ok {
		if meta, ok := tmpl["metadata"].(map[string]any); ok {
			delete(meta, "creationTimestamp")
		}
	}

	got, err := json.MarshalIndent(map[string]any{
		"input": map[string]any{
			"name":       spec.Name,
			"namespace":  spec.Namespace,
			"image":      spec.Image,
			"endpoint":   spec.Endpoint,
			"authSecret": spec.AuthSecret,
			"caSecret":   spec.CASecret,
		},
		"job": jobMap,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	if *updateCheckJobGolden {
		if err = os.MkdirAll(filepath.Dir(checkJobGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(checkJobGoldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(checkJobGoldenPath)
	if err != nil {
		t.Fatalf("read golden file (run: go test ./pkg/replication -run CheckJob -update-check-job): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the check Job changed. If that is on purpose, run: go test ./pkg/replication -run CheckJob -update-check-job, and change the console builder (console-plugin/src/utils/receiver-objects.ts) to match.\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildCheckJob_WithoutCASecretLeavesOutTheCA(t *testing.T) {
	spec := goldenCheckJobSpec()
	spec.CASecret = ""
	job := BuildCheckJob(spec)

	container := job.Spec.Template.Spec.Containers[0]
	for _, arg := range container.Args {
		if arg == "--ca-cert-file=/etc/replication-ca/ca.crt" {
			t.Errorf("unexpected CA argument: %v", container.Args)
		}
	}
	if len(job.Spec.Template.Spec.Volumes) != 1 || len(container.VolumeMounts) != 1 {
		t.Errorf("want only the token volume, got %d volumes and %d mounts", len(job.Spec.Template.Spec.Volumes), len(container.VolumeMounts))
	}
}

func TestBuildCheckJob_RunsOnceAndEndsByItself(t *testing.T) {
	job := BuildCheckJob(goldenCheckJobSpec())
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("the check must not retry: backoffLimit = %v", job.Spec.BackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Error("the Job must delete itself after it ends")
	}
	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Error("the Job must have a deadline")
	}
}
