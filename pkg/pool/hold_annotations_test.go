package pool

import (
	"context"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// The holds of a SubVolume are annotations written by several writers: the
// export, the browse sessions and the file restores. A writer must name only
// the annotation it owns. A merge patch made from a copy whose last annotation
// was removed says "annotations: null" and wipes the ones another writer added
// since the copy was read.

// racingBackupEnv returns a backup environment whose first SubVolume patch is
// preceded by another writer adding the given annotation to the SubVolume.
func racingBackupEnv(t *testing.T, svName, key, value string) *backupEnv {
	t.Helper()
	e := newBackupEnv(t)
	inner, ok := e.c.(client.WithWatch)
	if !ok {
		t.Fatal("the fake client cannot be wrapped")
	}
	raced := false
	wrapped := interceptor.NewClient(inner, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isSV := obj.(*v1alpha1.SubVolume); isSV && !raced {
				raced = true
				other := &v1alpha1.SubVolume{}
				if err := c.Get(ctx, client.ObjectKey{Name: svName}, other); err != nil {
					t.Fatal(err)
				}
				if other.Annotations == nil {
					other.Annotations = map[string]string{}
				}
				other.Annotations[key] = value
				if err := c.Update(ctx, other); err != nil {
					t.Fatal(err)
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	e.c = wrapped
	e.bc = NewBackupController(wrapped)
	e.bc.SetNowFunc(func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) })
	return e
}

func TestExportHold_ReleaseKeepsABrowseHoldAddedMeanwhile(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	hold := addBrowseHold("", "uid-bb1", now)
	e := racingBackupEnv(t, "pvc-apps-db", browseHoldAnnotation, hold)
	sv := e.addClaim("apps", "db", "data-a", yes)
	sv.Annotations = map[string]string{exportHoldAnnotation: "nightly/" + now.Format(time.RFC3339)}
	if err := e.c.Update(e.ctx, sv); err != nil {
		t.Fatal(err)
	}

	e.bc.releaseExportHold(e.ctx, "nightly", sv.Name)

	got := e.sv(sv.Name).Annotations
	if got[browseHoldAnnotation] != hold {
		t.Errorf("the browse hold added while the export hold was released is gone: %v", got)
	}
	if _, still := got[exportHoldAnnotation]; still {
		t.Errorf("the export hold was not released: %v", got)
	}
}

func TestBrowseHold_SweepKeepsAnExportHoldAddedMeanwhile(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	exportHold := "nightly/" + now.Format(time.RFC3339)
	e := racingBackupEnv(t, "pvc-apps-db", exportHoldAnnotation, exportHold)
	sv := e.addClaim("apps", "db", "data-a", yes)
	// The only hold is a session that no longer exists, so the sweep removes it.
	sv.Annotations = map[string]string{browseHoldAnnotation: addBrowseHold("", "uid-gone", now)}
	if err := e.c.Update(e.ctx, sv); err != nil {
		t.Fatal(err)
	}

	e.bc.sweepBrowseHolds(e.ctx)
	// The pass that met the other writer lets the SubVolume be; the next one
	// removes the hold.
	e.bc.sweepBrowseHolds(e.ctx)

	got := e.sv(sv.Name).Annotations
	if got[exportHoldAnnotation] != exportHold {
		t.Errorf("the export hold added while the sweep ran is gone: %v", got)
	}
	if _, still := got[browseHoldAnnotation]; still {
		t.Errorf("the browse hold of a session that is gone was kept: %v", got)
	}
}

func TestBrowseHold_AddKeepsAHoldAddedMeanwhile(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	other := addBrowseHold("", "uid-other", now)
	e := racingBackupEnv(t, "pvc-apps-db", exportHoldAnnotation, "nightly/"+now.Format(time.RFC3339))
	e.addClaim("apps", "db", "data-a", yes)
	// Another session holds the volume already, then the first writer adds its own
	// annotation between the read and the write of this one.
	sv := e.sv("pvc-apps-db")
	sv.Annotations = map[string]string{browseHoldAnnotation: other}
	if err := e.c.Update(e.ctx, sv); err != nil {
		t.Fatal(err)
	}

	e.bc.holdForGuest(e.ctx, "uid-bb1", "nightly", sv.Name, now, true)

	got := e.sv(sv.Name).Annotations
	if _, ok := got[exportHoldAnnotation]; !ok {
		t.Errorf("an annotation added while the hold was written is gone: %v", got)
	}
	holds := parseBrowseHolds(got[browseHoldAnnotation])
	if len(holds) != 2 {
		t.Errorf("holds = %q, want the other session's and this one's", got[browseHoldAnnotation])
	}
}
