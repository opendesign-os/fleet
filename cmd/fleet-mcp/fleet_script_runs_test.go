package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunScript_SyncSendsScriptIDAndOmitsFleet(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]interface{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"host_id": 12, "execution_id": "exec-1", "exit_code": 0, "output": "ok",
		})
	}))
	defer srv.Close()

	scriptID := uint(7)
	run, err := newTestClient(srv.URL).RunScript(t.Context(), 12, &scriptID, "", "", nil, true)
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if run.ExecutionID != "exec-1" || run.ExitCode == nil || *run.ExitCode != 0 {
		t.Fatalf("unexpected run: %+v", run)
	}
	if gotPath != "/api/v1/fleet/scripts/run/sync" {
		t.Fatalf("expected the sync route, got %s", gotPath)
	}
	if _, ok := gotBody["team_id"]; ok {
		t.Fatalf("team_id should be omitted when nil, got %v", gotBody)
	}
	if gotBody["script_id"] != float64(7) {
		t.Fatalf("expected script_id 7, got %v", gotBody["script_id"])
	}
}

func TestRunScript_AsyncUsesQueueRoute(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"host_id": 12, "execution_id": "exec-2"})
	}))
	defer srv.Close()

	run, err := newTestClient(srv.URL).RunScript(t.Context(), 12, nil, "echo hi", "check.sh", nil, false)
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if run.ExecutionID != "exec-2" || run.ExitCode != nil {
		t.Fatalf("unexpected run: %+v", run)
	}
	if gotPath != "/api/v1/fleet/scripts/run" {
		t.Fatalf("expected the async route, got %s", gotPath)
	}
}

func TestRunScript_AnonymousSendsFleetAndName(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"host_id": 12, "execution_id": "exec-3"})
	}))
	defer srv.Close()

	teamID := uint(3)
	if _, err := newTestClient(srv.URL).RunScript(t.Context(), 12, nil, "echo hi", "check.ps1", &teamID, true); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if gotBody["team_id"] != float64(3) {
		t.Fatalf("expected team_id 3, got %v", gotBody["team_id"])
	}
	if gotBody["script_name"] != "check.ps1" || gotBody["script_contents"] != "echo hi" {
		t.Fatalf("unexpected body: %v", gotBody)
	}
	if _, ok := gotBody["script_id"]; ok {
		t.Fatalf("script_id should be omitted for an anonymous run, got %v", gotBody)
	}
}

// Fleet answers a sync run that outlives its timeout with 408 plus a body — the
// execution is still queued, so this must surface as a result to poll rather
// than as a transport failure.
func TestRunScript_TimeoutIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"host_id": 12, "execution_id": "exec-4", "host_timeout": true,
		})
	}))
	defer srv.Close()

	scriptID := uint(7)
	run, err := newTestClient(srv.URL).RunScript(t.Context(), 12, &scriptID, "", "", nil, true)
	if err != nil {
		t.Fatalf("a host timeout should not be an error: %v", err)
	}
	if !run.HostTimeout || run.ExecutionID != "exec-4" {
		t.Fatalf("unexpected run: %+v", run)
	}
}

func TestRunScript_ServerErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"script and host are in different fleets"}`))
	}))
	defer srv.Close()

	scriptID := uint(7)
	if _, err := newTestClient(srv.URL).RunScript(t.Context(), 12, &scriptID, "", "", nil, true); err == nil {
		t.Fatal("expected an error for a 422 response")
	}
}

func TestGetScriptResult_PendingHasNilExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/fleet/scripts/results/exec-1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"host_id": 12, "execution_id": "exec-1", "exit_code": nil, "hostname": "alpha.local",
		})
	}))
	defer srv.Close()

	run, err := newTestClient(srv.URL).GetScriptResult(t.Context(), "exec-1")
	if err != nil {
		t.Fatalf("GetScriptResult: %v", err)
	}
	if run.ExitCode != nil || run.HostName != "alpha.local" {
		t.Fatalf("unexpected run: %+v", run)
	}
}

func TestRunScriptBatch_SendsResolvedHostIDs(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"batch_execution_id": "batch-1"})
	}))
	defer srv.Close()

	batchID, err := newTestClient(srv.URL).RunScriptBatch(t.Context(), 7, []uint{1, 2, 3})
	if err != nil {
		t.Fatalf("RunScriptBatch: %v", err)
	}
	if batchID != "batch-1" {
		t.Fatalf("unexpected batch id: %s", batchID)
	}
	ids, ok := gotBody["host_ids"].([]interface{})
	if !ok || len(ids) != 3 {
		t.Fatalf("expected 3 host_ids, got %v", gotBody["host_ids"])
	}
}

func TestGetScriptBatchStatus_DecodesCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/fleet/scripts/batch/batch-1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"batch_execution_id":  "batch-1",
			"status":              "started",
			"script_name":         "restart.sh",
			"targeted_host_count": 3,
			"ran_host_count":      1,
			"pending_host_count":  2,
		})
	}))
	defer srv.Close()

	status, err := newTestClient(srv.URL).GetScriptBatchStatus(t.Context(), "batch-1")
	if err != nil {
		t.Fatalf("GetScriptBatchStatus: %v", err)
	}
	if status.Status != "started" || status.TargetedHostCount == nil || *status.TargetedHostCount != 3 {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.RanHostCount == nil || *status.RanHostCount != 1 {
		t.Fatalf("unexpected ran count: %+v", status.RanHostCount)
	}
}

func TestGetEndpointsWithAggregations_ScopesToFleet(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"totals_hosts_count": 4,
			"platforms": []map[string]interface{}{
				{"platform": "darwin", "hosts_count": 3},
				{"platform": "ubuntu", "hosts_count": 1},
			},
		})
	}))
	defer srv.Close()

	fc := newTestClient(srv.URL)
	teamID := uint(3)
	agg, err := fc.GetEndpointsWithAggregations(t.Context(), &teamID)
	if err != nil {
		t.Fatalf("GetEndpointsWithAggregations: %v", err)
	}
	if agg.Count != 4 {
		t.Fatalf("expected 4 hosts, got %d", agg.Count)
	}
	if gotQuery != "team_id=3" {
		t.Fatalf("expected team_id=3, got %q", gotQuery)
	}

	if _, err := fc.GetEndpointsWithAggregations(t.Context(), nil); err != nil {
		t.Fatalf("GetEndpointsWithAggregations(nil): %v", err)
	}
	if gotQuery != "" {
		t.Fatalf("expected no query string for the unscoped call, got %q", gotQuery)
	}
}
