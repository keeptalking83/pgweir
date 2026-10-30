// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNeon implements the subset of the Neon API v2 the provider uses.
type fakeNeon struct {
	mu         sync.Mutex
	lockedOnce bool
	created    map[string]any
	deleted    []string
	opPolls    int
	uriQuery   string
}

func (f *fakeNeon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer key" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/projects/prj/branches":
		_, _ = io.WriteString(w, `{"branches":[{"id":"br-main","name":"main","default":true},{"id":"br-dev","name":"dev"}]}`)
	case r.Method == http.MethodGet && p == "/projects/prj/endpoints/ep-main-123":
		_, _ = io.WriteString(w, `{"endpoint":{"id":"ep-main-123","branch_id":"br-main"}}`)
	case r.Method == http.MethodPost && p == "/projects/prj/branches":
		if !f.lockedOnce {
			f.lockedOnce = true
			w.WriteHeader(http.StatusLocked)
			_, _ = io.WriteString(w, `{"message":"project already has running operations"}`)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&f.created)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"branch":{"id":"br-new","name":"pw_demo"},
"endpoints":[{"id":"ep-new-1","branch_id":"br-new","host":"ep-new-1.example","current_state":"init","type":"read_write"}],
"operations":[{"id":"op-1","action":"create_branch","status":"running"}]}`)
	case r.Method == http.MethodGet && p == "/projects/prj/operations/op-1":
		f.opPolls++
		status := "running"
		if f.opPolls >= 2 {
			status = "finished"
		}
		_, _ = io.WriteString(w, `{"operation":{"id":"op-1","action":"create_branch","status":"`+status+`"}}`)
	case r.Method == http.MethodGet && p == "/projects/prj/connection_uri":
		f.uriQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"uri":"postgresql://app:pw@ep-new-1.example/shop?sslmode=require"}`)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/projects/prj/branches/"):
		f.deleted = append(f.deleted, strings.TrimPrefix(p, "/projects/prj/branches/"))
		_, _ = io.WriteString(w, `{"branch":{"id":"br-new"},"operations":[]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"not found: `+r.Method+" "+p+`"}`)
	}
}

func newTestNeon(t *testing.T, f *fakeNeon, parent string) *Neon {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("NEON_API_KEY", "key")
	t.Setenv("NEON_PROJECT_ID", "prj")
	t.Setenv("PGWEIR_NEON_API_URL", srv.URL)
	p, err := NewNeon(Options{NeonParent: parent, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	n := p.(*Neon)
	n.poll = time.Millisecond
	n.ready = func(context.Context, string) error { return nil }
	return n
}

func TestNeonFork(t *testing.T) {
	f := &fakeNeon{}
	n := newTestNeon(t, f, "")
	dsn, ref, err := n.Fork(context.Background(), "postgresql://app:pw@ep-main-123.us-east-2.aws.neon.tech/shop", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dsn, "postgresql://app:pw@ep-new-1.example/shop") {
		t.Fatalf("dsn %s", dsn)
	}
	if ref["branch_id"] != "br-new" || ref["endpoint_id"] != "ep-new-1" || ref["project_id"] != "prj" {
		t.Fatalf("ref %v", ref)
	}
	br := f.created["branch"].(map[string]any)
	if br["parent_id"] != "br-main" || br["name"] != "pw_demo" {
		t.Fatalf("create payload %v", f.created)
	}
	eps := f.created["endpoints"].([]any)
	if eps[0].(map[string]any)["type"] != "read_write" {
		t.Fatalf("endpoint payload %v", eps)
	}
	for _, want := range []string{"database_name=shop", "role_name=app", "pooled=false", "branch_id=br-new", "endpoint_id=ep-new-1"} {
		if !strings.Contains(f.uriQuery, want) {
			t.Errorf("connection_uri query %q lacks %s", f.uriQuery, want)
		}
	}
	if f.opPolls < 2 {
		t.Error("operation was not polled until finished")
	}
	if err := n.Drop(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "br-new" {
		t.Fatalf("deleted %v", f.deleted)
	}
}

func TestNeonParentMismatch(t *testing.T) {
	f := &fakeNeon{}
	n := newTestNeon(t, f, "dev")
	_, _, err := n.Fork(context.Background(), "postgresql://app:pw@ep-main-123-pooler.us-east-2.aws.neon.tech/shop", "demo")
	if err == nil || !strings.Contains(err.Error(), "pass --parent br-main") {
		t.Fatalf("expected parent mismatch, got %v", err)
	}
	if f.created != nil {
		t.Fatal("branch created despite mismatch")
	}
}

func TestNeonNeedsCredentials(t *testing.T) {
	t.Setenv("NEON_API_KEY", "")
	t.Setenv("NEON_PROJECT_ID", "")
	if _, err := NewNeon(Options{}); err == nil {
		t.Fatal("expected an error without credentials")
	}
}

func TestEndpointID(t *testing.T) {
	for host, want := range map[string]string{
		"ep-cool-darkness-123456.us-east-2.aws.neon.tech":        "ep-cool-darkness-123456",
		"ep-cool-darkness-123456-pooler.us-east-2.aws.neon.tech": "ep-cool-darkness-123456",
		"localhost":      "",
		"db.example.com": "",
	} {
		if got := endpointID(host); got != want {
			t.Errorf("endpointID(%q) = %q, want %q", host, got, want)
		}
	}
}
