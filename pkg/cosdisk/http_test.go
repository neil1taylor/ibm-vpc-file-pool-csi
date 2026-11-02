package cosdisk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// rangeServer serves data like Kopia's WebDAV server: HEAD gives the size and a
// GET with a Range header gives 206 and that range.
func rangeServer(t *testing.T, data []byte, honourRange bool, gets *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			return
		}
		gets.Add(1)
		rng := r.Header.Get("Range")
		if !honourRange || rng == "" {
			_, _ = w.Write(data)
			return
		}
		var a, b int
		if _, err := fmtSscanRange(rng, &a, &b); err != nil || a > b || b >= len(data) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+strconv.Itoa(a)+"-"+strconv.Itoa(b)+"/"+strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
	}))
}

func fmtSscanRange(s string, a, b *int) (int, error) {
	s = strings.TrimPrefix(s, "bytes=")
	i := strings.IndexByte(s, '-')
	var err error
	if *a, err = strconv.Atoi(s[:i]); err != nil {
		return 0, err
	}
	if *b, err = strconv.Atoi(s[i+1:]); err != nil {
		return 0, err
	}
	return 2, nil
}

func TestHTTPSourceReadsARange(t *testing.T) {
	data := newData(5000)
	var gets atomic.Int64
	srv := rangeServer(t, data, true, &gets)
	defer srv.Close()
	src, err := NewHTTPSource(context.Background(), srv.Client(), srv.URL+"/disk.img")
	if err != nil {
		t.Fatal(err)
	}
	if src.Size() != 5000 {
		t.Fatalf("Size = %d", src.Size())
	}
	got := make([]byte, 700)
	n, err := src.ReadRange(context.Background(), got, 1234)
	if err != nil || n != 700 {
		t.Fatalf("ReadRange = %d, %v", n, err)
	}
	if string(got) != string(data[1234:1934]) {
		t.Fatal("wrong bytes")
	}
}

func TestHTTPSourceRefusesAServerThatIgnoresRange(t *testing.T) {
	var gets atomic.Int64
	srv := rangeServer(t, newData(5000), false, &gets)
	defer srv.Close()
	src, err := NewHTTPSource(context.Background(), srv.Client(), srv.URL+"/disk.img")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = src.ReadRange(context.Background(), make([]byte, 10), 100); err == nil {
		t.Fatal("a 200 reply to a range request would send the whole file for every read; want an error")
	}
}

func TestHTTPSourceRefusesAHostThatIsNotThisPod(t *testing.T) {
	for _, u := range []string{"http://example.com/disk.img", "http://10.0.0.5:8080/disk.img", "https://127.0.0.1/disk.img", "http://127.0.0.1.evil.com/x", "file:///etc/passwd"} {
		if _, err := NewHTTPSource(context.Background(), http.DefaultClient, u); err == nil {
			t.Errorf("%s: want a refusal; the Kopia server is only ever on 127.0.0.1", u)
		}
	}
}

func TestHTTPSourceAcceptsLoopback(t *testing.T) {
	var gets atomic.Int64
	srv := rangeServer(t, newData(100), true, &gets)
	defer srv.Close()
	if !strings.HasPrefix(srv.URL, "http://127.0.0.1:") {
		t.Skip("test server is not on 127.0.0.1")
	}
	if _, err := NewHTTPSource(context.Background(), srv.Client(), srv.URL+"/disk.img"); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSourceHeadFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := NewHTTPSource(context.Background(), srv.Client(), srv.URL+"/disk.img"); err == nil {
		t.Fatal("want an error for a missing file")
	}
}
