package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// maxFileRestoreSizeGB is the largest claim a file restore may make.
	maxFileRestoreSizeGB = 32000
	// defaultMaxOpenRestores is how many restores one user may have open.
	defaultMaxOpenRestores = 10
)

func badRequest(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusBadRequest, "bad_request", msg)
}

// canDo checks one right. It answers false on an error, after logging it, so a
// failure to check is never a yes.
func (g *Gateway) canDo(ctx context.Context, u User, a Access) bool {
	ok, err := g.authz.Can(ctx, u, a)
	if err != nil {
		klog.ErrorS(err, "Rights check failed")
		return false
	}
	return ok
}

// checkRestoreRequest checks the shape of a request and returns the paths in
// their one canonical escaped form. The message names no path.
func checkRestoreRequest(req *RestoreRequest) ([]string, error) {
	for _, f := range []struct{ name, value string }{
		{"policy", req.Policy}, {"namespace", req.Namespace}, {"pvc", req.PVC},
		{"targetNamespace", req.TargetNamespace}, {"targetName", req.TargetName},
	} {
		if f.value == "" {
			return nil, fmt.Errorf("%s is required", f.name)
		}
	}
	if len(validation.IsDNS1123Subdomain(req.Policy)) > 0 || len(req.Policy) > 63 {
		return nil, fmt.Errorf("policy is not a policy name")
	}
	for _, f := range []struct{ name, value string }{{"namespace", req.Namespace}, {"targetNamespace", req.TargetNamespace}} {
		if len(validation.IsDNS1123Label(f.value)) > 0 {
			return nil, fmt.Errorf("%s is not a namespace name", f.name)
		}
	}
	for _, f := range []struct{ name, value string }{{"pvc", req.PVC}, {"targetName", req.TargetName}} {
		if len(validation.IsDNS1123Subdomain(f.value)) > 0 {
			return nil, fmt.Errorf("%s is not a claim name", f.name)
		}
	}
	if req.SizeGB < 0 || req.SizeGB > maxFileRestoreSizeGB {
		return nil, fmt.Errorf("sizeGB must be between 0 and %d", maxFileRestoreSizeGB)
	}
	if req.Existing && (req.SizeGB != 0 || req.StorageClassName != "") {
		return nil, fmt.Errorf("sizeGB and storageClassName are for a new claim; an existing claim keeps its size and class")
	}
	parsed, err := filerestore.CheckPaths(req.Paths)
	if err != nil {
		return nil, err
	}
	canonical := make([]string, len(parsed))
	for i, p := range parsed {
		canonical[i] = browse.JoinPath(p)
	}
	return canonical, nil
}

func (g *Gateway) handleRestoreCreate(w http.ResponseWriter, r *http.Request, u User) {
	var req RestoreRequest
	if err := decodeStrict(r, &req); err != nil {
		badRequest(w, "the request is not valid")
		return
	}
	paths, err := checkRestoreRequest(&req)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	ctx := r.Context()
	// The right on the source comes first: nothing is looked up for a user who
	// may not read it.
	if !g.allowed(ctx, u, req.Namespace) {
		forbidden(w)
		return
	}
	pvcs := Access{Namespace: req.TargetNamespace, Resource: "persistentvolumeclaims", Verb: "create"}
	ok := g.canDo(ctx, u, pvcs)
	if ok && req.Existing {
		pvcs.Verb = "update"
		ok = g.canDo(ctx, u, pvcs)
	}
	if !ok {
		writeError(w, http.StatusForbidden, "forbidden", "you may not create or change claims in that namespace")
		return
	}
	policy := &v1alpha1.BackupPolicy{}
	found := false
	if perr := g.c.Get(ctx, types.NamespacedName{Name: req.Policy}, policy); perr == nil {
		for _, v := range policy.Status.Volumes {
			if v.PVCNamespace == req.Namespace && v.PVCName == req.PVC && v.SubPath != "" {
				found = true
			}
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no backup of that volume")
		return
	}
	spec := v1alpha1.BackupFileRestoreSpec{
		Policy: req.Policy, SourceNamespace: req.Namespace, SourceName: req.PVC, Paths: paths,
		TargetNamespace: req.TargetNamespace, TargetName: req.TargetName, Existing: req.Existing,
		SizeGB: req.SizeGB, StorageClassName: req.StorageClassName, RequestedBy: u.Name,
	}
	if req.Moment != nil {
		m := metav1.NewTime(*req.Moment)
		spec.Moment = &m
	}
	target := &corev1.PersistentVolumeClaim{}
	err = g.c.Get(ctx, types.NamespacedName{Namespace: req.TargetNamespace, Name: req.TargetName}, target)
	switch {
	case err != nil && !apierrors.IsNotFound(err):
		klog.ErrorS(err, "Get the target claim failed")
		writeError(w, http.StatusInternalServerError, "internal", "cannot look at the target claim")
		return
	case req.Existing && err != nil:
		writeError(w, http.StatusNotFound, "not_found", "no such claim")
		return
	case req.Existing:
		// The claim the user's rights were checked for. The restore refuses a claim
		// that was deleted and made again since.
		spec.TargetUID = target.UID
	case err == nil:
		writeError(w, http.StatusConflict, "exists", "that claim exists; a restore into a new claim never writes over one")
		return
	}
	fr, err := g.createInSlot(ctx, u, spec)
	if errors.Is(err, errNoSlot) {
		writeError(w, http.StatusTooManyRequests, "too_many", fmt.Sprintf("you have %d restores that have not ended; the limit is %d", g.cfg.MaxOpenRestores, g.cfg.MaxOpenRestores))
		return
	}
	if err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			badRequest(w, "the restore was refused: "+firstLine(err.Error()))
			return
		}
		klog.ErrorS(err, "Make a file restore failed")
		writeError(w, http.StatusInternalServerError, "internal", "cannot make the restore")
		return
	}
	where := "a new claim " + req.TargetNamespace + "/" + req.TargetName
	if req.Existing {
		where = "a new folder in the claim " + req.TargetNamespace + "/" + req.TargetName
	}
	g.auditObject(ctx, "BackupFileRestore", fr.Name, fr.UID, "FileRestoreRequested",
		fmt.Sprintf("user %s asked to restore %s from %s/%s of policy %s, %s, into %s",
			u.Name, strings.Join(paths, ", "), req.Namespace, req.PVC, req.Policy, momentText(req.Moment), where))
	writeJSON(w, http.StatusAccepted, restoreOf(fr))
}

