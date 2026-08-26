package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

// Script represents a saved script in Fleet. TeamID is nil for scripts at the
// Unassigned scope.
type Script struct {
	ID        uint   `json:"id"`
	TeamID    *uint  `json:"team_id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Contents  string `json:"contents,omitempty"`
}

// GetScripts lists saved scripts scoped to one fleet. A nil teamID lists the
// Unassigned scope.
func (fc *FleetClient) GetScripts(ctx context.Context, teamID *uint, perPage int) ([]Script, error) {
	params := url.Values{}
	if teamID != nil {
		params.Set("team_id", strconv.FormatUint(uint64(*teamID), 10))
	}
	params.Set("per_page", strconv.Itoa(perPage))

	resp, err := fc.makeFleetRequest(ctx, "GET", "/api/v1/fleet/scripts?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get scripts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get scripts: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}

	var result struct {
		Scripts []Script `json:"scripts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode scripts: %w", err)
	}
	return result.Scripts, nil
}

// GetScript returns a saved script's metadata, with its contents filled in when
// withContents is set. Fleet serves the body from the same route under
// ?alt=media, so that costs a second call.
func (fc *FleetClient) GetScript(ctx context.Context, scriptID uint, withContents bool) (*Script, error) {
	endpoint := fmt.Sprintf("/api/v1/fleet/scripts/%d", scriptID)

	resp, err := fc.makeFleetRequest(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get script: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get script: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}

	var script Script
	if err := json.NewDecoder(resp.Body).Decode(&script); err != nil {
		return nil, fmt.Errorf("failed to decode script: %w", err)
	}

	if withContents {
		contents, err := fc.getScriptContents(ctx, scriptID)
		if err != nil {
			return nil, err
		}
		script.Contents = contents
	}
	return &script, nil
}

func (fc *FleetClient) getScriptContents(ctx context.Context, scriptID uint) (string, error) {
	endpoint := fmt.Sprintf("/api/v1/fleet/scripts/%d?alt=media", scriptID)

	resp, err := fc.makeFleetRequest(ctx, "GET", endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get script contents: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read script contents: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get script contents: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}
	return string(bodyBytes), nil
}

