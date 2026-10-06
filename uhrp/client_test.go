package uhrp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/chirp"
)

// server serves one object of one root, answering Range as answer says.
func server(t *testing.T, root, obj [32]byte, data []byte, answer func(w http.ResponseWriter, r *http.Request)) *Client {
	t.Helper()
	path := "/inc/chirp/v1/" + chirp.Identifier(root) + "/objects/" + chirp.Identifier(obj)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL + "/inc", HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func TestFetchHoldsTheIncumbentToItsAnswer(t *testing.T) {
	ctx := context.Background()
	data := bytes.Repeat([]byte("0123456789abcdef"), 1024)
	obj := sha256.Sum256(data)
	root := sha256.Sum256([]byte("root"))
	size := uint64(len(data))

	honest := server(t, root, obj, data, func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	})
	r, err := honest.Fetch(ctx, root, obj, 100, 50, size, true)
	if err != nil || !r.Honoured || !bytes.Equal(r.Data, data[100:150]) {
		t.Fatalf("honest range: %v %+v", err, r)
	}
	if b, err := honest.Get(ctx, root, obj, size); err != nil || !bytes.Equal(b, data) {
		t.Fatalf("whole: %v", err)
	}
	if n, err := honest.Head(ctx, root, obj); err != nil || n != size {
		t.Fatalf("head: %v %d", err, n)
	}

	// Range ignored: the whole object, cut here, not honoured.
	whole := server(t, root, obj, data, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) })
	r, err = whole.Fetch(ctx, root, obj, 100, 50, size, true)
	if err != nil || r.Honoured || !bytes.Equal(r.Data, data[100:150]) {
		t.Fatalf("range ignored: %v %+v", err, r)
	}

	// A 206 for another range, or the wrong number of bytes: not honoured.
	other := server(t, root, obj, data, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-49/%d", size))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[:50])
	})
	if r, err = other.Fetch(ctx, root, obj, 100, 50, size, true); err != nil || r.Honoured {
		t.Fatalf("another range: %v %+v", err, r)
	}

	// A whole answer of another length is not the object.
	short := server(t, root, obj, data, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data[:10]) })
	if _, err := short.Fetch(ctx, root, obj, 0, 5, size, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("short whole: %v", err)
	}
	// Bytes that are not the object's hash are not the object.
	wrong := server(t, root, obj, data, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(bytes.ToUpper(data)) })
	if _, err := wrong.Get(ctx, root, obj, size); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other bytes: %v", err)
	}
	// 404 and 410 are gone; a redirect is never followed.
	gone := server(t, root, obj, data, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGone) })
	if _, err := gone.Fetch(ctx, root, obj, 0, 5, size, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("gone: %v", err)
	}
	hits := 0
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; _, _ = w.Write(data) }))
	defer elsewhere.Close()
	redirect := server(t, root, obj, data, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/x", http.StatusFound)
	})
	if _, err := redirect.Fetch(ctx, root, obj, 0, 5, size, true); err == nil || hits != 0 {
		t.Fatalf("a redirect was followed: %v, %d hit(s)", err, hits)
	}
	// An answer past the object's bound is cut.
	big := server(t, root, obj, data, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(MaxObject+2<<20))
		w.WriteHeader(http.StatusOK)
	})
	if _, err := big.Fetch(ctx, root, obj, 0, 5, size, true); err == nil {
		t.Fatal("an over-long answer was read")
	}
	// A range outside the object is not asked for.
	if _, err := honest.Fetch(ctx, root, obj, size-1, 2, size, true); err == nil {
		t.Fatal("a range outside the object")
	}
}

func TestObjectURLAndUHRPURL(t *testing.T) {
	c := &Client{Base: "https://incumbent.example/store"}
	var r, o [32]byte
	r[0], o[0] = 1, 2
	u := c.ObjectURL(r, o)
	if !strings.HasPrefix(u, "https://incumbent.example/store/chirp/v1/") || !strings.HasSuffix(u, "/objects/"+chirp.Identifier(o)) {
		t.Fatal(u)
	}
	if URL(r) != "uhrp://"+chirp.Identifier(r) {
		t.Fatal(URL(r))
	}
}
