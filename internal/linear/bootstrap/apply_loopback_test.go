package bootstrap

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/boardspec"
)

// TestApplyLoopbackCreatesWithMarkerAndRelation drives boardspec.Apply against a
// bootstrap.Client pointed at an httptest server, asserting the wire calls:
// a start-state resolve, an issueCreate whose description carries the marker,
// and an issueRelationCreate of type "blocks".
func TestApplyLoopbackCreatesWithMarkerAndRelation(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-key")

	var sawMarkerInCreate, sawRelation bool
	created := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := string(body)
		switch {
		case strings.Contains(q, "TeamMeta"):
			w.Write([]byte(`{"data":{"team":{"id":"TEAM1","states":{"nodes":[
				{"id":"S_TODO","name":"Todo","type":"unstarted"}]},
				"labels":{"nodes":[{"id":"LB1","name":"agent:coder"}]}}}}`))
		case strings.Contains(q, "issueCreate"):
			created++
			if strings.Contains(q, `clipse-ref:`) {
				sawMarkerInCreate = true
			}
			// each create gets a distinct id
			w.Write([]byte(`{"data":{"issueCreate":{"issue":{"id":"L` + itoa(created) + `","identifier":"CLI-` + itoa(created) + `"}}}}`))
		case strings.Contains(q, "issueRelationCreate"):
			sawRelation = true
			w.Write([]byte(`{"data":{"issueRelationCreate":{"success":true}}}`))
		default:
			t.Errorf("unexpected query: %s", q)
			w.Write([]byte(`{"data":{}}`))
		}
	}))
	defer srv.Close()

	c, err := NewClientWithBaseURL(srv.URL, "CLI")
	if err != nil {
		t.Fatalf("NewClientWithBaseURL: %v", err)
	}
	spec := &boardspec.Spec{Team: "CLI", DefaultLabels: []string{"agent:coder"}, Issues: []boardspec.Issue{
		{Ref: "a", Title: "A", Body: "aa"},
		{Ref: "b", Title: "B", Body: "bb", Deps: []string{"a"}},
	}}
	p := boardspec.BuildPlan(spec, nil)
	if err := boardspec.Apply(context.Background(), c, spec, p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if created != 2 {
		t.Errorf("created = %d, want 2", created)
	}
	if !sawMarkerInCreate {
		t.Error("issueCreate did not carry the clipse-ref marker")
	}
	if !sawRelation {
		t.Error("no issueRelationCreate issued for the b->a dependency")
	}
}

// itoa avoids importing strconv just for one small conversion in the fixture.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

var _ boardspec.Linear = (*Client)(nil)
