package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/keyprotect"
	"k8s.io/klog/v2"
)

// unwrapOptions are the flags of unwrap-backup-key mode. The Secrets are files:
// the mode never reads the environment, and the key is never printed.
type unwrapOptions struct {
	region, instance, rootKey                            string
	wrappedKeyFile, apiKeyFile, emergencyKeyFile, keyOut string
}

// unwrapBackupKey asks Key Protect to unwrap the backup key and writes the
// readable key to keyOut (mode 0400) and nowhere else.
func unwrapBackupKey(ctx context.Context, o unwrapOptions,
	newClient func(keyprotect.Config) (keyprotect.Unwrapper, error), log io.Writer) error {
	if o.region == "" || o.instance == "" || o.rootKey == "" || o.wrappedKeyFile == "" || o.apiKeyFile == "" || o.keyOut == "" {
		return errors.New("kp-region, kp-instance, kp-root-key, wrapped-key-file, api-key-file and key-out are required")
	}
	apiKey, err := os.ReadFile(o.apiKeyFile)
	if err != nil {
		return fmt.Errorf("reading the API key file: %w", err)
	}
	u, err := newClient(keyprotect.Config{
		Region: o.region, InstanceID: o.instance, RootKeyID: o.rootKey, APIKey: strings.TrimSpace(string(apiKey)),
	})
	if err != nil {
		return err
	}
	return keyprotect.UnwrapToFile(ctx, u, keyprotect.FileOptions{
		WrappedKeyFile: o.wrappedKeyFile, EmergencyKeyFile: o.emergencyKeyFile, OutFile: o.keyOut,
	}, log)
}

// runUnwrapBackupKey is the unwrap-backup-key mode. An export, maintenance or
// restore Job runs it before Kopia.
func runUnwrapBackupKey(o unwrapOptions) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	newClient := func(cfg keyprotect.Config) (keyprotect.Unwrapper, error) { return keyprotect.New(cfg) }
	if err := unwrapBackupKey(ctx, o, newClient, os.Stderr); err != nil {
		klog.ErrorS(err, "Could not get the backup key")
		os.Exit(1)
	}
}
