package v1alpha1

import "testing"

func TestRestoreFromCOS_EndpointMustBeACOSS3Host(t *testing.T) {
	for _, e := range []string{"evil.appdomain.cloud", "s3.eu-de.cloud-object-storage.appdomain.cloud.evil.com", "10.0.0.1", "localhost:9000"} {
		r := validFromCOS()
		r.Endpoint = e
		if err := r.Validate(); err == nil {
			t.Errorf("endpoint %s accepted", e)
		}
	}
	for _, e := range []string{"s3.eu-de.cloud-object-storage.appdomain.cloud", "https://s3.private.eu-de.cloud-object-storage.appdomain.cloud"} {
		r := validFromCOS()
		r.Endpoint = e
		if err := r.Validate(); err != nil {
			t.Errorf("endpoint %s refused: %v", e, err)
		}
	}
}