// CreateScript uploads a new saved script. The name's extension is what Fleet
// uses to pick the interpreter (.sh, .ps1), and teamID scopes it to a fleet.
func (fc *FleetClient) CreateScript(ctx context.Context, teamID *uint, name, contents string) (uint, error) {
	fields := map[string]string{}
	if teamID != nil {
		fields["fleet_id"] = strconv.FormatUint(uint64(*teamID), 10)
	}

	resp, err := fc.makeFleetMultipartRequest(ctx, "POST", "/api/v1/fleet/scripts", fields, name, []byte(contents))
	if err != nil {
		return 0, fmt.Errorf("failed to create script: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("failed to create script: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}

	var result struct {
		ScriptID uint `json:"script_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("failed to decode created script: %w", err)
	}
	return result.ScriptID, nil
}

// UpdateScript replaces a saved script's contents. Fleet keeps the script's
// name and fleet — neither can be changed through this route.
func (fc *FleetClient) UpdateScript(ctx context.Context, scriptID uint, name, contents string) (*Script, error) {
	endpoint := fmt.Sprintf("/api/v1/fleet/scripts/%d", scriptID)

	resp, err := fc.makeFleetMultipartRequest(ctx, "PATCH", endpoint, nil, name, []byte(contents))
	if err != nil {
		return nil, fmt.Errorf("failed to update script: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to update script: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}

	var script Script
	if err := json.NewDecoder(resp.Body).Decode(&script); err != nil {
		return nil, fmt.Errorf("failed to decode updated script: %w", err)
	}
	return &script, nil
}

// DeleteScript deletes a saved script by ID.
func (fc *FleetClient) DeleteScript(ctx context.Context, scriptID uint) error {
	endpoint := fmt.Sprintf("/api/v1/fleet/scripts/%d", scriptID)

	resp, err := fc.makeFleetRequest(ctx, "DELETE", endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to delete script: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to delete script: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}
	return nil
}

// makeFleetMultipartRequest posts a multipart/form-data body, which Fleet's
// script upload routes require instead of JSON. The file always goes in the
// "script" part.
func (fc *FleetClient) makeFleetMultipartRequest(ctx context.Context, method, endpoint string, fields map[string]string, fileName string, fileContents []byte) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			return nil, fmt.Errorf("failed to write form field %s: %w", k, err)
		}
	}
	part, err := writer.CreateFormFile("script", fileName)
	if err != nil {
		return nil, fmt.Errorf("failed to create form file: %w", err)
	}
	if _, err := part.Write(fileContents); err != nil {
		return nil, fmt.Errorf("failed to write form file: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, fc.baseURL+endpoint, &body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+fc.apiKey)

	pathOnly, _, _ := strings.Cut(endpoint, "?")
	logrus.Debugf("%s %s", method, pathOnly)

	resp, err := fc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request to Fleet API: %w", err)
	}
	return resp, nil
}

// ScriptRun is the outcome of one script execution. Fleet returns three
// different shapes here — the async accept ({host_id, execution_id}), the sync
// result, and the stored result lookup — so this is the union of all three.
// ExitCode nil with HostTimeout false means the host has not reported back yet.
type ScriptRun struct {
	HostID         uint   `json:"host_id"`
	HostName       string `json:"hostname,omitempty"`
	ExecutionID    string `json:"execution_id"`
	ScriptID       *uint  `json:"script_id,omitempty"`
	ScriptContents string `json:"script_contents,omitempty"`
	Output         string `json:"output,omitempty"`
	ExitCode       *int64 `json:"exit_code"`
	Runtime        int    `json:"runtime,omitempty"`
	Message        string `json:"message,omitempty"`
	HostTimeout    bool   `json:"host_timeout,omitempty"`
}

// runScriptRequest is the body for both run routes. Every optional field is a
// pointer so an unset one is omitted rather than sent as a zero that Fleet
// would read as "the Unassigned fleet" or "script 0".
type runScriptRequest struct {
	HostID         uint   `json:"host_id"`
	ScriptID       *uint  `json:"script_id,omitempty"`
	ScriptContents string `json:"script_contents,omitempty"`
	ScriptName     string `json:"script_name,omitempty"`
	TeamID         *uint  `json:"team_id,omitempty"`
}

// ScriptBatchStatus is the progress of a batch script run.
type ScriptBatchStatus struct {
	BatchExecutionID   string `json:"batch_execution_id"`
	ScriptID           *uint  `json:"script_id,omitempty"`
	ScriptName         string `json:"script_name,omitempty"`
	TeamID             *uint  `json:"team_id,omitempty"`
	Status             string `json:"status"`
	Canceled           bool   `json:"canceled"`
	TargetedHostCount  *uint  `json:"targeted_host_count,omitempty"`
	PendingHostCount   *uint  `json:"pending_host_count,omitempty"`
	RanHostCount       *uint  `json:"ran_host_count,omitempty"`
	ErroredHostCount   *uint  `json:"errored_host_count,omitempty"`
	CanceledHostCount  *uint  `json:"canceled_host_count,omitempty"`
	IncompatibleHostCt *uint  `json:"incompatible_host_count,omitempty"`
}

// RunScript executes a script on one host. Pass scriptID to run a saved
// script — Fleet then requires the script and the host to be in the same
// fleet — or contents to run an anonymous one, in which case teamID scopes the
// authorization check.
//
// wait picks Fleet's synchronous route, which blocks until the host reports
// back or Fleet's own timeout fires. A timeout is not an error: Fleet answers
// 408 with host_timeout set, and the returned ScriptRun carries the execution
// ID so the caller can poll GetScriptResult instead.
func (fc *FleetClient) RunScript(ctx context.Context, hostID uint, scriptID *uint, contents, name string, teamID *uint, wait bool) (*ScriptRun, error) {
	endpoint := "/api/v1/fleet/scripts/run"
	if wait {
		endpoint += "/sync"
	}

	resp, err := fc.makeFleetRequest(ctx, "POST", endpoint, runScriptRequest{
		HostID:         hostID,
		ScriptID:       scriptID,
		ScriptContents: contents,
		ScriptName:     name,
		TeamID:         teamID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to run script: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read run script response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusRequestTimeout:
	default:
		return nil, fmt.Errorf("failed to run script: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}

	var run ScriptRun
	if err := json.Unmarshal(bodyBytes, &run); err != nil {
		return nil, fmt.Errorf("failed to decode run script response: %w", err)
	}
	if resp.StatusCode == http.StatusRequestTimeout {
		run.HostTimeout = true
	}
	return &run, nil
}

// GetScriptResult fetches a script execution's stored result. An ExitCode of
// nil means the host has not reported back yet.
func (fc *FleetClient) GetScriptResult(ctx context.Context, executionID string) (*ScriptRun, error) {
	endpoint := fmt.Sprintf("/api/v1/fleet/scripts/results/%s", url.PathEscape(executionID))

	resp, err := fc.makeFleetRequest(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get script result: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get script result: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}

	var run ScriptRun
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		return nil, fmt.Errorf("failed to decode script result: %w", err)
	}
	return &run, nil
}

// RunScriptBatch queues a saved script across an explicit host set and returns
// the batch execution ID to poll with GetScriptBatchStatus. Fleet also accepts
// a server-side filter map on this route, but callers here resolve targets up
// front so the operator confirms the exact host list.
func (fc *FleetClient) RunScriptBatch(ctx context.Context, scriptID uint, hostIDs []uint) (string, error) {
	resp, err := fc.makeFleetRequest(ctx, "POST", "/api/v1/fleet/scripts/run/batch", map[string]interface{}{
		"script_id": scriptID,
		"host_ids":  hostIDs,
	})
	if err != nil {
		return "", fmt.Errorf("failed to run script batch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to run script batch: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}

	var result struct {
		BatchExecutionID string `json:"batch_execution_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode batch response: %w", err)
	}
	return result.BatchExecutionID, nil
}

// GetScriptBatchStatus returns a batch script run's progress counts.
func (fc *FleetClient) GetScriptBatchStatus(ctx context.Context, batchExecutionID string) (*ScriptBatchStatus, error) {
	endpoint := fmt.Sprintf("/api/v1/fleet/scripts/batch/%s", url.PathEscape(batchExecutionID))

	resp, err := fc.makeFleetRequest(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get batch status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get batch status: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}

	var status ScriptBatchStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("failed to decode batch status: %w", err)
	}
	return &status, nil
}
