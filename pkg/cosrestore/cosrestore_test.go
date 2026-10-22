package cosrestore

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cossetup"
)

const testToken = "SENTINEL-IAM-TOKEN"

// fakeBucket answers the few S3 calls that this package makes and records every
// write.
type fakeBucket struct {
	mu      sync.Mutex
	deletes []string // "key?versionId" of each DELETE
	markers []marker // delete markers; removed when deleted
	pages   int      // list pages served
	wrapped string
	unauth  bool
}

type marker struct{ key, version string }

func (f *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testToken || f.unauth {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Query().Has("versions"):
		f.pages++
		prefix := r.URL.Query().Get("prefix")
		var b strings.Builder
		b.WriteString(`<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name>`)
		// Serve one marker per page to prove paging works.
		start := 0
		if km := r.URL.Query().Get("key-marker"); km != "" {
			for i, m := range f.markers {
				if m.key == km {
					start = i + 1
				}
			}
		}
		shown := 0
		for i := start; i < len(f.markers) && shown < 1; i++ {
			m := f.markers[i]
			if !strings.HasPrefix(m.key, prefix) {
				continue
			}
			shown++
			fmt.Fprintf(&b, `<DeleteMarker><Key>%s</Key><VersionId>%s</VersionId><IsLatest>true</IsLatest></DeleteMarker>`, m.key, m.version)
			if i+1 < len(f.markers) {
				fmt.Fprintf(&b, `<IsTruncated>true</IsTruncated><NextKeyMarker>%s</NextKeyMarker><NextVersionIdMarker>%s</NextVersionIdMarker>`, m.key, m.version)
			}
		}
		if !strings.Contains(b.String(), "IsTruncated") {
			b.WriteString(`<IsTruncated>false</IsTruncated>`)
		}
		fmt.Fprintf(&b, `<Version><Key>%skeep-me</Key><VersionId>v1</VersionId><IsLatest>false</IsLatest></Version>`, prefix)
		b.WriteString(`</ListVersionsResult>`)
		_, _ = w.Write([]byte(b.String()))
	case r.Method == http.MethodDelete:
		f.deletes = append(f.deletes, strings.TrimPrefix(r.URL.Path, "/bucket/")+"?"+r.URL.RawQuery)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+cossetup.WrappedKeyObject):
		if f.wrapped == "" {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `{"version":1,"wrappedKey":%q,"rootKeyCRN":"crn:x"}`, f.wrapped)
	default:
		http.Error(w, "unexpected", http.StatusTeapot)
	}
}

func newTestClient(t *testing.T, f *fakeBucket) (*Client, string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Token: cossetup.StaticToken(testToken), allowAnyHost: true}, srv.URL
}

func TestRemoveDeleteMarkers_RemovesOnlyMarkersByVersionAcrossPages(t *testing.T) {
	f := &fakeBucket{markers: []marker{{"team-a/nightly/blob1", "m1"}, {"team-a/nightly/blob2", "m2"}, {"other/blob3", "m3"}}}
	c, base := newTestClient(t, f)
	n, err := c.RemoveDeleteMarkers(context.Background(), base, "bucket", "team-a/nightly/")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed %d markers, want 2 (the area of the policy only)", n)
	}
	if f.pages < 2 {
		t.Errorf("served %d pages, want the second page read", f.pages)
	}
	want := map[string]bool{"team-a/nightly/blob1?versionId=m1": true, "team-a/nightly/blob2?versionId=m2": true}
	for _, d := range f.deletes {
		if !want[d] {
			t.Errorf("unexpected DELETE %q: only delete markers of the area, by version, may be deleted", d)
		}
		delete(want, d)
	}
	if len(want) != 0 {
		t.Errorf("missing DELETEs: %v", want)
	}
}

func TestRemoveDeleteMarkers_NothingToDo(t *testing.T) {
	f := &fakeBucket{}
	c, base := newTestClient(t, f)
	n, err := c.RemoveDeleteMarkers(context.Background(), base, "bucket", "p/")
	if err != nil || n != 0 || len(f.deletes) != 0 {
		t.Errorf("n=%d err=%v deletes=%v, want nothing", n, err, f.deletes)
	}
}

func TestRemoveDeleteMarkers_RefusesAnEmptyPrefix(t *testing.T) {
	f := &fakeBucket{markers: []marker{{"a", "m1"}}}
	c, base := newTestClient(t, f)
	if _, err := c.RemoveDeleteMarkers(context.Background(), base, "bucket", ""); err == nil {
		t.Error("an empty prefix would clear the whole bucket; it must be refused")
	}
}

func TestRemoveDeleteMarkers_ErrorsNeverHoldTheToken(t *testing.T) {
	f := &fakeBucket{unauth: true}
	c, base := newTestClient(t, f)
	_, err := c.RemoveDeleteMarkers(context.Background(), base, "bucket", "p/")
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("err = %v; want an error without the token", err)
	}
}

func TestReadWrappedKey(t *testing.T) {
	f := &fakeBucket{wrapped: "WRAPPED-CIPHERTEXT"}
	c, base := newTestClient(t, f)
	got, err := c.ReadWrappedKey(context.Background(), base, "bucket")
	if err != nil || got != "WRAPPED-CIPHERTEXT" {
		t.Fatalf("ReadWrappedKey() = %q, %v", got, err)
	}
	f.wrapped = ""
	if _, err = c.ReadWrappedKey(context.Background(), base, "bucket"); err == nil || !strings.Contains(err.Error(), "cos-setup") {
		t.Errorf("err = %v; want a message that points to cos-setup and the Secret", err)
	}
}

func TestClient_NeverSendsTheTokenToANonIBMHost(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Token: cossetup.StaticToken(testToken)}
	for _, endpoint := range []string{srv.URL, "https://evil.example.com", "http://s3.eu-de.cloud-object-storage.appdomain.cloud"} {
		if _, err := c.ReadWrappedKey(context.Background(), endpoint, "bucket"); err == nil {
			t.Errorf("endpoint %s: accepted", endpoint)
		}
	}
	if called {
		t.Error("a request reached a host that is not on an IBM Cloud domain")
	}
}

func TestBaseURL(t *testing.T) {
	c := &Client{}
	for in, want := range map[string]string{
		"s3.eu-de.cloud-object-storage.appdomain.cloud":         "https://s3.eu-de.cloud-object-storage.appdomain.cloud/mybucket",
		"https://s3.eu-de.cloud-object-storage.appdomain.cloud": "https://s3.eu-de.cloud-object-storage.appdomain.cloud/mybucket",
	} {
		got, err := c.bucketURL(in, "mybucket")
		if err != nil || got != want {
			t.Errorf("bucketURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := c.bucketURL("bad host/x", "mybucket"); err == nil {
		t.Error("accepted a host with a slash")
	}
	_ = url.URL{}
}
