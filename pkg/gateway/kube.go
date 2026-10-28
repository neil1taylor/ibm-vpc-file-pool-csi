package gateway

import (
	"context"
	"errors"
	"fmt"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// KubeAuth checks tokens and rights with the Kubernetes API.
type KubeAuth struct{ cs kubernetes.Interface }

// NewKubeAuth makes a KubeAuth that asks the API through cs.
func NewKubeAuth(cs kubernetes.Interface) *KubeAuth { return &KubeAuth{cs: cs} }

// Authenticate asks for a token review. A token that is not valid gives
// ErrUnauthenticated; a failure of the API gives another error.
func (k *KubeAuth) Authenticate(ctx context.Context, token string) (User, error) {
	tr, err := k.cs.AuthenticationV1().TokenReviews().Create(ctx,
		&authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token}}, metav1.CreateOptions{})
	if err != nil {
		return User{}, fmt.Errorf("token review: %w", err)
	}
	if !tr.Status.Authenticated {
		return User{}, ErrUnauthenticated
	}
	u := User{Name: tr.Status.User.Username, UID: tr.Status.User.UID, Groups: tr.Status.User.Groups}
	if len(tr.Status.User.Extra) > 0 {
		u.Extra = map[string][]string{}
		for k, v := range tr.Status.User.Extra {
			u.Extra[k] = []string(v)
		}
	}
	if u.Name == "" {
		return User{}, ErrUnauthenticated
	}
	return u, nil
}

// CanReadBackups asks a SubjectAccessReview, as the user, for "get backupfiles"
// in the namespace. The resource is a rights-only name: no object of that kind
// exists.
func (k *KubeAuth) CanReadBackups(ctx context.Context, u User, namespace string) (bool, error) {
	if namespace == "" {
		return false, errors.New("a namespace is required")
	}
	spec := authzv1.SubjectAccessReviewSpec{
		ResourceAttributes: &authzv1.ResourceAttributes{Namespace: namespace, Verb: RightsVerb, Group: RightsGroup, Resource: RightsResource},
		User:               u.Name, UID: u.UID, Groups: u.Groups,
	}
	if len(u.Extra) > 0 {
		spec.Extra = map[string]authzv1.ExtraValue{}
		for k, v := range u.Extra {
			spec.Extra[k] = authzv1.ExtraValue(v)
		}
	}
	sar, err := k.cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{Spec: spec}, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("access review: %w", err)
	}
	return sar.Status.Allowed, nil
}
