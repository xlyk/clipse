package boardspec

import "testing"

func TestMarkerRoundTrip(t *testing.T) {
	is := Issue{Ref: "core-1", Title: "T", Body: "b", Labels: []string{"agent:coder"}}
	sha := ContentSHA(is)
	desc := WithBody(is)
	ref, gotSHA, ok := ParseMarker(desc)
	if !ok || ref != "core-1" || gotSHA != sha {
		t.Fatalf("parse = (%q,%q,%v), want (core-1,%q,true)", ref, gotSHA, ok, sha)
	}
	if StripMarker(desc) != "b" {
		t.Errorf("strip = %q, want b", StripMarker(desc))
	}
}

func TestContentSHAChangesWithContent(t *testing.T) {
	a := Issue{Ref: "x", Title: "T", Body: "b"}
	b := Issue{Ref: "x", Title: "T", Body: "b2"}
	if ContentSHA(a) == ContentSHA(b) {
		t.Error("sha did not change with body")
	}
	// Ref is NOT part of the content hash (identity, not content).
	c := Issue{Ref: "y", Title: "T", Body: "b"}
	if ContentSHA(a) != ContentSHA(c) {
		t.Error("sha changed with ref; ref must not affect content sha")
	}
	// Label order must not affect the sha.
	d := Issue{Ref: "x", Title: "T", Body: "b", Labels: []string{"a", "z"}}
	e := Issue{Ref: "x", Title: "T", Body: "b", Labels: []string{"z", "a"}}
	if ContentSHA(d) != ContentSHA(e) {
		t.Error("sha changed with label order; must be order-independent")
	}
}

func TestParseMarkerMissing(t *testing.T) {
	if _, _, ok := ParseMarker("just a body, no marker"); ok {
		t.Error("ParseMarker matched a description with no marker")
	}
}
