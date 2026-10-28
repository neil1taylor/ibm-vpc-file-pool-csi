package pool

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// receiverServer starts a TLS receiver that accepts the token "good-token".
// storageDown makes its health call answer 503.
func receiverServer(t *testing.T, storageDown bool) (*httptest.Server, []byte) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		if storageDown {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/auth-check", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return srv, ca
}

// unrelatedCA returns a CA certificate that did not sign the test receiver.
func unrelatedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func receiverPolicy(endpoint string) *v1alpha1.ReplicationPolicy {
	p := newTestReplicationPolicy("to-dr", "prod-pool", "1h")
	p.Spec.DestinationNFSServer = ""
	p.Spec.DestinationEndpoint = endpoint
	p.Spec.DestinationAuthSecretRef = "to-dr-receiver-token"
	p.Spec.DestinationCACertSecretRef = "to-dr-receiver-ca"
	return p
}

func putSecret(t *testing.T, dc client.Client, name, key, value string) {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: replJobNamespace, Name: name},
		Data:       map[string][]byte{key: []byte(value)},
	}
	if err := dc.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func TestReceiverReachable(t *testing.T) {
	good, goodCA := receiverServer(t, false)
	down, downCA := receiverServer(t, true)
	otherCA := unrelatedCA(t)

	tests := []struct {
		name       string
		endpoint   string
		token      string
		ca         []byte
		noTokenSec bool
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string
	}{
		{"reachable", good.URL, "good-token", goodCA, false, metav1.ConditionTrue, "Reachable", "The receiver answers and accepts the token."},
		{"wrong token", good.URL, "bad", goodCA, false, metav1.ConditionFalse, "TokenRefused", "token refused"},
		{"certificate not trusted", good.URL, "good-token", otherCA, false, metav1.ConditionFalse, "CertificateNotTrusted", "certificate not trusted"},
		{"storage down", down.URL, "good-token", downCA, false, metav1.ConditionFalse, "StorageNotReachable", "receiver storage not reachable"},
		{"no route", "https://127.0.0.1:1", "good-token", goodCA, false, metav1.ConditionFalse, "CannotConnect", "cannot connect"},
		{"token secret missing", good.URL, "", goodCA, true, metav1.ConditionFalse, "SecretMissing", "token secret to-dr-receiver-token not found in kube-system"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dc := newReplFakeDirectClient()
			if !tt.noTokenSec {
				putSecret(t, dc, "to-dr-receiver-token", "token", tt.token)
			}
			putSecret(t, dc, "to-dr-receiver-ca", "ca.crt", string(tt.ca))
			rc := newReplController(newReplFakeK8sClient(), dc, newFakeNFSOperations())

			p := receiverPolicy(tt.endpoint)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			rc.checkReceiver(ctx, p)

			c := meta.FindStatusCondition(p.Status.Conditions, "ReceiverReachable")
			if c == nil {
				t.Fatal("no ReceiverReachable condition")
			}
			if c.Status != tt.wantStatus || c.Reason != tt.wantReason || c.Message != tt.wantMsg {
				t.Errorf("got %s/%s/%q, want %s/%s/%q", c.Status, c.Reason, c.Message, tt.wantStatus, tt.wantReason, tt.wantMsg)
			}
		})
	}
}

func TestReceiverReachable_NotForDirectNFS(t *testing.T) {
	rc := newReplController(newReplFakeK8sClient(), newReplFakeDirectClient(), newFakeNFSOperations())
	p := newTestReplicationPolicy("nfs", "prod-pool", "1h")
	rc.checkReceiver(context.Background(), p)
	if len(p.Status.Conditions) != 0 {
		t.Errorf("direct NFS policy got conditions: %v", p.Status.Conditions)
	}
}

func TestProcessOnce_SetsReachableAndChecksOncePerSchedule(t *testing.T) {
	srv, ca := receiverServer(t, false)
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	putSecret(t, dc, "to-dr-receiver-token", "token", "good-token")
	putSecret(t, dc, "to-dr-receiver-ca", "ca.crt", string(ca))
	k.addPolicy(receiverPolicy(srv.URL))
	rc := newReplController(k, dc, newFakeNFSOperations())

	rc.processOnce(context.Background())
	c := meta.FindStatusCondition(k.getPolicy("to-dr").Status.Conditions, "ReceiverReachable")
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("after first tick: %+v", c)
	}

	// Within the schedule (1h) the receiver is not asked again.
	srv.Close()
	rc.processOnce(context.Background())
	c = meta.FindStatusCondition(k.getPolicy("to-dr").Status.Conditions, "ReceiverReachable")
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("second tick should not re-check: %+v", c)
	}

	// After the schedule has passed it is asked again and the failure shows.
	rc.SetNowFunc(func() time.Time { return time.Now().Add(2 * time.Hour) })
	rc.processOnce(context.Background())
	c = meta.FindStatusCondition(k.getPolicy("to-dr").Status.Conditions, "ReceiverReachable")
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("after schedule: %+v", c)
	}
}
