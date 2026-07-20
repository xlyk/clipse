package linear_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/linear"
)

var stateLabelIDs = map[string]string{
	"clipse:todo":    "label-todo",
	"clipse:ready":   "label-ready",
	"clipse:running": "label-running",
	"clipse:review":  "label-review",
	"clipse:merging": "label-merging",
	"clipse:done":    "label-done",
	"clipse:rework":  "label-rework",
	"clipse:blocked": "label-blocked",
}

func newLabelLoopbackClient(t *testing.T, handler http.HandlerFunc) *linear.HTTPClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("LINEAR_API_KEY", "test-key")
	c, err := linear.NewHTTPClientWithBaseURL(
		srv.URL,
		testTeamKey,
		testTeamID,
		"agent:",
		linear.WithStateLabelPrefix("clipse:"),
	)
	if err != nil {
		t.Fatalf("NewHTTPClientWithBaseURL: %v", err)
	}
	return c
}

func stateLabelsResponse(labels map[string]string) []byte {
	type node struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	nodes := make([]node, 0, len(labels))
	for name, id := range labels {
		nodes = append(nodes, node{ID: id, Name: name})
	}
	payload := struct {
		Data struct {
			IssueLabels struct {
				Nodes []node `json:"nodes"`
			} `json:"issueLabels"`
		} `json:"data"`
	}{}
	payload.Data.IssueLabels.Nodes = nodes
	b, _ := json.Marshal(payload)
	return b
}

func TestHTTPClient_ValidateStateLabels(t *testing.T) {
	t.Run("all labels present", func(t *testing.T) {
		requests := 0
		c := newLabelLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
			requests++
			req, ok := decodeGQLRequest(t, r)
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.Query != linear.StateLabelsQuery {
				t.Errorf("query = %q, want StateLabelsQuery", req.Query)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(stateLabelsResponse(stateLabelIDs))
		})
		if err := c.ValidateStateLabels(context.Background()); err != nil {
			t.Fatalf("ValidateStateLabels: %v", err)
		}
		if err := c.ValidateStateLabels(context.Background()); err != nil {
			t.Fatalf("ValidateStateLabels cached: %v", err)
		}
		if requests != 1 {
			t.Errorf("requests = %d, want 1 (cached)", requests)
		}
	})

	t.Run("missing labels fail preflight", func(t *testing.T) {
		labels := make(map[string]string, len(stateLabelIDs)-1)
		for name, id := range stateLabelIDs {
			if name != "clipse:rework" {
				labels[name] = id
			}
		}
		c := newLabelLoopbackClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(stateLabelsResponse(labels))
		})
		err := c.ValidateStateLabels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "clipse:rework") {
			t.Fatalf("ValidateStateLabels error = %v, want missing clipse:rework", err)
		}
	})
}

func TestHTTPClient_CandidateIssues_LabelModeUsesCompletedReconciliationQuery(t *testing.T) {
	c := newLabelLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeGQLRequest(t, r)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Query != linear.LabelStateCandidateIssuesQuery {
			t.Errorf("query = %q, want LabelStateCandidateIssuesQuery", req.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"issues":{"nodes":[]}}}`))
	})

	issues, err := c.CandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("CandidateIssues: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("CandidateIssues = %+v, want empty", issues)
	}
}

func TestHTTPClient_SetState_LabelModeAtomicallyReplacesOnlyStateLabel(t *testing.T) {
	var updateVars map[string]any
	c := newLabelLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeGQLRequest(t, r)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Query {
		case linear.StateLabelsQuery:
			w.Write(stateLabelsResponse(stateLabelIDs))
		case linear.IssueLabelsQuery:
			w.Write([]byte(`{"data":{"issue":{"labels":{"nodes":[
				{"id":"lane-coder","name":"agent:coder"},
				{"id":"repo-agents","name":"agents"},
				{"id":"label-ready","name":"clipse:ready"}
			]}}}}`))
		case linear.UpdateStateLabelsMutation:
			updateVars = req.Variables
			w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		default:
			t.Errorf("unexpected query: %s", req.Query)
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	if err := c.ValidateStateLabels(context.Background()); err != nil {
		t.Fatalf("ValidateStateLabels: %v", err)
	}
	if err := c.SetState(context.Background(), "issue-1", "review"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	want := map[string]any{
		"issueId":         "issue-1",
		"addedLabelIds":   []any{"label-review"},
		"removedLabelIds": []any{"label-ready"},
	}
	if !reflect.DeepEqual(updateVars, want) {
		t.Errorf("update variables = %#v, want %#v", updateVars, want)
	}
}

func TestHTTPClient_SetState_LabelModeDoneRemovesLaneOptIn(t *testing.T) {
	var updateVars map[string]any
	c := newLabelLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		req, _ := decodeGQLRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		switch req.Query {
		case linear.StateLabelsQuery:
			w.Write(stateLabelsResponse(stateLabelIDs))
		case linear.IssueLabelsQuery:
			w.Write([]byte(`{"data":{"issue":{"labels":{"nodes":[
				{"id":"lane-coder","name":"agent:coder"},
				{"id":"repo-agents","name":"agents"},
				{"id":"label-merging","name":"clipse:merging"}
			]}}}}`))
		case linear.UpdateStateLabelsMutation:
			updateVars = req.Variables
			w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		}
	})

	if err := c.SetState(context.Background(), "issue-1", "done"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	removed, _ := updateVars["removedLabelIds"].([]any)
	if !reflect.DeepEqual(removed, []any{"lane-coder", "label-merging"}) && !reflect.DeepEqual(removed, []any{"label-merging", "lane-coder"}) {
		t.Errorf("removedLabelIds = %#v, want only agent:coder + clipse:merging", removed)
	}
	if got := updateVars["addedLabelIds"]; !reflect.DeepEqual(got, []any{"label-done"}) {
		t.Errorf("addedLabelIds = %#v, want label-done", got)
	}
}

func TestHTTPClient_SetState_LabelModeDoneAlreadyPresentStillRemovesLaneOptIn(t *testing.T) {
	var updateVars map[string]any
	c := newLabelLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		req, _ := decodeGQLRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		switch req.Query {
		case linear.StateLabelsQuery:
			w.Write(stateLabelsResponse(stateLabelIDs))
		case linear.IssueLabelsQuery:
			w.Write([]byte(`{"data":{"issue":{"labels":{"nodes":[
				{"id":"lane-coder","name":"agent:coder"},
				{"id":"repo-agents","name":"agents"},
				{"id":"label-done","name":"clipse:done"}
			]}}}}`))
		case linear.UpdateStateLabelsMutation:
			updateVars = req.Variables
			w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		}
	})

	if err := c.SetState(context.Background(), "issue-1", "done"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	want := map[string]any{
		"issueId":         "issue-1",
		"addedLabelIds":   []any{},
		"removedLabelIds": []any{"lane-coder"},
	}
	if !reflect.DeepEqual(updateVars, want) {
		t.Errorf("update variables = %#v, want existing done label preserved and only lane removed %#v", updateVars, want)
	}
}

func TestHTTPClient_SetState_LabelModeRejectsUnknownColumnBeforeHTTP(t *testing.T) {
	called := false
	t.Setenv("LINEAR_API_KEY", "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	c, err := linear.NewHTTPClientWithBaseURL(srv.URL, testTeamKey, testTeamID, "agent:", linear.WithStateLabelPrefix("clipse:"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetState(context.Background(), "issue-1", "bogus"); err == nil {
		t.Fatal("SetState: expected error")
	}
	if called {
		t.Error("SetState made an HTTP request for an unknown column")
	}
}
