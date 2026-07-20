package linear_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/xlyk/clipse/internal/linear"
)

func TestDiscoverTeams(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "linear-secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "linear-secret" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"teams":{"nodes":[{"id":"team-2","key":"TWO","name":"Second"},{"id":"team-1","key":"ONE","name":"First"}]}}}`))
	}))
	defer srv.Close()

	got, err := linear.DiscoverTeamsWithBaseURL(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("DiscoverTeamsWithBaseURL: %v", err)
	}
	want := []linear.Team{{ID: "team-1", Key: "ONE", Name: "First"}, {ID: "team-2", Key: "TWO", Name: "Second"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("teams = %#v, want %#v", got, want)
	}
}
