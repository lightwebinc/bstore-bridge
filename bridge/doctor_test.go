package bridge

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/testchain"

	"github.com/lightwebinc/bstore/provider"
)

// The bridge's doctor reads and reports the front, then the incumbent.
func TestDoctorReportsTheIncumbent(t *testing.T) {
	cs := httptest.NewServer(testchain.New(700))
	defer cs.Close()
	data := filepath.Join(t.TempDir(), "b")
	if _, err := Init(data); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Provider: provider.Config{Data: data, PublicURL: "https://127.0.0.1:1/front", Node: cs.URL, Headers: cs.URL, Backend: "bridge"},
		Incumbent: Incumbent{URL: cs.URL, Identity: "02" + strings.Repeat("ab", 32), Lookup: []string{"http://127.0.0.1:1"}}}
	var out bytes.Buffer
	if err := Doctor(context.Background(), cfg, "", &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"backend      bridge", "incumbent    " + cs.URL + ": answers", "ls_uhrp      http://127.0.0.1:1: does not answer",
		"forwarded    0 settlement(s)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}
