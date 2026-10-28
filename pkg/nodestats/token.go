package nodestats

import (
	"context"
	"fmt"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	tokenLifetime = time.Hour
	// tokenRenewBefore is how long before it ends a token is replaced.
	tokenRenewBefore = 10 * time.Minute
)

// ServiceAccountTokens makes short-lived tokens for one service account that
// may only read the node agents' metrics. A caller sends such a token to the
// node agents instead of its own, so a node agent that a person has taken over
// learns nothing that opens the API server.
type ServiceAccountTokens struct {
	Clientset kubernetes.Interface
	Namespace string
	Name      string
	Now       func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token is a token that is good for at least another ten minutes.
func (s *ServiceAccountTokens) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	if s.token != "" && now.Before(s.expires.Add(-tokenRenewBefore)) {
		return s.token, nil
	}
	seconds := int64(tokenLifetime.Seconds())
	res, err := s.Clientset.CoreV1().ServiceAccounts(s.Namespace).CreateToken(ctx, s.Name,
		&authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("request a token for service account %s/%s: %w", s.Namespace, s.Name, err)
	}
	s.token, s.expires = res.Status.Token, res.Status.ExpirationTimestamp.Time
	return s.token, nil
}
