package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetQueriesForFleet_ScopesAndFilters(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"queries": []map[string]interface{}{{"id": 5, "name": "os_version"}},
		})
	}))
	defer srv.Close()

	teamID := uint(3)
	queries, err := newTestClient(srv.URL).GetQueriesForFleet(t.Context(), &teamID, "macos", true)
	if err != nil {
		t.Fatalf("GetQueriesForFleet: %v", err)
	}
	if len(queries) != 1 || queries[0].ID != 5 {
		t.Fatalf("unexpected queries: %+v", queries)
	}
	for _, want := range []string{"team_id=3", "merge_inherited=true", "platform=darwin"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("expected %q in query, got %q", want, gotQuery)
		}
	}
}

func TestGetQueriesForFleet_NilFleetDropsMergeInherited(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"queries": []map[string]interface{}{}})
	}))
	defer srv.Close()

	if _, err := newTestClient(srv.URL).GetQueriesForFleet(t.Context(), nil, "", true); err != nil {
		t.Fatalf("GetQueriesForFleet: %v", err)
	}
	if strings.Contains(gotQuery, "team_id") || strings.Contains(gotQuery, "merge_inherited") {
		t.Fatalf("expected neither team_id nor merge_inherited, got %q", gotQuery)
	}
}

func TestUpdateSavedQuery_SendsOnlySetFields(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   map[string]interface{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"query": map[string]interface{}{"id": 5, "name": "renamed"},
		})
	}))
	defer srv.Close()

	name := "renamed"
	query, err := newTestClient(srv.URL).UpdateSavedQuery(t.Context(), 5, UpdateQueryRequest{Name: &name})
	if err != nil {
		t.Fatalf("UpdateSavedQuery: %v", err)
	}
	if query.Name != "renamed" {
		t.Fatalf("unexpected query: %+v", query)
	}
	if gotMethod != http.MethodPatch || gotPath != "/api/v1/fleet/reports/5" {
		t.Fatalf("unexpected request: %s %s", gotMethod, gotPath)
	}
	if len(gotBody) != 1 {
		t.Fatalf("expected only the name field in the body, got %v", gotBody)
	}
}

func TestDeleteSavedQuery_UsesIDRoute(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL).DeleteSavedQuery(t.Context(), 5); err != nil {
		t.Fatalf("DeleteSavedQuery: %v", err)
	}
	if gotPath != "/api/v1/fleet/reports/id/5" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
}

func TestGetPoliciesForFleet_PicksRouteByScope(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"policies": []map[string]interface{}{}})
	}))
	defer srv.Close()

	fc := newTestClient(srv.URL)

	if _, err := fc.GetPoliciesForFleet(t.Context(), nil); err != nil {
		t.Fatalf("GetPoliciesForFleet(nil): %v", err)
	}
	if gotPath != "/api/v1/fleet/global/policies" {
		t.Fatalf("expected the global route, got %s", gotPath)
	}

	teamID := uint(3)
	if _, err := fc.GetPoliciesForFleet(t.Context(), &teamID); err != nil {
		t.Fatalf("GetPoliciesForFleet(3): %v", err)
	}
	if gotPath != "/api/v1/fleet/fleets/3/policies" {
		t.Fatalf("expected the per-fleet route, got %s", gotPath)
	}
}

func TestResolveFleetID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"teams": []map[string]interface{}{{"id": 3, "name": "Workstations"}},
		})
	}))
	defer srv.Close()

	fc := newTestClient(srv.URL)

	id, err := fc.resolveFleetID(t.Context(), "")
	if err != nil || id != nil {
		t.Fatalf("empty name should resolve to nil, got %v %v", id, err)
	}

	id, err = fc.resolveFleetID(t.Context(), "workstations")
	if err != nil {
		t.Fatalf("resolveFleetID: %v", err)
	}
	if id == nil || *id != 3 {
		t.Fatalf("expected fleet 3, got %v", id)
	}

	if _, err := fc.resolveFleetID(t.Context(), "nope"); err == nil {
		t.Fatal("expected an error for an unknown fleet name")
	}
}
