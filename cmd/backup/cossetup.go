package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cossetup"
)

// cosSetupOptions turns the flags of `kubectl backup cos-setup` into options.
// The checks that need a reason (a missing flag, a bad value) are made by
// cossetup, so the same message appears whoever calls it.
func cosSetupOptions(flags map[string]string) cossetup.Options {
	var admins []string
	for _, a := range strings.Split(flags["admin-address"], ",") {
		if a = strings.TrimSpace(a); a != "" {
			admins = append(admins, a)
		}
	}
	return cossetup.Options{
		Region: flags["region"], ResourceGroup: flags["resource-group"], InstanceName: flags["instance"],
		Bucket: flags["bucket"], ExistingBucket: flags["existing-bucket"] == "true", RootKeyCRN: flags["root-key"],
		ZoneID: flags["zone-id"], VPCCRN: flags["vpc-crn"], AdminAddresses: admins, MyAddress: flags["my-address"],
		CBRMode: flags["cbr-mode"], KeepFor: flags["keep-for"], Lock: flags["lock"], Prefix: flags["prefix"],
		ExportEndpoint: flags["endpoint"], Namespace: flags["namespace"],
		CredentialsSecret: flags["credentials-secret"], BackupKeySecret: flags["backup-key-secret"],
		KeyFile: flags["key-file"], SecretFile: flags["secret-file"],
		DryRun: flags["dry-run"] == "true", Yes: flags["yes"] == "true", ConfirmKeepFor: flags["confirm-keep-for"],
	}
}

// runCOSSetup does the setup with the caller's own IBM Cloud login. It needs
// no cluster. It runs only on a person's machine, never in the driver.
func runCOSSetup(ctx context.Context, out io.Writer, in io.Reader, interactive bool, flags map[string]string) error {
	httpClient := &http.Client{Timeout: 2 * time.Minute}
	eps := cossetup.DefaultEndpoints()
	s := &cossetup.Setup{
		HTTP: httpClient, Endpoints: eps,
		Token:   cossetup.EnvironmentToken{HTTP: httpClient, IAM: eps.IAM},
		Address: cossetup.PublicAddress{HTTP: httpClient},
		In:      in, Out: out, Interactive: interactive,
	}
	return s.Run(ctx, cosSetupOptions(flags))
}
