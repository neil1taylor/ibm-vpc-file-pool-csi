package nodestats

import (
	"context"
	"errors"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func fakeTokens(t *testing.T, now *time.Time) (*ServiceAccountTokens, *int) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	calls := 0
	cs.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" {
			return false, nil, nil
		}
		calls++
		req := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		if req.Spec.ExpirationSeconds == nil || *req.Spec.ExpirationSeconds != 3600 {
			t.Errorf("expiration = %v, want 3600 s", req.Spec.ExpirationSeconds)
		}
		return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{
			Token:               "token-" + string(rune('0'+calls)),
			ExpirationTimestamp: metav1.Time{Time: now.Add(time.Hour)},
		}}, nil
	})
	return &ServiceAccountTokens{Clientset: cs, Namespace: "kube-system", Name: "reader", Now: func() time.Time { return *now }}, &calls
}

func TestServiceAccountTokens_AsksOnceAndReuses(t *testing.T) {
	now := time.Now()
	s, calls := fakeTokens(t, &now)
	a, err := s.Token(context.Background())
	if err != nil || a != "token-1" {
		t.Fatalf("got %q, %v", a, err)
	}
	now = now.Add(30 * time.Minute)
	b, _ := s.Token(context.Background())
	if b != a || *calls != 1 {
		t.Fatalf("a token that is still good is reused: %q after %d requests", b, *calls)
	}
}

func TestServiceAccountTokens_AsksAgainBeforeItEnds(t *testing.T) {
	now := time.Now()
	s, calls := fakeTokens(t, &now)
	_, _ = s.Token(context.Background())
	now = now.Add(55 * time.Minute) // within the last 10 minutes
	b, _ := s.Token(context.Background())
	if b != "token-2" || *calls != 2 {
		t.Fatalf("a token near its end is renewed: %q after %d requests", b, *calls)
	}
}

func TestServiceAccountTokens_ErrorIsAnError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	s := &ServiceAccountTokens{Clientset: cs, Namespace: "kube-system", Name: "reader", Now: time.Now}
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("a refused request must be an error, never an empty token")
	}
}
