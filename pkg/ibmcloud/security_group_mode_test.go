package ibmcloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sgModeMountPath is a mount path in the form the VPC API returns for a
// security-group mount target (seen on rovslab-prod-eude, 2026-10-19).
const sgModeMountPath = "10.250.0.18:/83ad9ea6_1be6_4c1e_9b3f_2a0d8c3e7f10"

// decodeBody reads a JSON request body into a generic map.
func decodeBody(t *testing.T, r *http.Request) map[string]interface{} {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request body %q: %v", raw, err)
	}
	return body
}

// assertSecurityGroupMountTarget checks that a mount target prototype uses a
// virtual network interface in the given subnet with the given security groups,
// and does not name a VPC (VPC access mode is refused by the API).
func assertSecurityGroupMountTarget(t *testing.T, mt map[string]interface{}, subnetID string, sgIDs []string) {
	t.Helper()
	if _, ok := mt["vpc"]; ok {
		t.Errorf("mount target names a VPC (VPC access mode): %v", mt)
	}
	vni, ok := mt["virtual_network_interface"].(map[string]interface{})
	if !ok {
		t.Fatalf("mount target has no virtual_network_interface: %v", mt)
	}
	subnet, _ := vni["subnet"].(map[string]interface{})
	if subnet["id"] != subnetID {
		t.Errorf("VNI subnet = %v, want id %q", vni["subnet"], subnetID)
	}
	groups, _ := vni["security_groups"].([]interface{})
	if len(groups) != len(sgIDs) {
		t.Fatalf("VNI security_groups = %v, want ids %v", vni["security_groups"], sgIDs)
	}
	for i, g := range groups {
		if group, _ := g.(map[string]interface{}); group["id"] != sgIDs[i] {
			t.Errorf("security_groups[%d] = %v, want id %q", i, g, sgIDs[i])
		}
	}
}

func TestCreateFileShare_SecurityGroupMode(t *testing.T) {
	var body map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		writeJSON(w, http.StatusCreated, shareJSONWithMountTargets(
			"share-sg", "sg-share", "stable", "dp2", "eu-de-3", 500, 10000,
			[]map[string]interface{}{mountTargetRef("mt-sg", "sg-share-mt")}))
	})
	mux.HandleFunc("/shares/share-sg", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, shareJSONWithMountTargets(
			"share-sg", "sg-share", "stable", "dp2", "eu-de-3", 500, 10000,
			[]map[string]interface{}{mountTargetRef("mt-sg", "sg-share-mt")}))
	})
	mux.HandleFunc("/shares/share-sg/mount_targets/mt-sg", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-sg", "sg-share-mt", sgModeMountPath, "10.250.0.18"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, newTestVPCService(t, server.URL))

	info, err := client.CreateFileShare(context.Background(), CreateShareInput{
		Name:             "sg-share",
		Zone:             "eu-de-3",
		Profile:          "dp2",
		SizeGB:           500,
		SubnetID:         "subnet-eu-de-3",
		SecurityGroupIDs: []string{"sg-cluster"},
	})
	if err != nil {
		t.Fatalf("CreateFileShare() error = %v", err)
	}

	if body["access_control_mode"] != "security_group" {
		t.Errorf("access_control_mode = %v, want security_group", body["access_control_mode"])
	}
	mts, _ := body["mount_targets"].([]interface{})
	if len(mts) != 1 {
		t.Fatalf("mount_targets = %v, want exactly one", body["mount_targets"])
	}
	mt0, _ := mts[0].(map[string]interface{})
	assertSecurityGroupMountTarget(t, mt0, "subnet-eu-de-3", []string{"sg-cluster"})

	mt := info.MountTargets[0]
	if mt.IPAddress != "10.250.0.18" || mt.ExportPath != "/83ad9ea6_1be6_4c1e_9b3f_2a0d8c3e7f10" {
		t.Errorf("mount target = %s:%s, want %s", mt.IPAddress, mt.ExportPath, sgModeMountPath)
	}
}

func TestCreateFileShare_RequiresSubnet(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("share was created without mount target details")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, newTestVPCService(t, server.URL))

	_, err := client.CreateFileShare(context.Background(), CreateShareInput{
		Name: "no-subnet", Zone: "eu-de-3", Profile: "dp2", SizeGB: 100,
	})
	if err == nil {
		t.Fatal("expected an error for a share with no subnet")
	}
}

