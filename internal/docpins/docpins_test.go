// Package docpins holds the versions the documentation names to what
// go.mod pins. A version written into a page goes stale silently the first
// time a pin moves; this test fails instead.
package docpins

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const root = "../.."

func TestDocumentedVersionsMatchThePins(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	req := map[string]string{}
	goVer := ""
	for _, l := range strings.Split(string(raw), "\n") {
		f := strings.Fields(strings.TrimSpace(l))
		switch {
		case len(f) >= 2 && f[0] == "go":
			goVer = f[1]
		case len(f) >= 2 && strings.Contains(f[0], "/") && strings.HasPrefix(f[1], "v"):
			req[f[0]] = f[1]
		}
	}
	pins := []struct {
		name string
		re   *regexp.Regexp
		want string
	}{
		{"go-sdk", regexp.MustCompile(`go-sdk\x60? (v\d+\.\d+\.\d+)`), req["github.com/bsv-blockchain/go-sdk"]},
		{"bcommon", regexp.MustCompile(`bcommon\x60? (v\d+\.\d+\.\d+)`), req["github.com/lightwebinc/bcommon"]},
		{"bstore", regexp.MustCompile(`\bbstore\x60? (v\d+\.\d+\.\d+)`), req["github.com/lightwebinc/bstore"]},
		{"Go", regexp.MustCompile(`\bGo (1\.\d+)\b`), regexp.MustCompile(`^\d+\.\d+`).FindString(goVer)},
	}
	ps := []string{filepath.Join(root, "README.md")}
	more, _ := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	ps = append(ps, more...)
	seen := map[string]int{}
	for _, p := range ps {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, pn := range pins {
			if pn.want == "" {
				t.Fatalf("%s: no pin in go.mod", pn.name)
			}
			for _, m := range pn.re.FindAllStringSubmatchIndex(text, -1) {
				seen[pn.name]++
				if got := text[m[2]:m[3]]; got != pn.want {
					t.Errorf("%s:%d names %s %s; go.mod pins %s", filepath.Base(p), strings.Count(text[:m[0]], "\n")+1, pn.name, got, pn.want)
				}
			}
		}
	}
	for _, n := range []string{"go-sdk", "bcommon", "bstore", "Go"} {
		if seen[n] == 0 {
			t.Errorf("no page names the %s version; the README should", n)
		}
	}
}
