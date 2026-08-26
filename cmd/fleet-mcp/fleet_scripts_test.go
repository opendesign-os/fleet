package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetScripts_ScopesToFleet(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"scripts": []map[string]interface{}{{"id": 7, "name": "restart.sh", "team_id": 3}},
		})
	}))
	defer srv.Close()

	fc := newTestClient(srv.URL)
	teamID := uint(3)
	scripts, err := fc.GetScripts(t.Context(), &teamID, 50)
	if err != nil {
		t.Fatalf("GetScripts: %v", err)
	}
	if len(scripts) != 1 || scripts[0].ID != 7 || scripts[0].Name != "restart.sh" {
		t.Fatalf("unexpected scripts: %+v", scripts)
	}
	if !strings.Contains(gotQuery, "team_id=3") {
		t.Fatalf("expected team_id=3 in query, got %q", gotQuery)
	}
}

func TestGetScripts_OmitsFleetWhenNil(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"scripts": []map[string]interface{}{}})
	}))
	defer srv.Close()

	if _, err := newTestClient(srv.URL).GetScripts(t.Context(), nil, 50); err != nil {
		t.Fatalf("GetScripts: %v", err)
	}
	if strings.Contains(gotQuery, "team_id") {
		t.Fatalf("expected no team_id in query, got %q", gotQuery)
	}
}

func TestGetScript_WithContentsIssuesSecondCall(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.URL.Query().Get("alt") == "media" {
			_, _ = w.Write([]byte("#!/bin/sh\necho hi\n"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 7, "name": "restart.sh"})
	}))
	defer srv.Close()

	script, err := newTestClient(srv.URL).GetScript(t.Context(), 7, true)
	if err != nil {
		t.Fatalf("GetScript: %v", err)
	}
	if script.Contents != "#!/bin/sh\necho hi\n" {
		t.Fatalf("unexpected contents: %q", script.Contents)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 calls, got %v", paths)
	}
}

func TestCreateScript_SendsMultipartWithFleet(t *testing.T) {
	var (
		gotContentType string
		gotFleetID     string
		gotFileName    string
		gotBody        string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			return
		}
		if vals := r.MultipartForm.Value["fleet_id"]; len(vals) > 0 {
			gotFleetID = vals[0]
		}
		fhs := r.MultipartForm.File["script"]
		if len(fhs) == 0 {
			t.Error("no script file part")
			return
		}
		gotFileName = fhs[0].Filename
		f, err := fhs[0].Open()
		if err != nil {
			t.Errorf("open part: %v", err)
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		gotBody = string(b)

		_ = json.NewEncoder(w).Encode(map[string]interface{}{"script_id": 11})
	}))
	defer srv.Close()

	teamID := uint(3)
	scriptID, err := newTestClient(srv.URL).CreateScript(t.Context(), &teamID, "restart.sh", "echo hi")
	if err != nil {
		t.Fatalf("CreateScript: %v", err)
	}
	if scriptID != 11 {
		t.Fatalf("expected script_id 11, got %d", scriptID)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Fatalf("expected multipart body, got %q", gotContentType)
	}
	if gotFleetID != "3" {
		t.Fatalf("expected fleet_id 3, got %q", gotFleetID)
	}
	if gotFileName != "restart.sh" || gotBody != "echo hi" {
		t.Fatalf("unexpected file part: name=%q body=%q", gotFileName, gotBody)
	}
}

func TestDeleteScript_SurfacesFleetError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}))
	defer srv.Close()

	err := newTestClient(srv.URL).DeleteScript(t.Context(), 404)
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
}
