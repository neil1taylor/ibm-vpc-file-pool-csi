package pool

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// labEnv runs the backup controller and the replication controller against one
// API server, as in production: a VM with three disks, a policy that runs every
// minute, and browse sessions on its disks.
type labEnv struct {
	*browseEnv
	rc                   *ReplicationController
	cop                  *fakeVMCopier
	svs                  []string
	jobsSeen             [][]string
	slowCopy, lastStarts int
	jobAge               map[string]int
}

func newLabEnv(t *testing.T) *labEnv {
	t.Helper()
	e := newBrowseEnv(t)
	e.jobs.guest = true
	e.addClaim("apps", "db2", "data-a", yes)
	e.addClaim("apps", "db3", "data-a", yes)
	e.addVM("apps", "web", "db", "db2", "db3")
	bp := e.bp("nightly")
	bp.Spec.Schedule = "1m"
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	l := &labEnv{browseEnv: e, cop: newFakeVMCopier()}
	for _, n := range []string{"db", "db2", "db3"} {
		l.svs = append(l.svs, "pvc-apps-"+n)
	}
	pod := newLauncherPod("virt-launcher-web-abcde", "pod-uid-1", "web", map[string]string{"d1": "db", "d2": "db2", "d3": "db3"})
	pod.Namespace = "apps"
	if err := e.c.Create(e.ctx, pod); err != nil {
		t.Fatal(err)
	}
	l.rc = NewReplicationController(k8s.NewClient(e.c), e.c, newFakeNFSOperations())
	l.rc.SetDriverImage("registry.example/driver:test")
	l.rc.SetVMCopier(l.cop)
	l.rc.SetNowFunc(func() time.Time { return e.now })
	e.reconcile()
	return l
}

// cycle is one pass of both controllers, then 30 seconds. Copies end at once
// and every apply Job succeeds, so a copy takes two passes.
func (l *labEnv) cycle() {
	l.t.Helper()
	l.rc.processOnce(l.ctx)
	l.cop.state = vmcopy.Done
	jobs := &batchv1.JobList{}
	if err := l.c.List(l.ctx, jobs, client.InNamespace(replJobNamespace)); err != nil {
		l.t.Fatal(err)
	}
	l.jobsSeen = append(l.jobsSeen, nil)
	for i := range jobs.Items {
		l.jobsSeen[len(l.jobsSeen)-1] = append(l.jobsSeen[len(l.jobsSeen)-1], jobs.Items[i].Name)
		jobs.Items[i].Status.Succeeded = 1
		if err := l.c.Status().Update(l.ctx, &jobs.Items[i]); err != nil {
			l.t.Fatal(err)
		}
	}
	l.reconcile()
	l.now = l.now.Add(30 * time.Second)
}

func (l *labEnv) openOn(name, claim string) {
	l.open(name, func(b *v1alpha1.BackupBrowse) { b.Spec.Guest = true; b.Spec.PVCName = claim })
}

// With sessions open on two disks of a VM, the disks of the sessions get no new
// copy, whether the VM runs (copies go through QEMU) or is stopped (file copies),
// and both sessions start.
func TestLab_SessionsOnTwoDisksOfOneVM(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		l := newLabEnv(t)
		if stopped {
			pods := &corev1.PodList{}
			if err := l.c.List(l.ctx, pods); err != nil {
				t.Fatal(err)
			}
			for i := range pods.Items {
				if err := l.c.Delete(l.ctx, &pods.Items[i]); err != nil {
					t.Fatal(err)
				}
			}
		}
		for i := 0; i < 4; i++ {
			l.cycle()
		}
		l.openOn("bb1", "db")
		l.cycle()
		l.cycle()
		l.openOn("bb2", "db2")
		for i := 0; i < 16; i++ {
			l.cycle()
			if i < 3 {
				continue // a copy that began before the hold may still end
			}
			for _, job := range l.jobsSeen[len(l.jobsSeen)-1] {
				if strings.HasSuffix(job, "--pvc-apps-db") || strings.HasSuffix(job, "--pvc-apps-db2") {
					t.Fatalf("stopped=%v pass %d: a copy Job %s exists for a disk with a browse session", stopped, i, job)
				}
			}
		}
		for _, name := range []string{"bb1", "bb2"} {
			if ph := l.bb(name).Status.Phase; ph != "Starting" {
				t.Errorf("stopped=%v: session %s is %s: %q", stopped, name, ph, l.bb(name).Status.Message)
			}
		}
	}
}

// cycleSlow is a pass in which a copy takes copyTicks passes in QEMU and the
// apply Jobs take jobTicks passes.
func (l *labEnv) cycleSlow(copyTicks, jobTicks int) {
	l.t.Helper()
	l.rc.processOnce(l.ctx)
	l.slowCopy++
	if l.slowCopy >= copyTicks {
		l.cop.state = vmcopy.Done
	} else {
		l.cop.state = vmcopy.Running
	}
	jobs := &batchv1.JobList{}
	if err := l.c.List(l.ctx, jobs, client.InNamespace(replJobNamespace)); err != nil {
		l.t.Fatal(err)
	}
	for i := range jobs.Items {
		l.jobAge[jobs.Items[i].Name]++
		if l.jobAge[jobs.Items[i].Name] >= jobTicks {
			jobs.Items[i].Status.Succeeded = 1
			if err := l.c.Status().Update(l.ctx, &jobs.Items[i]); err != nil {
				l.t.Fatal(err)
			}
		}
	}
	l.reconcile()
	l.now = l.now.Add(30 * time.Second)
	if len(l.cop.starts) != l.lastStarts {
		l.lastStarts = len(l.cop.starts)
		l.slowCopy = 0
		l.jobAge = map[string]int{}
	}
}

// A session on a disk of a running VM starts within a few passes, however long a
// copy that began before the hold takes, and no new copy starts after the hold.
func TestLab_SlowCopies(t *testing.T) {
	for copyTicks := 1; copyTicks <= 2; copyTicks++ {
		for jobTicks := 1; jobTicks <= 2; jobTicks++ {
			for warm := 0; warm < 6; warm++ {
				l := newLabEnv(t)
				l.jobAge = map[string]int{}
				for i := 0; i < warm; i++ {
					l.cycleSlow(copyTicks, jobTicks)
				}
				startsAtHold := len(l.cop.starts)
				l.openOn("bb1", "db")
				started := -1
				for i := 0; i < 14; i++ {
					l.cycleSlow(copyTicks, jobTicks)
					if started < 0 && l.bb("bb1").Status.Phase == "Starting" {
						started = i
					}
				}
				if extra := len(l.cop.starts) - startsAtHold; extra > 1 || started < 0 || started > 8 {
					t.Errorf("copy=%d job=%d warm=%d: copies after the hold=%d, session started at pass %d", copyTicks, jobTicks, warm, extra, started)
				}
			}
		}
	}
}
