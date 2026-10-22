package nodemetrics

import (
	"strings"
	"testing"
)

const sampleMountStats = `device rootfs mounted on / with fstype rootfs
device 10.240.0.5:/share_abc mounted on /var/data/kubelet/plugins/kubernetes.io/csi/vpc-file-pool.csi.ibm.io/h1/globalmount with fstype nfs4 statvers=1.1
	opts:	rw,vers=4.1,rsize=1048576,soft,timeo=600
	age:	3600
	bytes:	1 2 3 4 5 6 7 8
	RPC iostats version: 1.1  p/v: 100003/4 (nfs)
	xprt:	tcp 800 0 1 0 5 10 10 0 10 0 2 0 0
	per-op statistics
	        NULL: 1 1 0 44 24 0 1 2 0
	        READ: 100 100 0 20000 4000000 5 300 400 0
	       WRITE: 50 51 1 5000000 2000 10 150 250 3
	      GETATTR: 10 10 0 800 1200 0 20 30 0
	      LOOKUP: 4 4 0 400 600 0 8 12 0
device 10.240.0.5:/share_abc mounted on /pods/p1/volumes/mnt with fstype nfs4 statvers=1.1
	per-op statistics
	        READ: 100 100 0 20000 4000000 5 300 400 0
device tmpfs mounted on /dev with fstype tmpfs
device server:/export mounted on /mnt/old with fstype nfs statvers=1.1
	per-op statistics
	        READ: 7 7 0 10 20 1 2 3
`

func TestParseMountStats(t *testing.T) {
	got, err := ParseMountStats(strings.NewReader(sampleMountStats))
	if err != nil {
		t.Fatalf("ParseMountStats: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 NFS mounts (nfs4, nfs4 bind, nfs), got %d: %+v", len(got), got)
	}
	m := got[0]
	if m.Server != "10.240.0.5" || m.Export != "/share_abc" {
		t.Errorf("server/export = %q %q", m.Server, m.Export)
	}
	if m.MountPoint != "/var/data/kubelet/plugins/kubernetes.io/csi/vpc-file-pool.csi.ibm.io/h1/globalmount" {
		t.Errorf("mount point = %q", m.MountPoint)
	}
	read := m.Ops["READ"]
	if read.Ops != 100 || read.BytesSent != 20000 || read.BytesRecv != 4000000 || read.RTTMillis != 300 || read.ExecMillis != 400 {
		t.Errorf("READ = %+v", read)
	}
	write := m.Ops["WRITE"]
	if write.Ops != 50 || write.BytesSent != 5000000 || write.RTTMillis != 150 || write.ExecMillis != 250 {
		t.Errorf("WRITE = %+v", write)
	}
	if _, ok := m.Ops["NULL"]; !ok {
		t.Errorf("NULL op missing")
	}
}

func TestParseMountStats_OlderKernelWithoutErrorsColumn(t *testing.T) {
	got, err := ParseMountStats(strings.NewReader(got1))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
	if r := got[0].Ops["READ"]; r.Ops != 7 || r.ExecMillis != 3 {
		t.Errorf("READ = %+v", r)
	}
}

const got1 = `device server:/export mounted on /mnt/old with fstype nfs4 statvers=1.1
	per-op statistics
	        READ: 7 7 0 10 20 1 2 3
`

func TestParseMountStats_Empty(t *testing.T) {
	got, err := ParseMountStats(strings.NewReader(""))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestParseMountStats_IgnoresMalformedOpLines(t *testing.T) {
	in := "device s:/e mounted on /m with fstype nfs4 statvers=1.1\n\tper-op statistics\n\t READ: x y\n\t WRITE: 1 1 0 2 3 0 4 5\n"
	got, err := ParseMountStats(strings.NewReader(in))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
	if _, ok := got[0].Ops["READ"]; ok {
		t.Errorf("malformed READ should be skipped")
	}
	if got[0].Ops["WRITE"].Ops != 1 {
		t.Errorf("WRITE lost")
	}
}