var errNoSlot = errors.New("all restore slots of the user are taken")

// createInSlot makes the restore in one of the user's numbered slots. The name
// of a restore is bfr-<user hash>-<slot>, so the API server refuses a second
// object in a slot that is taken, however many requests or gateway replicas
// arrive at once. A slot whose restore has ended is freed and used again.
func (g *Gateway) createInSlot(ctx context.Context, u User, spec v1alpha1.BackupFileRestoreSpec) (*v1alpha1.BackupFileRestore, error) {
	hash := userHash(u.Name)[:16]
	for slot := 0; slot < g.cfg.MaxOpenRestores; slot++ {
		name := fmt.Sprintf("bfr-%s-%d", hash, slot)
		for attempt := 0; attempt < 2; attempt++ {
			fr := &v1alpha1.BackupFileRestore{
				ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{userLabel: userHash(u.Name)}},
				Spec:       spec,
			}
			err := g.c.Create(ctx, fr)
			if err == nil {
				return fr, nil
			}
			if !apierrors.IsAlreadyExists(err) {
				return nil, err
			}
			old := &v1alpha1.BackupFileRestore{}
			if gerr := g.c.Get(ctx, types.NamespacedName{Name: name}, old); gerr != nil {
				if apierrors.IsNotFound(gerr) {
					continue
				}
				return nil, gerr
			}
			if old.Status.Phase != "Done" && old.Status.Phase != "Failed" {
				break // taken by a restore that has not ended: try the next slot
			}
			uid := old.UID
			if derr := g.c.Delete(ctx, old, client.Preconditions{UID: &uid}); derr != nil && !apierrors.IsNotFound(derr) {
				break
			}
		}
	}
	return nil, errNoSlot
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func restoreOf(fr *v1alpha1.BackupFileRestore) Restore {
	st := fr.Status
	phase := st.Phase
	if phase == "" {
		phase = "Pending"
	}
	out := Restore{
		Name: fr.Name, Phase: phase, Message: st.Message,
		TargetNamespace: fr.Spec.TargetNamespace, TargetName: fr.Spec.TargetName, Existing: fr.Spec.Existing,
		Folder: st.Folder, SizeGB: st.SizeGB, Latest: st.Latest,
		Files: st.Files, Dirs: st.Dirs, Links: st.Links, Skipped: st.Skipped, Unreadable: st.Unreadable, Bytes: st.Bytes,
	}
	if st.RestoredMoment != nil {
		t := st.RestoredMoment.UTC()
		out.RestoredMoment = &t
	}
	return out
}

// handleRestoreStatus shows a restore to the user who asked for it. Another
// user, and a restore that does not exist, look the same. Then the user's right
// on the source namespace is checked again.
func (g *Gateway) handleRestoreStatus(w http.ResponseWriter, r *http.Request, u User) {
	fr := &v1alpha1.BackupFileRestore{}
	err := g.c.Get(r.Context(), types.NamespacedName{Name: r.PathValue("name")}, fr)
	if err != nil || fr.Spec.RequestedBy != u.Name || fr.Labels[userLabel] != userHash(u.Name) {
		if err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Get a file restore failed")
		}
		writeError(w, http.StatusNotFound, "not_found", "no such restore")
		return
	}
	if !g.allowed(r.Context(), u, fr.Spec.SourceNamespace) {
		forbidden(w)
		return
	}
	writeJSON(w, http.StatusOK, restoreOf(fr))
}
