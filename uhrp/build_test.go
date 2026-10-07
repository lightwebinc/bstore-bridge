package uhrp

import "testing"

func TestReadBuild(t *testing.T) {
	for _, c := range []struct {
		doc      string
		affected bool
	}{
		// ts-stack b3155fa2 onward: the commit route and the bsvAuth scheme.
		{`{"components":{"securitySchemes":{"bsvAuth":{"type":"http","scheme":"bsv-auth"}}},"paths":{"/chirp/v1/uploads":{},"/chirp/v1/uploads/{uploadId}/commit":{}}}`, true},
		// Before it: the commit route, no security scheme.
		{`{"paths":{"/chirp/v1/uploads":{},"/chirp/v1/uploads/{uploadId}/commit":{}}}`, false},
		// No CHIRP commit route at all.
		{`{"components":{"securitySchemes":{"bsvAuth":{}}},"paths":{}}`, false},
	} {
		b, err := ReadBuild([]byte(c.doc))
		if err != nil || !b.Served || b.Affected != c.affected || len(b.Fingerprint) != 64 {
			t.Fatalf("%s: %+v %v", c.doc, b, err)
		}
	}
	if _, err := ReadBuild([]byte("nope")); err == nil {
		t.Fatal("a description that does not read was read")
	}
}
