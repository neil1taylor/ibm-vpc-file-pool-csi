package cossetup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnvironmentTokenExchangesAnAPIKey(t *testing.T) {
	var gotKey, gotGrant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotKey, gotGrant = r.PostForm.Get("apikey"), r.PostForm.Get("grant_type")
		_, _ = w.Write([]byte(`{"access_token":"tok-1"}`))
	}))
	defer srv.Close()
	t.Setenv("IBMCLOUD_IAM_TOKEN", "")
	t.Setenv("IBMCLOUD_API_KEY", "my-api-key")
	tok, err := EnvironmentToken{HTTP: srv.Client(), IAM: srv.URL}.Token(context.Background())
	if err != nil || tok != "tok-1" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if gotKey != "my-api-key" || gotGrant != "urn:ibm:params:oauth:grant-type:apikey" {
		t.Errorf("exchange sent apikey=%q grant=%q", gotKey, gotGrant)
	}
}

func TestEnvironmentTokenUsesAnIAMToken(t *testing.T) {
	t.Setenv("IBMCLOUD_IAM_TOKEN", "Bearer abc")
	tok, err := EnvironmentToken{}.Token(context.Background())
	if err != nil || tok != "abc" {
		t.Fatalf("token = %q, %v", tok, err)
	}
}

func TestZoneHolds(t *testing.T) {
	zone := []zoneAddr{{"vpc", "crn:vpc"}, {"ipAddress", "203.0.113.7"}, {"subnet", "198.51.100.0/24"}, {"ipRange", "192.0.2.10-192.0.2.20"}}
	for ip, want := range map[string]bool{
		"203.0.113.7": true, "203.0.113.8": false, "198.51.100.200": true, "192.0.2.15": true, "192.0.2.21": false, "": false,
	} {
		if got := zoneHolds(zone, ip); got != want {
			t.Errorf("zoneHolds(%q) = %v, want %v", ip, got, want)
		}
	}
}
