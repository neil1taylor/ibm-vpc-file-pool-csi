package cossetup

import (
	"strings"
	"testing"
)

func TestBadRegionIsRefusedAndNothingIsSent(t *testing.T) {
	for _, region := range []string{"x.evil.com/", "eu-de@evil.com", "eu/de", "eu-de:443", "eu de", "EU-DE", "", "eu-de.evil.com"} {
		e := newEnv(t, testSaved)
		e.opts.Region = region
		if err := e.run(); err == nil {
			t.Errorf("region %q was accepted", region)
		}
		if e.fake.requests != 0 {
			t.Errorf("region %q: %d request(s) were sent", region, e.fake.requests)
		}
	}
}

func TestBadRootKeyRegionIsRefusedAndNothingIsSent(t *testing.T) {
	for _, crn := range []string{
		"crn:v1:bluemix:public:kms:evil.com/:a/acct1:kpinst1:key:rootkey1",
		"crn:v1:bluemix:public:kms:eu-de@evil.com:a/acct1:kpinst1:key:rootkey1",
		"crn:v1:bluemix:public:kms:eu de:a/acct1:kpinst1:key:rootkey1",
	} {
		e := newEnv(t, testSaved)
		e.opts.RootKeyCRN = crn
		if err := e.run(); err == nil {
			t.Errorf("CRN %q was accepted", crn)
		}
		if e.fake.requests != 0 {
			t.Errorf("CRN %q: %d request(s) were sent", crn, e.fake.requests)
		}
	}
}

func TestForeignHostGetsNoCredentials(t *testing.T) {
	e := newEnv(t, testSaved)
	e.setup.allowAnyHost = false
	// Every endpoint points at the fake (127.0.0.1), which is not an IBM Cloud host.
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "refusing to send credentials") {
		t.Fatalf("want a refusal of the host, got %v", err)
	}
	if e.fake.requests != 0 {
		t.Errorf("%d request(s) reached a host that is not IBM Cloud", e.fake.requests)
	}
}

func TestHostCheck(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://s3.eu-de.cloud-object-storage.appdomain.cloud/b":      true,
		"https://eu-de.kms.cloud.ibm.com/api/v2/keys/x":                true,
		"https://iam.cloud.ibm.com/v1/policies":                        true,
		"http://iam.cloud.ibm.com/v1/policies":                         false,
		"https://evil.com/x":                                           false,
		"https://iam.cloud.ibm.com.evil.com/x":                         false,
		"https://evil.com/.cloud.ibm.com":                              false,
		"https://iam.cloud.ibm.com@evil.com/x":                         false,
		"https://evilcloud.ibm.com/x":                                  false,
		"https://s3.eu-de.cloud-object-storage.appdomain.cloud:8443/x": true,
		"https://127.0.0.1/x":                                          false,
	} {
		if got := checkHost(raw) == nil; got != ok {
			t.Errorf("checkHost(%q) ok = %v, want %v", raw, got, ok)
		}
	}
}
