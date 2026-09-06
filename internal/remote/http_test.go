package remote

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every hub endpoint must turn a 403 into ErrForbidden: that sentinel is what
// tells the syncer "you were refused" rather than "the network is down", and a
// miss on any one call would put that path back into a silent forever-retry.
func TestHubForbiddenIsSentinel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "you have read-only access to this project", http.StatusForbidden)
	}))
	defer ts.Close()

	be, err := Open(context.Background(), ts.URL+"/p/p-0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	ctx := context.Background()

	calls := map[string]func() error{
		"List":   func() error { _, err := be.List(ctx, "journal/"); return err },
		"Get":    func() error { _, err := be.Get(ctx, "journal/d.jsonl"); return err },
		"Exists": func() error { _, err := be.Exists(ctx, "journal/d.jsonl"); return err },
		"Put":    func() error { return be.Put(ctx, "blobs/abc", strings.NewReader("hi"), 2) },
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s: no error on 403", name)
			continue
		}
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v does not wrap ErrForbidden", name, err)
		}
		if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: the server's message is lost: %v", name, err)
		}
	}
	if rr, ok := be.(ReadReporter); ok {
		if err := rr.ReportReads(ctx, ReadKindAgent, []ReadEvent{{Path: "a.md"}}); !errors.Is(err, ErrForbidden) {
			t.Errorf("ReportReads: %v does not wrap ErrForbidden", err)
		}
	}
}

// A 403 relayed from the object store is an expired presigned URL, not an
// authorization answer. Mapping it would park a healthy device in permanent
// read-only over a transient signing problem.
func TestPresignedForbiddenIsNotAuthz(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
	}))
	defer storage.Close()

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/store/sign") {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"mode":"direct","exists":false,"url":"` + storage.URL + `/blob","method":"PUT"}`))
	}))
	defer hub.Close()

	be, err := Open(context.Background(), hub.URL+"/p/p-0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	err = be.Put(context.Background(), "blobs/abc", bytes.NewReader([]byte("hi")), 2)
	if err == nil {
		t.Fatal("direct upload to a 403 target should fail")
	}
	if errors.Is(err, ErrForbidden) {
		t.Fatalf("a presigned-target 403 must not be ErrForbidden: %v", err)
	}
}
