package cosdisk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseRestorePointFolder(t *testing.T) {
	good := map[string]time.Time{
		"20261106T180326Z": time.Date(2026, 11, 6, 18, 3, 26, 0, time.UTC),
		"20240229T000000Z": time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC),
	}
	for name, want := range good {
		got, ok := ParseRestorePointFolder(name)
		if !ok || !got.Equal(want) {
			t.Errorf("%q = %v, %v; want %v", name, got, ok, want)
		}
	}
	for _, name := range []string{
		"", ".", "..", "20261106T180326Z/../x", "20261106T180326Z/", "/20261106T180326Z",
		"20261106T180326", "20261106t180326Z", "2026110T180326Z", "20261306T180326Z",
		"20230229T000000Z", "20261106T250326Z", "20261106T180326Zx", " 20261106T180326Z",
		"2026-11-06T18:03:26Z", "20261106T180326Z\n", "２０２６1106T180326Z", "disk.img",
	} {
		if got, ok := ParseRestorePointFolder(name); ok {
			t.Errorf("%q must be refused, got %v", name, got)
		}
	}
}

func davList(t *testing.T, status int, hrefs ...string) (*httptest.Server, *int) {
	t.Helper()
	calls := new(int)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Method != "PROPFIND" || r.URL.Path != "/" || r.Header.Get("Depth") != "1" {
			t.Errorf("unexpected request %s %s depth %q", r.Method, r.URL.Path, r.Header.Get("Depth"))
		}
		if status != http.StatusMultiStatus {
			w.WriteHeader(status)
			return
		}
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">`)
		for _, h := range hrefs {
			fmt.Fprintf(&b, `<D:response><D:href>%s</D:href></D:response>`, h)
		}
		b.WriteString(`</D:multistatus>`)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(ts.Close)
	return ts, calls
}

func TestListRestorePointFolders_OneListingSortedAndStrict(t *testing.T) {
	ts, calls := davList(t, http.StatusMultiStatus,
		"/", "/20261106T180326Z/", "/20261105T090000Z/", "/disk.img", "/..", "/20261106T180326Z/../x/",
		"/20261301T000000Z/", "/20261104T120000Z/disk.img", "http://evil.example/20261103T000000Z/", "/20261107T010203Z", "/%32%30261102T000000Z/")
	got, err := ListRestorePointFolders(context.Background(), ts.Client(), ts.URL+"/disk.img")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got {
		names = append(names, f.Name)
	}
	// An absolute href is read by its path only; the request never leaves 127.0.0.1.
	want := []string{"20261102T000000Z", "20261103T000000Z", "20261105T090000Z", "20261106T180326Z", "20261107T010203Z"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("folders = %v, want %v", names, want)
	}
	if *calls != 1 {
		t.Errorf("%d requests, want 1", *calls)
	}
}

func TestListRestorePointFolders_MissingOrUnlistableIsNone(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		ts, _ := davList(t, status)
		got, err := ListRestorePointFolders(context.Background(), ts.Client(), ts.URL+"/disk.img")
		if err != nil || len(got) != 0 {
			t.Errorf("status %d: %v, %v; want none and no error", status, got, err)
		}
	}
	ts, _ := davList(t, http.StatusMultiStatus, "/")
	if got, err := ListRestorePointFolders(context.Background(), ts.Client(), ts.URL+"/disk.img"); err != nil || len(got) != 0 {
		t.Errorf("empty listing: %v, %v", got, err)
	}
	ts, _ = davList(t, http.StatusInternalServerError)
	if _, err := ListRestorePointFolders(context.Background(), ts.Client(), ts.URL+"/disk.img"); err == nil {
		t.Error("a server error must be an error, so that the listing is tried again")
	}
}

func TestListRestorePointFolders_HugeListIsRefused(t *testing.T) {
	hrefs := make([]string, 0, maxFolders+2)
	for i := 0; i < maxFolders+2; i++ {
		hrefs = append(hrefs, fmt.Sprintf("/x%d/", i))
	}
	ts, _ := davList(t, http.StatusMultiStatus, hrefs...)
	if _, err := ListRestorePointFolders(context.Background(), ts.Client(), ts.URL+"/disk.img"); err == nil {
		t.Error("a list of more than the limit must be refused")
	}
}

func TestListRestorePointFolders_OversizedBodyIsRefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<D:multistatus xmlns:D="DAV:">`))
		pad := strings.Repeat(" ", 1<<20)
		for i := 0; i < 20; i++ {
			_, _ = w.Write([]byte(pad))
		}
	}))
	defer ts.Close()
	if _, err := ListRestorePointFolders(context.Background(), ts.Client(), ts.URL+"/disk.img"); err == nil {
		t.Error("a body over the cap must be an error")
	}
}

func TestListRestorePointFolders_OnlyThisHostIsUsed(t *testing.T) {
	if _, err := ListRestorePointFolders(context.Background(), http.DefaultClient, "http://example.com/disk.img"); err == nil {
		t.Error("a server that is not on 127.0.0.1 must be refused")
	}
}

func TestPickRestorePointFolder(t *testing.T) {
	mk := func(s string) RestorePointFolder {
		m, _ := ParseRestorePointFolder(s)
		return RestorePointFolder{Name: s, Moment: m}
	}
	folders := []RestorePointFolder{mk("20261104T120000Z"), mk("20261105T090000Z"), mk("20261106T180326Z")}
	tm := func(d, h, m, s int) time.Time { return time.Date(2026, 11, d, h, m, s, 0, time.UTC) }
	cases := []struct {
		at   time.Time
		want string
	}{
		{tm(6, 18, 3, 26), "20261106T180326Z"},
		{tm(6, 18, 3, 25), "20261105T090000Z"},
		{tm(5, 9, 0, 0), "20261105T090000Z"},
		{tm(4, 12, 0, 0), "20261104T120000Z"},
		{tm(20, 0, 0, 0), "20261106T180326Z"},
	}
	for _, c := range cases {
		got, err := PickRestorePointFolder(folders, c.at)
		if err != nil || got.Name != c.want {
			t.Errorf("at %v = %v, %v; want %s", c.at, got, err, c.want)
		}
	}
	if _, err := PickRestorePointFolder(folders, tm(4, 11, 59, 59)); !errors.Is(err, ErrNoRestorePoint) {
		t.Errorf("before every folder: %v, want ErrNoRestorePoint", err)
	}
	if _, err := PickRestorePointFolder(nil, tm(4, 11, 59, 59)); !errors.Is(err, ErrNoRestorePoint) {
		t.Errorf("no folders: %v, want ErrNoRestorePoint", err)
	}
}
