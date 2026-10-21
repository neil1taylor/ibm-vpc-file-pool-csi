package main

import (
	"reflect"
	"testing"
)

func TestCOSSetupFlagsBecomeOptions(t *testing.T) {
	flags := parseFlags([]string{
		"--region", "eu-de", "--resource-group=rg", "--bucket", "b1", "--root-key", "crn:x",
		"--admin-address", "203.0.113.7, 198.51.100.0/24", "--vpc-crn", "crn:vpc", "--dry-run", "--existing-bucket",
		"--key-file", "k.yaml", "--secret-file", "s.yaml", "--cbr-mode", "report", "--keep-for", "25h", "--yes",
	})
	o := cosSetupOptions(flags)
	if o.Region != "eu-de" || o.ResourceGroup != "rg" || o.Bucket != "b1" || o.RootKeyCRN != "crn:x" || o.VPCCRN != "crn:vpc" {
		t.Errorf("basic flags not mapped: %+v", o)
	}
	if want := []string{"203.0.113.7", "198.51.100.0/24"}; !reflect.DeepEqual(o.AdminAddresses, want) {
		t.Errorf("admin addresses = %v, want %v", o.AdminAddresses, want)
	}
	if !o.DryRun || !o.ExistingBucket || !o.Yes {
		t.Errorf("boolean flags not read: dry-run=%v existing-bucket=%v yes=%v", o.DryRun, o.ExistingBucket, o.Yes)
	}
	if o.CBRMode != "report" || o.KeepFor != "25h" || o.KeyFile != "k.yaml" || o.SecretFile != "s.yaml" {
		t.Errorf("other flags not mapped: %+v", o)
	}
}

func TestBooleanFlagsDoNotTakeTheNextWord(t *testing.T) {
	flags := parseFlags([]string{"--dry-run", "--bucket", "b1", "--existing-bucket", "--region", "eu-de"})
	if flags["dry-run"] != "true" || flags["existing-bucket"] != "true" || flags["bucket"] != "b1" || flags["region"] != "eu-de" {
		t.Errorf("flags = %v", flags)
	}
}
