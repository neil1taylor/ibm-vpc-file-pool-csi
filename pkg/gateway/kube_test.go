package gateway

import (
	"context"
	"errors"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestKubeAuthenticate(t *testing.T) {
	cs := fake.NewClientset()
	var seen string
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr, _ := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		seen = tr.Spec.Token
		switch tr.Spec.Token {
		case "good":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{
				Username: "alice", UID: "u1", Groups: []string{"devs", "system:authenticated"},
				Extra: map[string]authnv1.ExtraValue{"scopes": {"a", "b"}}}}
		case "broken":
			return true, nil, errors.New("api down")
		default:
			tr.Status = authnv1.TokenReviewStatus{Authenticated: false}
		}
		return true, tr, nil
	})
	k := NewKubeAuth(cs)
	u, err := k.Authenticate(context.Background(), "good")
	if err != nil || seen != "good" {
		t.Fatalf("good: %v (token seen %q)", err, seen)
	}
	if u.Name != "alice" || u.UID != "u1" || len(u.Groups) != 2 || len(u.Extra["scopes"]) != 2 {
		t.Errorf("user = %+v", u)
	}
	if _, err := k.Authenticate(context.Background(), "bad"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("bad token: %v", err)
	}
	// A failure of the API is not a "no" that can be mistaken for a bad token, and never a yes.
	if _, err := k.Authenticate(context.Background(), "broken"); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Errorf("api error: %v", err)
	}
}

func TestKubeCanReadBackups(t *testing.T) {
	cs := fake.NewClientset()
	var got authzv1.SubjectAccessReviewSpec
	allowed := true
	cs.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sar, _ := a.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		got = sar.Spec
		sar.Status = authzv1.SubjectAccessReviewStatus{Allowed: allowed}
		return true, sar, nil
	})
	k := NewKubeAuth(cs)
	u := User{Name: "alice", UID: "u1", Groups: []string{"devs"}, Extra: map[string][]string{"scopes": {"a"}}}
	ok, err := k.CanReadBackups(context.Background(), u, "apps")
	if err != nil || !ok {
		t.Fatalf("allowed: %v %v", ok, err)
	}
	ra := got.ResourceAttributes
	if ra == nil || ra.Namespace != "apps" || ra.Verb != "get" || ra.Group != "storage.ibmcloud.io" || ra.Resource != "backupfiles" || ra.Name != "" {
		t.Errorf("attributes = %+v", ra)
	}
	if got.User != "alice" || got.UID != "u1" || len(got.Groups) != 1 || got.Groups[0] != "devs" || len(got.Extra["scopes"]) != 1 {
		t.Errorf("subject = %+v", got)
	}
	allowed = false
	if ok, _ := k.CanReadBackups(context.Background(), u, "apps"); ok {
		t.Error("a denied review was taken as allowed")
	}
	// An error is never a yes.
	cs2 := fake.NewClientset()
	cs2.PrependReactor("create", "subjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	if ok, err := NewKubeAuth(cs2).CanReadBackups(context.Background(), u, "apps"); ok || err == nil {
		t.Errorf("api error: %v %v", ok, err)
	}
	// An empty namespace is never asked: cluster-wide rights are not what this means.
	if ok, err := k.CanReadBackups(context.Background(), u, ""); ok || err == nil {
		t.Errorf("empty namespace: %v %v", ok, err)
	}
}