func TestCreateShareMountTarget_SecurityGroupMode(t *testing.T) {
	var body map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-1/mount_targets", func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		writeJSON(w, http.StatusCreated, mountTargetJSON("mt-1", "mt", "", ""))
	})
	mux.HandleFunc("/shares/share-1/mount_targets/mt-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-1", "mt", sgModeMountPath, "10.250.0.18"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, newTestVPCService(t, server.URL))

	mt, err := client.CreateShareMountTarget(context.Background(), "share-1", CreateMountTargetInput{
		Name:             "mt",
		SubnetID:         "subnet-eu-de-3",
		SecurityGroupIDs: []string{"sg-a", "sg-b"},
	})
	if err != nil {
		t.Fatalf("CreateShareMountTarget() error = %v", err)
	}
	assertSecurityGroupMountTarget(t, body, "subnet-eu-de-3", []string{"sg-a", "sg-b"})
	if mt.ExportPath != "/83ad9ea6_1be6_4c1e_9b3f_2a0d8c3e7f10" {
		t.Errorf("ExportPath = %q, want the share-specific export", mt.ExportPath)
	}
}

func TestGetMountTarget_SecurityGroupModeExportPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-1/mount_targets/mt-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-1", "mt", sgModeMountPath, "10.250.0.18"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, newTestVPCService(t, server.URL))

	mt, err := client.getMountTarget(context.Background(), "share-1", "mt-1")
	if err != nil {
		t.Fatalf("getMountTarget() error = %v", err)
	}
	if mt.IPAddress != "10.250.0.18" {
		t.Errorf("IPAddress = %q, want 10.250.0.18", mt.IPAddress)
	}
	if mt.ExportPath != "/83ad9ea6_1be6_4c1e_9b3f_2a0d8c3e7f10" {
		t.Errorf("ExportPath = %q, want the share-specific export, never \"/\"", mt.ExportPath)
	}
}

// A mount target whose IP is known but whose mount path is not yet set is not
// usable: without the export path, every NFS mount would fail.
func TestGetMountTarget_IPWithoutMountPathIsNotReady(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-1/mount_targets/mt-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-1", "mt", "", "10.250.0.18"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, newTestVPCService(t, server.URL))

	mt, err := client.getMountTarget(context.Background(), "share-1", "mt-1")
	if err != nil {
		t.Fatalf("getMountTarget() error = %v", err)
	}
	if mt.IPAddress != "" || mt.ExportPath != "" {
		t.Errorf("mount target = %q:%q, want empty until the mount path is set", mt.IPAddress, mt.ExportPath)
	}
}

func TestGetSubnetZone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/subnets/subnet-3", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id":   "subnet-3",
			"name": "workers-3",
			"zone": map[string]interface{}{"name": "eu-de-3"},
		})
	})
	mux.HandleFunc("/subnets/missing", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "not found"}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, newTestVPCService(t, server.URL))

	zone, err := client.GetSubnetZone(context.Background(), "subnet-3")
	if err != nil || zone != "eu-de-3" {
		t.Errorf("GetSubnetZone() = %q, %v; want eu-de-3, nil", zone, err)
	}
	if _, err := client.GetSubnetZone(context.Background(), "missing"); err == nil {
		t.Error("expected an error for a missing subnet")
	}
}

func TestFindSecurityGroupID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/security_groups", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("vpc.id"); got != "vpc-1" {
			t.Errorf("vpc.id filter = %q, want vpc-1", got)
		}
		sg := func(id, name string) map[string]interface{} {
			return map[string]interface{}{"id": id, "name": name}
		}
		if r.URL.Query().Get("start") == "" {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"security_groups": []interface{}{sg("sg-1", "kube-lbaas-abc"), sg("sg-2", "default-sg")},
				"next":            map[string]interface{}{"href": "http://" + r.Host + "/security_groups?start=page2&vpc.id=vpc-1"},
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"security_groups": []interface{}{sg("sg-3", "kube-abc")},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, newTestVPCService(t, server.URL))

	id, err := client.FindSecurityGroupID(context.Background(), "vpc-1", "kube-abc")
	if err != nil || id != "sg-3" {
		t.Errorf("FindSecurityGroupID() = %q, %v; want sg-3, nil", id, err)
	}
	_, err = client.FindSecurityGroupID(context.Background(), "vpc-1", "kube-nope")
	if err == nil || !strings.Contains(err.Error(), "kube-nope") {
		t.Errorf("FindSecurityGroupID(missing) error = %v, want one that names the group", err)
	}
}
