package pool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

const (
	// conditionReceiverReachable is the policy condition that says whether the receiver
	// answers, its certificate is trusted and it accepts the token.
	conditionReceiverReachable = "ReceiverReachable"

	// receiverCheckTimeout bounds one check, so a dead receiver cannot stall the loop.
	receiverCheckTimeout = 15 * time.Second
)

// checkReceiver tests the receiver of a receiver-mode policy and records the result as the
// ReceiverReachable condition on the policy (in memory; the caller saves the status).
// It does nothing for a direct-NFS policy. The result never stops replication.
func (rc *ReplicationController) checkReceiver(ctx context.Context, policy *v1alpha1.ReplicationPolicy) {
	if !isReceiverMode(policy) {
		return
	}
	cond := metav1.Condition{Type: conditionReceiverReachable}
	if err := rc.runReceiverCheck(ctx, policy); err != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason, cond.Message = receiverFailure(err)
	} else {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Reachable"
		cond.Message = "The receiver answers and accepts the token."
	}
	cond.ObservedGeneration = policy.Generation
	meta.SetStatusCondition(&policy.Status.Conditions, cond)
}

func (rc *ReplicationController) runReceiverCheck(ctx context.Context, policy *v1alpha1.ReplicationPolicy) error {
	token, err := rc.secretValue(ctx, policy.Spec.DestinationAuthSecretRef, "token")
	if err != nil {
		return err
	}
	var ca []byte
	if ref := policy.Spec.DestinationCACertSecretRef; ref != "" {
		ca, err = rc.secretValue(ctx, ref, "ca.crt")
		if err != nil {
			return err
		}
	}
	client, err := replication.NewSyncClientWithCA(policy.Spec.DestinationEndpoint, strings.TrimSpace(string(token)), ca)
	if err != nil {
		return &replication.CheckError{Reason: replication.ReasonBadInput, Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, receiverCheckTimeout)
	defer cancel()
	return client.Check(ctx)
}

type secretMissingError struct{ msg string }

func (e *secretMissingError) Error() string { return e.msg }

// secretValue reads one key of a Secret in the replication namespace.
func (rc *ReplicationController) secretValue(ctx context.Context, name, key string) ([]byte, error) {
	if name == "" {
		return nil, &secretMissingError{msg: fmt.Sprintf("no secret name set for the %s", key)}
	}
	s := &corev1.Secret{}
	if err := rc.directClient.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: name}, s); err != nil {
		return nil, &secretMissingError{msg: fmt.Sprintf("%s secret %s not found in %s", secretKind(key), name, replJobNamespace)}
	}
	v, ok := s.Data[key]
	if !ok || len(v) == 0 {
		return nil, &secretMissingError{msg: fmt.Sprintf("%s secret %s has no %q key", secretKind(key), name, key)}
	}
	return v, nil
}

func secretKind(key string) string {
	if key == "token" {
		return "token"
	}
	return "CA"
}

// receiverFailure turns a check error into a condition reason (one word) and plain-words message.
func receiverFailure(err error) (reason, message string) {
	var sm *secretMissingError
	if errors.As(err, &sm) {
		return "SecretMissing", sm.msg
	}
	var ce *replication.CheckError
	if errors.As(err, &ce) {
		var ne net.Error
		if ce.Reason == replication.ReasonCannotConnect && errors.As(ce.Err, &ne) && ne.Timeout() {
			return "TimedOut", "timed out"
		}
		switch ce.Reason {
		case replication.ReasonCertNotTrusted:
			return "CertificateNotTrusted", ce.Reason
		case replication.ReasonTokenRefused:
			return "TokenRefused", ce.Reason
		case replication.ReasonStorageNotReachable:
			return "StorageNotReachable", ce.Reason
		case replication.ReasonReceiverTooOld:
			return "ReceiverTooOld", ce.Reason
		case replication.ReasonBadInput:
			return "BadInput", ce.Reason
		default:
			return "CannotConnect", ce.Reason
		}
	}
	return "CannotConnect", "cannot connect"
}

// maybeCheckReceiver runs checkReceiver for a receiver-mode policy when it has no result yet
// or the last check is older than the policy's schedule, and saves the status.
func (rc *ReplicationController) maybeCheckReceiver(ctx context.Context, policy *v1alpha1.ReplicationPolicy, every time.Duration) {
	if !isReceiverMode(policy) {
		return
	}
	now := rc.nowFunc()
	rc.mu.Lock()
	last, seen := rc.lastReceiverChecks[policy.Name]
	rc.mu.Unlock()
	if seen && meta.FindStatusCondition(policy.Status.Conditions, conditionReceiverReachable) != nil && now.Sub(last) < every {
		return
	}
	rc.checkReceiver(ctx, policy)
	rc.mu.Lock()
	rc.lastReceiverChecks[policy.Name] = now
	rc.mu.Unlock()
	if err := rc.k8sClient.UpdateReplicationPolicyStatus(ctx, policy); err != nil {
		klog.ErrorS(err, "Failed to save the receiver check", "policy", policy.Name)
	}
}
