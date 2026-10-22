package cosrestore

import "testing"

// The token goes only to a COS S3 endpoint, not to any host on appdomain.cloud
// (other services and customer apps live there too).
func TestBucketURL_OnlyCOSS3Endpoints(t *testing.T) {
	c := &Client{}
	good := []string{
		"s3.eu-de.cloud-object-storage.appdomain.cloud",
		"s3.private.eu-de.cloud-object-storage.appdomain.cloud",
		"s3.direct.us-south.cloud-object-storage.appdomain.cloud",
		"https://s3.eu-gb.cloud-object-storage.appdomain.cloud",
	}
	bad := []string{
		"evil.appdomain.cloud",
		"s3.eu-de.cloud-object-storage.appdomain.cloud.evil.com",
		"evil.s3.eu-de.cloud-object-storage.appdomain.cloud",
		"s3.eu-de.cloud-object-storage.appdomain.cloud:8443",
		"foo.cloud-object-storage.appdomain.cloud",
		"s3.cloud.ibm.com",
	}
	for _, e := range good {
		if _, err := c.bucketURL(e, "b"); err != nil {
			t.Errorf("%s refused: %v", e, err)
		}
	}
	for _, e := range bad {
		if _, err := c.bucketURL(e, "b"); err == nil {
			t.Errorf("%s accepted", e)
		}
	}
}
