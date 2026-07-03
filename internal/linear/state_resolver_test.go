package linear_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xlyk/clipse/internal/linear"
)

// teamStatesFixture is a canned TeamWorkflowStatesQuery response covering
// every canonical workflow-state name SetState resolves a board Column to
// (see canonicalWorkflowNameByColumn in status.go).
const teamStatesFixture = `{
  "data": {
    "team": {
      "states": {
        "nodes": [
          { "id": "state-todo-id", "name": "Todo", "type": "backlog" },
          { "id": "state-ready-id", "name": "Ready", "type": "unstarted" },
          { "id": "state-running-id", "name": "Running", "type": "started" },
          { "id": "state-review-id", "name": "Review", "type": "started" },
          { "id": "state-merging-id", "name": "Merging", "type": "started" },
          { "id": "state-done-id", "name": "Done", "type": "completed" },
          { "id": "state-rework-id", "name": "Rework", "type": "started" },
          { "id": "state-blocked-id", "name": "Blocked", "type": "canceled" }
        ]
      }
    }
  }
}`

// decodeGQLRequest reads and decodes r's body as a GraphQL request. On
// failure it records a t.Errorf and returns ok=false — it deliberately never
// calls t.Fatalf/FailNow, which the testing package documents as unsafe to
// call from a goroutine other than the one running the test (handler funcs
// run on the httptest.Server's own goroutine, not the test's).
func decodeGQLRequest(t *testing.T, r *http.Request) (gqlRequest, bool) {
	t.Helper()
	body, err := readAll(r)
	if err != nil {
		t.Errorf("reading request body: %v", err)
		return gqlRequest{}, false
	}
	var req gqlRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Errorf("unmarshaling request body: %v", err)
		return gqlRequest{}, false
	}
	return req, true
}

// TestHTTPClient_SetState_ResolvesColumnAndSendsExactBodies asserts that
// SetState, given a bare board Column, first fetches the team's workflow
// states, then issues the mutation with the resolved state id — and that
// both requests' bodies are byte-for-byte what BuildTeamWorkflowStatesRequest
// / BuildSetStateRequest would produce.
func TestHTTPClient_SetState_ResolvesColumnAndSendsExactBodies(t *testing.T) {
	var requests []gqlRequest
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeGQLRequest(t, r)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		requests = append(requests, req)

		w.Header().Set("Content-Type", "application/json")
		switch req.Query {
		case linear.TeamWorkflowStatesQuery:
			w.Write([]byte(teamStatesFixture))
		case linear.SetStateMutation:
			w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		default:
			t.Errorf("unexpected query: %s", req.Query)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	if err := c.SetState(context.Background(), "issue-1", "review"); err != nil {
		t.Fatalf("SetState: unexpected error: %v", err)
	}

	if len(requests) != 2 {
		t.Fatalf("len(requests) = %d, want 2 (states query then issueUpdate mutation)", len(requests))
	}

	wantStates, err := linear.BuildTeamWorkflowStatesRequest(testTeamID)
	if err != nil {
		t.Fatalf("BuildTeamWorkflowStatesRequest: unexpected error: %v", err)
	}
	assertJSONEqual(t, mustMarshal(t, requests[0]), wantStates)

	wantSetState, err := linear.BuildSetStateRequest("issue-1", "state-review-id")
	if err != nil {
		t.Fatalf("BuildSetStateRequest: unexpected error: %v", err)
	}
	assertJSONEqual(t, mustMarshal(t, requests[1]), wantSetState)
}

// TestHTTPClient_SetState_ResolvesAllCanonicalColumns exercises every board
// Column against teamStatesFixture, asserting each resolves to its expected
// Linear state id — a full sweep of canonicalWorkflowNameByColumn's inverse
// of statusFromWorkflowName.
func TestHTTPClient_SetState_ResolvesAllCanonicalColumns(t *testing.T) {
	wantStateIDByColumn := map[string]string{
		"todo":    "state-todo-id",
		"ready":   "state-ready-id",
		"running": "state-running-id",
		"review":  "state-review-id",
		"merging": "state-merging-id",
		"done":    "state-done-id",
		"rework":  "state-rework-id",
		"blocked": "state-blocked-id",
	}

	var gotStateID string
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeGQLRequest(t, r)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch req.Query {
		case linear.TeamWorkflowStatesQuery:
			w.Write([]byte(teamStatesFixture))
		case linear.SetStateMutation:
			gotStateID, _ = req.Variables["stateId"].(string)
			w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		default:
			t.Errorf("unexpected query: %s", req.Query)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	for column, wantStateID := range wantStateIDByColumn {
		if err := c.SetState(context.Background(), "issue-1", column); err != nil {
			t.Fatalf("SetState(%q): unexpected error: %v", column, err)
		}
		if gotStateID != wantStateID {
			t.Errorf("SetState(%q): resolved stateId = %q, want %q", column, gotStateID, wantStateID)
		}
	}
}

// TestHTTPClient_SetState_CachesWorkflowStatesAcrossCalls asserts the
// team's workflow-states map is fetched once and reused across multiple
// SetState calls, not refetched per call.
func TestHTTPClient_SetState_CachesWorkflowStatesAcrossCalls(t *testing.T) {
	statesRequests := 0
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeGQLRequest(t, r)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch req.Query {
		case linear.TeamWorkflowStatesQuery:
			statesRequests++
			w.Write([]byte(teamStatesFixture))
		case linear.SetStateMutation:
			w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		default:
			t.Errorf("unexpected query: %s", req.Query)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	if err := c.SetState(context.Background(), "issue-1", "review"); err != nil {
		t.Fatalf("SetState(review): unexpected error: %v", err)
	}
	if err := c.SetState(context.Background(), "issue-1", "blocked"); err != nil {
		t.Fatalf("SetState(blocked): unexpected error: %v", err)
	}

	if statesRequests != 1 {
		t.Errorf("states query requests = %d, want 1 (cached after the first resolve)", statesRequests)
	}
}

// TestHTTPClient_SetState_UnrecognizedColumn_ReturnsError asserts SetState
// rejects a column outside the board's Column enum before ever making an
// HTTP call.
func TestHTTPClient_SetState_UnrecognizedColumn_ReturnsError(t *testing.T) {
	called := false
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	})

	err := c.SetState(context.Background(), "issue-1", "not-a-column")
	if err == nil {
		t.Fatal("SetState: expected error for an unrecognized column, got nil")
	}
	if called {
		t.Error("SetState: expected no HTTP call for an unrecognized column")
	}
}

// TestHTTPClient_SetState_StateNameNotFoundOnTeam_ReturnsError asserts
// SetState errors clearly when the team's fetched states don't include the
// canonical name a column resolves to (e.g. the real board is missing a
// custom column Clipse expects).
func TestHTTPClient_SetState_StateNameNotFoundOnTeam_ReturnsError(t *testing.T) {
	c := newLoopbackClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"team":{"states":{"nodes":[{"id":"state-todo-id","name":"Todo","type":"backlog"}]}}}}`))
	})

	err := c.SetState(context.Background(), "issue-1", "review")
	if err == nil {
		t.Fatal("SetState: expected error when the team has no matching workflow state, got nil")
	}
}
