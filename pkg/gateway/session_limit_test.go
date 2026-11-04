package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

// seedSession puts a session of user in apps/db on the cluster, as the gateway made it.
func (e *env) seedSession(name, user string, age time.Duration, moment *time.Time) {
	e.t.Helper()
	bb := &v1alpha1.BackupBrowse{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Labels:            map[string]string{userLabel: userHash(user)},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Spec: v1alpha1.BackupBrowseSpec{Policy: "nightly", PVCNamespace: "apps", PVCName: "db", RequestedBy: user},
	}
	if moment != nil {
		m := metav1.NewTime(*moment)
		bb.Spec.Moment = &m
	}
	if err := e.c.Create(context.Background(), bb); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) closing(name string) bool {
	e.t.Helper()
	for _, s := range e.sessions() {
		if s.Name == name {
			return s.Spec.Close
		}
	}
	e.t.Fatalf("no session %s", name)
	return false
}

func TestNewMomentClosesTheUsersOldestSessionWhenTheNamespaceIsFull(t *testing.T) {
	e := newEnv(t, Config{MaxSessionsPerNamespace: 2}, memTree{})
	m1 := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	e.seedSession("bb-old", "alice", 10*time.Minute, nil)
	e.seedSession("bb-new", "alice", 5*time.Minute, &m1)
	m2 := m1.Add(time.Hour)
	resp, body := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "db", Moment: &m2})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if !e.closing("bb-old") || e.closing("bb-new") {
		t.Errorf("the oldest session should close and the newer one stay: old=%v new=%v", e.closing("bb-old"), e.closing("bb-new"))
	}
	if len(e.sessions()) != 3 {
		t.Errorf("%d sessions, want the new one made", len(e.sessions()))
	}
}

func TestNewMomentIsRefusedAtOnceWhenOtherUsersHoldTheNamespace(t *testing.T) {
	e := newEnv(t, Config{MaxSessionsPerNamespace: 2}, memTree{})
	e.authz.set("bob", "apps", true)
	e.seedSession("bb-1", "bob", 10*time.Minute, nil)
	e.seedSession("bb-2", "carol", 5*time.Minute, nil)
	resp, body := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "db"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	var er ErrorBody
	_ = json.Unmarshal(body, &er)
	if er.Code != "too_many" || !strings.Contains(er.Message, "apps") || !strings.Contains(er.Message, "2") {
		t.Errorf("error = %s", body)
	}
	if len(e.sessions()) != 2 || e.closing("bb-1") || e.closing("bb-2") {
		t.Error("another user's session was touched or a session was made")
	}
}

func TestSessionsThatHaveEndedDoNotCountAgainstTheNamespace(t *testing.T) {
	e := newEnv(t, Config{MaxSessionsPerNamespace: 2}, memTree{})
	e.seedSession("bb-1", "bob", 10*time.Minute, nil)
	e.seedSession("bb-2", "bob", 9*time.Minute, nil)
	for _, n := range []string{"bb-1", "bb-2"} {
		bb := &v1alpha1.BackupBrowse{}
		_ = e.c.Get(context.Background(), types.NamespacedName{Name: n}, bb)
		bb.Status.Phase = "Closed"
		_ = e.c.Status().Update(context.Background(), bb)
	}
	resp, body := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "db"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
}

func TestNoLimitMeansNoCheck(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.seedSession("bb-1", "bob", 10*time.Minute, nil)
	e.seedSession("bb-2", "bob", 9*time.Minute, nil)
	e.seedSession("bb-3", "bob", 8*time.Minute, nil)
	resp, _ := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "db"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
