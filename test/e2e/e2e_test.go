//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Test configuration parsed from environment variables.
var (
	homeZone          string
	accessorZone      string
	accessorSubnetID  string
	resourceGroupID   string
	testNamespace     string
	k8sClient         client.Client
	testRunID         string // unique suffix for resource names
)

// Timeouts for e2e operations.
const (
	poolReadyTimeout = 3 * time.Minute
	pvcBoundTimeout  = 60 * time.Second
	podReadyTimeout  = 5 * time.Minute
	pollInterval     = 2 * time.Second
)

func TestMain(m *testing.M) {
	// Parse required env vars
	homeZone = os.Getenv("E2E_HOME_ZONE")
	accessorZone = os.Getenv("E2E_ACCESSOR_ZONE")
	accessorSubnetID = os.Getenv("E2E_ACCESSOR_SUBNET_ID")
	resourceGroupID = os.Getenv("E2E_RESOURCE_GROUP_ID")
	testNamespace = os.Getenv("E2E_NAMESPACE")

	if homeZone == "" {
		fmt.Fprintln(os.Stderr, "E2E_HOME_ZONE is required")
		os.Exit(1)
	}
	if accessorZone == "" {
		fmt.Fprintln(os.Stderr, "E2E_ACCESSOR_ZONE is required")
		os.Exit(1)
	}
	if accessorSubnetID == "" {
		fmt.Fprintln(os.Stderr, "E2E_ACCESSOR_SUBNET_ID is required")
		os.Exit(1)
	}
	if testNamespace == "" {
		testNamespace = "default"
	}

	// Unique run ID to avoid collisions
	testRunID = fmt.Sprintf("%d", time.Now().Unix())

	// Build controller-runtime client
	var err error
	k8sClient, err = buildClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create k8s client: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	// Global cleanup: delete any lingering e2e resources
	cleanup()

	os.Exit(code)
}

// buildClient creates a controller-runtime client with CRD types registered.
func buildClient() (client.Client, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("adding core/v1 to scheme: %w", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("adding storage/v1 to scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("adding v1alpha1 to scheme: %w", err)
	}

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("creating client: %w", err)
	}

	return c, nil
}

// cleanup deletes all e2e test resources that may have been left behind.
func cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	klog.InfoS("Running global e2e cleanup")

	// Delete pods first, then PVCs, then pools, then storage classes.
	// Each resource name contains "e2e-" prefix.
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(testNamespace)); err == nil {
		for i := range pods.Items {
			if isE2EResource(pods.Items[i].Name) {
				_ = k8sClient.Delete(ctx, &pods.Items[i])
			}
		}
	}

	// Wait briefly for pods to terminate before deleting PVCs
	time.Sleep(5 * time.Second)

	var pvcs corev1.PersistentVolumeClaimList
	if err := k8sClient.List(ctx, &pvcs, client.InNamespace(testNamespace)); err == nil {
		for i := range pvcs.Items {
			if isE2EResource(pvcs.Items[i].Name) {
				_ = k8sClient.Delete(ctx, &pvcs.Items[i])
			}
		}
	}

	// Delete ReplicationPolicy resources before pools
	var policies v1alpha1.ReplicationPolicyList
	if err := k8sClient.List(ctx, &policies); err == nil {
		for i := range policies.Items {
			if isE2EResource(policies.Items[i].Name) {
				_ = k8sClient.Delete(ctx, &policies.Items[i])
			}
		}
	}

	// Wait for SubVolumes to be cleaned up
	time.Sleep(10 * time.Second)

	var pools v1alpha1.FileSharePoolList
	if err := k8sClient.List(ctx, &pools); err == nil {
		for i := range pools.Items {
			if isE2EResource(pools.Items[i].Name) {
				_ = k8sClient.Delete(ctx, &pools.Items[i])
			}
		}
	}

	var scs storagev1.StorageClassList
	if err := k8sClient.List(ctx, &scs); err == nil {
		for i := range scs.Items {
			if isE2EResource(scs.Items[i].Name) {
				_ = k8sClient.Delete(ctx, &scs.Items[i])
			}
		}
	}
}

func isE2EResource(name string) bool {
	return len(name) >= 4 && name[:4] == "e2e-"
}
