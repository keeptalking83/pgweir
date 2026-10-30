// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is a minimal path-style S3 server: ListObjectsV2, Head/Get/PutObject
// with If-None-Match: *.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	bucket := parts[0]
	if bucket != "audit" {
		http.Error(w, "<Error><Code>NoSuchBucket</Code></Error>", http.StatusNotFound)
		return
	}
	if len(parts) == 1 || parts[1] == "" {
		if r.URL.Query().Get("list-type") != "2" {
			http.Error(w, "unsupported", http.StatusBadRequest)
			return
		}
		prefix := r.URL.Query().Get("prefix")
		type content struct {
			Key  string `xml:"Key"`
			Size int    `xml:"Size"`
		}
		res := struct {
			XMLName     xml.Name  `xml:"ListBucketResult"`
			Name        string    `xml:"Name"`
			Prefix      string    `xml:"Prefix"`
			KeyCount    int       `xml:"KeyCount"`
			IsTruncated bool      `xml:"IsTruncated"`
			Contents    []content `xml:"Contents"`
		}{Name: bucket, Prefix: prefix}
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			res.Contents = append(res.Contents, content{Key: k, Size: len(f.objects[k])})
		}
		res.KeyCount = len(keys)
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(res)
		return
	}
	key := parts[1]
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			}
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
	case http.MethodPut:
		if _, ok := f.objects[key]; ok && r.Header.Get("If-None-Match") == "*" {
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.objects[key] = b
		f.puts++
		w.Header().Set("ETag", `"etag"`)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestS3Sink(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("AWS_ENDPOINT_URL_S3", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_required")
	t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "when_required")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")

	ctx := context.Background()
	sink, err := NewS3Sink(ctx, "s3://audit/ledger/prod")
	if err != nil {
		t.Fatal(err)
	}
	wrote, err := sink.Put(ctx, "sth/1.json", []byte(`{"a":1}`))
	if err != nil || !wrote {
		t.Fatalf("first put: %v %v", wrote, err)
	}
	if _, ok := fake.objects["ledger/prod/sth/1.json"]; !ok {
		t.Fatalf("object not stored under the prefix: %v", fake.objects)
	}
	wrote, err = sink.Put(ctx, "sth/1.json", []byte(`{"a":1}`))
	if err != nil || wrote {
		t.Fatalf("identical put: %v %v", wrote, err)
	}
	if _, err := sink.Put(ctx, "sth/1.json", []byte(`{"a":2}`)); !errors.Is(err, ErrExists) {
		t.Fatalf("overwrite with different content: %v", err)
	}
	if fake.puts != 1 {
		t.Fatalf("%d PUTs, want 1 (never overwrite)", fake.puts)
	}
	if _, err := sink.Put(ctx, "leaves/00000000-00000009.json", []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	names, err := sink.List(ctx, "leaves/")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "leaves/00000000-00000009.json" {
		t.Fatalf("list %v", names)
	}
	next, err := nextUnexported(ctx, sink)
	if err != nil || next != 10 {
		t.Fatalf("next unexported %d %v", next, err)
	}
	if _, err := NewS3Sink(ctx, "https://example.com/x"); err == nil {
		t.Fatal("non-s3 URL accepted")
	}
}

func TestDirSink(t *testing.T) {
	ctx := context.Background()
	sink := DirSink{Root: t.TempDir()}
	if wrote, err := sink.Put(ctx, "sth/1.json", []byte("x")); err != nil || !wrote {
		t.Fatal(err)
	}
	if wrote, err := sink.Put(ctx, "sth/1.json", []byte("x")); err != nil || wrote {
		t.Fatal("identical put rewrote")
	}
	if _, err := sink.Put(ctx, "sth/1.json", []byte("y")); !errors.Is(err, ErrExists) {
		t.Fatalf("overwrite: %v", err)
	}
	names, _ := sink.List(ctx, "sth/")
	if len(names) != 1 || names[0] != "sth/1.json" {
		t.Fatalf("list %v", names)
	}
	if names, err := sink.List(ctx, "leaves/"); err != nil || names != nil {
		t.Fatalf("missing dir: %v %v", names, err)
	}
}
