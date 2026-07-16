package boardspec

import "testing"

func TestMarkerRoundTrip(t *testing.T) {
	spec := &Spec{}
	is := Issue{Ref: "core-1", Title: "T", Body: "b", Labels: []string{"agent:coder"}}
	sha := ContentSHA(spec, is)
	desc := WithBody(spec, is)
	ref, gotSHA, ok := ParseMarker(desc)
	if !ok || ref != "core-1" || gotSHA != sha {
		t.Fatalf("parse = (%q,%q,%v), want (core-1,%q,true)", ref, gotSHA, ok, sha)
	}
	if StripMarker(desc) != "b" {
		t.Errorf("strip = %q, want b", StripMarker(desc))
	}
}

func TestContentSHAChangesWithContent(t *testing.T) {
	spec := &Spec{}
	a := Issue{Ref: "x", Title: "T", Body: "b"}
	b := Issue{Ref: "x", Title: "T", Body: "b2"}
	if ContentSHA(spec, a) == ContentSHA(spec, b) {
		t.Error("sha did not change with body")
	}
	// Ref is NOT part of the content hash (identity, not content).
	c := Issue{Ref: "y", Title: "T", Body: "b"}
	if ContentSHA(spec, a) != ContentSHA(spec, c) {
		t.Error("sha changed with ref; ref must not affect content sha")
	}
	// Label order must not affect the sha.
	d := Issue{Ref: "x", Title: "T", Body: "b", Labels: []string{"a", "z"}}
	e := Issue{Ref: "x", Title: "T", Body: "b", Labels: []string{"z", "a"}}
	if ContentSHA(spec, d) != ContentSHA(spec, e) {
		t.Error("sha changed with label order; must be order-independent")
	}
}

func TestContentSHAChangesWithEffectiveLabels(t *testing.T) {
	is := Issue{Ref: "x", Title: "T", Body: "b"}
	coder := &Spec{DefaultLabels: []string{"agent:coder"}}
	reviewer := &Spec{DefaultLabels: []string{"agent:reviewer"}}
	if ContentSHA(coder, is) == ContentSHA(reviewer, is) {
		t.Error("sha did not change with inherited default labels")
	}
	human := is
	human.Human = true
	if ContentSHA(coder, is) == ContentSHA(coder, human) {
		t.Error("sha did not change when effective labels changed to human")
	}
}

func TestParseMarkerMissing(t *testing.T) {
	if _, _, ok := ParseMarker("just a body, no marker"); ok {
		t.Error("ParseMarker matched a description with no marker")
	}
}
