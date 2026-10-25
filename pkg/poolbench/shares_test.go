package poolbench

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const agentMetrics = `# HELP vpc_file_pool_share_nfs_operations_total NFS operations.
vpc_file_pool_share_nfs_operations_total{op="read",pool="poolbench-t10k",share="s1"} 1000
vpc_file_pool_share_nfs_operations_total{op="write",pool="poolbench-t10k",share="s1"} 500
vpc_file_pool_share_nfs_operations_total{op="other",pool="poolbench-t10k",share="s1"} 99999
vpc_file_pool_share_nfs_operations_total{op="read",pool="other",share="s2"} 7777
vpc_file_pool_share_nfs_execute_seconds_total{op="read",pool="poolbench-t10k",share="s1"} 0.5
vpc_file_pool_share_nfs_execute_seconds_total{op="write",pool="poolbench-t10k",share="s1"} 0.25
vpc_file_pool_share_nfs_execute_seconds_total{op="read",pool="poolbench-t10k-x",share="s3"} 9
`

func TestParseShareTotals(t *testing.T) {
	ops, secs := parseShareTotals(strings.NewReader(agentMetrics), "poolbench-t10k")
	if ops != 1500 || secs != 0.75 {
		t.Fatalf("got %v ops, %v s; want 1500 and 0.75 (reads and writes of this pool only)", ops, secs)
	}
}

func slice(port int, ips ...string) *discoveryv1.EndpointSlice {
	p := int32(port) // #nosec G115 -- a test port
	name := "metrics"
	var eps []discoveryv1.Endpoint
	for _, ip := range ips {
		eps = append(eps, discoveryv1.Endpoint{Addresses: []string{ip}})
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "node-metrics-abc", Namespace: "kube-system",
			Labels: map[string]string{discoveryv1.LabelServiceName: "csi-node-metrics"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   eps,
		Ports:       []discoveryv1.EndpointPort{{Name: &name, Port: &p}},
	}
}

func TestNodeAgentShares_AddsUpEveryNode(t *testing.T) {
	var gotAuth []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		_, _ = fmt.Fprint(w, agentMetrics)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(slice(port, host, host)).Build()
	n := &NodeAgentShares{Client: c, Namespace: "kube-system", Service: "csi-node-metrics", HTTP: srv.Client(), TokenFile: tokenFile}
	ops, secs, err := n.Totals(context.Background(), "poolbench-t10k")
	if err != nil {
		t.Fatal(err)
	}
	if ops != 3000 || secs != 1.5 {
		t.Fatalf("got %v ops, %v s; want two nodes added up", ops, secs)
	}
	if len(gotAuth) != 2 || gotAuth[0] != "Bearer sa-token" {
		t.Fatalf("auth headers: %v", gotAuth)
	}
}

func TestNodeAgentShares_FailsIfANodeFails(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(slice(port, host)).Build()
	n := &NodeAgentShares{Client: c, Namespace: "kube-system", Service: "csi-node-metrics", HTTP: srv.Client()}
	if _, _, err := n.Totals(context.Background(), "poolbench-t10k"); err == nil {
		t.Fatal("a partial sum would be wrong: want an error")
	}
}

func TestNodeAgentShares_NoNodes(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	n := &NodeAgentShares{Client: c, Namespace: "kube-system", Service: "csi-node-metrics", HTTP: http.DefaultClient}
	if _, _, err := n.Totals(context.Background(), "poolbench-t10k"); err == nil {
		t.Fatal("want an error when no node agent is found")
	}
}
