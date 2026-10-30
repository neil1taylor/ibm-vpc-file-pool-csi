package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// addVMDisk adds the volume apps/web-root, a disk of the VM web, to the policy.
func addVMDisk(e *env, exported bool) {
	e.t.Helper()
	pol := &v1alpha1.BackupPolicy{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: "nightly"}, pol); err != nil {
		e.t.Fatal(err)
	}
	last := metav1.NewTime(time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC))
	v := v1alpha1.BackupVolumeStatus{PVCNamespace: "apps", PVCName: "web-root", VMName: "web", SubVolumeName: "pvc-3",
		SubPath: "/pvcs/pvc-3", RestorePoints: 2, LastBackupTime: &last, Consistency: "CrashConsistent"}
	if exported {
		pol.Spec.Export = &v1alpha1.BackupExport{}
		v.ExportedTime = &last
		if err := e.c.Update(context.Background(), pol); err != nil {
			e.t.Fatal(err)
		}
	}
	pol.Status.Volumes = append(pol.Status.Volumes, v)
	if err := e.c.Status().Update(context.Background(), pol); err != nil {
		e.t.Fatal(err)
	}
}

func TestSessionOnAVMDiskIsAGuestSession(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	addVMDisk(e, false)
	resp, body := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "web-root"})
	var s Session
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal(body, &s) != nil {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if !s.Guest {
		t.Errorf("session = %+v: a VM disk is read inside, through the guest reader", s)
	}
	got := e.sessions()
	if len(got) != 1 || !got[0].Spec.Guest {
		t.Fatalf("sessions = %+v", got)
	}
	if evs := e.events(); len(evs) != 1 || !strings.Contains(evs[0].Message, "VM disk") {
		t.Errorf("audit event = %+v", evs)
	}
}

func TestSessionOnAPlainVolumeIsNotAGuestSession(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	addVMDisk(e, false)
	_, body := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "db"})
	var s Session
	if json.Unmarshal(body, &s) != nil || s.Guest {
		t.Fatalf("session = %s", body)
	}
}

func TestAVMDiskInCOSIsRefusedInWords(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	addVMDisk(e, true)
	resp, body := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "web-root", Source: "cos"})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "VM disk") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	// With no source named, a moment that only COS holds is refused the same way.
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	resp, body = e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "web-root", Moment: &old})
	if resp.StatusCode == http.StatusAccepted && strings.Contains(string(body), `"source":"cos"`) {
		t.Fatalf("a VM disk session was started on COS: %s", body)
	}
}

func TestGuestAndPlainSessionsAreNeverShared(t *testing.T) {
	a := &v1alpha1.BackupBrowseSpec{Policy: "p", PVCNamespace: "n", PVCName: "c", Guest: true}
	b := &v1alpha1.BackupBrowseSpec{Policy: "p", PVCNamespace: "n", PVCName: "c"}
	if sameBackup(a, b, nil) || !sameBackup(a, a, nil) {
		t.Error("sameBackup must tell a guest session from a plain one")
	}
}

func TestRestoreFromAVMDiskIsAGuestRestore(t *testing.T) {
	e := newRestoreEnv(t)
	addVMDisk(e, false)
	req := newReq()
	req["pvc"] = "web-root"
	req["paths"] = []string{"/sda1/etc/hosts"}
	resp, body := e.do("POST", "/v1/restores", "alice-token", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	rs := e.restores()
	if len(rs) != 1 || !rs[0].Spec.Guest {
		t.Fatalf("restores = %+v", rs)
	}
}

func TestRestoreFromAPlainVolumeIsNotAGuestRestore(t *testing.T) {
	e := newRestoreEnv(t)
	addVMDisk(e, false)
	if resp, body := e.do("POST", "/v1/restores", "alice-token", newReq()); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if rs := e.restores(); len(rs) != 1 || rs[0].Spec.Guest {
		t.Fatalf("restores = %+v", rs)
	}
}

func TestRestoreFromAVMDiskInCOSIsRefused(t *testing.T) {
	e := newRestoreEnv(t)
	addVMDisk(e, true)
	req := newReq()
	req["pvc"] = "web-root"
	req["source"] = "cos"
	resp, body := e.do("POST", "/v1/restores", "alice-token", req)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "VM disk") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(e.restores()) != 0 {
		t.Error("a restore was made")
	}
}
