package crapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fetcher(url string, max int64) *Fetcher {
	f := New("eyJ.secret.key", "test")
	f.base = url
	f.maxBody = max
	return f
}

func TestBodyAtTheBoundIsKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), 64))
	}))
	defer srv.Close()
	r := fetcher(srv.URL, 64).Fetch(context.Background(), "/x")
	if r.Kind != "http" || r.Status != 200 || r.TooLarge || len(r.BodyText) != 64 {
		t.Fatalf("%+v", r)
	}
}

// A declared Content-Length over the bound: refused without reading.
func TestDeclaredLengthOverTheBoundIsTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		_, _ = w.Write(bytes.Repeat([]byte("a"), 65))
	}))
	defer srv.Close()
	r := fetcher(srv.URL, 64).Fetch(context.Background(), "/x")
	if r.Kind != "http" || r.Status != 200 || !r.TooLarge || r.BodyText != "" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.Message, "over 64 bytes") {
		t.Fatalf("message should say why: %q", r.Message)
	}
	if r.RetryAfterSeconds == nil || *r.RetryAfterSeconds != 7 {
		t.Fatalf("headers still read: %+v", r.RetryAfterSeconds)
	}
}

// Chunked: no length to judge up front, so the read itself stops at max+1.
func TestChunkedBodyOverTheBoundIsTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 100; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("a"), 16))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	r := fetcher(srv.URL, 64).Fetch(context.Background(), "/x")
	if !r.TooLarge || r.BodyText != "" {
		t.Fatalf("%+v", r)
	}
}

// A small gzip body that inflates past the bound is judged on what it
// inflates to: the bound is on memory, not on the wire.
func TestCompressedBodyIsBoundedAfterDecoding(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(bytes.Repeat([]byte("a"), 1<<20))
	_ = w.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("expected Go's transparent gzip")
		}
		rw.Header().Set("Content-Encoding", "gzip")
		_, _ = rw.Write(gz.Bytes())
	}))
	defer srv.Close()
	if gz.Len() > 4096 {
		t.Fatalf("fixture should be small on the wire, got %d", gz.Len())
	}
	r := fetcher(srv.URL, 4096).Fetch(context.Background(), "/x")
	if !r.TooLarge {
		t.Fatalf("%+v", r)
	}
}

// An https -> http redirect would put the next request on the wire in
// the clear; it is refused, and the http side never hears from us.
func TestRedirectDowngradeIsRefused(t *testing.T) {
	heard := false
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		heard = true
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
	}))
	defer tls.Close()
	f := fetcher(tls.URL, 64)
	f.client = tls.Client()
	f.client.CheckRedirect = RefuseDowngrade
	r := f.Fetch(context.Background(), "/x")
	if r.Kind != "transport" || !strings.Contains(r.Message, "refusing redirect from https to http") {
		t.Fatalf("%+v", r)
	}
	if heard {
		t.Fatal("the plain-http server was contacted")
	}
}

func TestSameSchemeRedirectIsFollowed(t *testing.T) {
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer tls.Close()
	f := fetcher(tls.URL, 64)
	f.client = tls.Client()
	f.client.CheckRedirect = RefuseDowngrade
	if r := f.Fetch(context.Background(), "/old"); r.Status != 200 || r.BodyText != "ok" {
		t.Fatalf("%+v", r)
	}
}
