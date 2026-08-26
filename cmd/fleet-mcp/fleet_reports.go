package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// UpdateQueryRequest is the PATCH payload for a saved report. Every field is a
// pointer so an omitted field keeps its stored value.
type UpdateQueryRequest struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Query       *string `json:"query,omitempty"`
	Platform    *string `json:"platform,omitempty"`
}

// resolveFleetID maps a fleet name to its ID. An empty name resolves to nil,
// which callers pass through as "no fleet filter" rather than as Unassigned.
func (fc *FleetClient) resolveFleetID(ctx context.Context, name string) (*uint, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil
	}
	ids, err := fc.resolveTeamNames(ctx, []string{name})
	if err != nil {
		return nil, err
	}
	return &ids[0], nil
}

// GetQueriesForFleet lists saved reports scoped to one fleet. A nil teamID
// lists the Unassigned scope only — use GetQueries for the union across every
// fleet. mergeInherited additionally returns the reports the fleet inherits.
func (fc *FleetClient) GetQueriesForFleet(ctx context.Context, teamID *uint, platform string, mergeInherited bool) ([]Query, error) {
	params := url.Values{}
	if teamID != nil {
		params.Set("team_id", strconv.FormatUint(uint64(*teamID), 10))
		if mergeInherited {
			params.Set("merge_inherited", "true")
		}
	}
	if p := normalizePlatform(platform); p != "" {
		params.Set("platform", p)
	}

	endpoint := "/api/v1/fleet/reports"
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	resp, err := fc.makeFleetRequest(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get reports: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get reports: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}

	var result struct {
		Queries []Query `json:"queries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode reports: %w", err)
	}
	return result.Queries, nil
}

// UpdateSavedQuery patches a saved report in place. The report keeps its fleet
// — Fleet has no route for moving a report between fleets.
func (fc *FleetClient) UpdateSavedQuery(ctx context.Context, queryID uint, payload UpdateQueryRequest) (*Query, error) {
	endpoint := fmt.Sprintf("/api/v1/fleet/reports/%d", queryID)

	resp, err := fc.makeFleetRequest(ctx, "PATCH", endpoint, payload)
	if err != nil {
		return nil, fmt.Errorf("failed to update report: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to update report: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}

	var result struct {
		Query Query `json:"query"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode updated report: %w", err)
	}
	return &result.Query, nil
}

// DeleteSavedQuery deletes a saved report by ID.
func (fc *FleetClient) DeleteSavedQuery(ctx context.Context, queryID uint) error {
	endpoint := fmt.Sprintf("/api/v1/fleet/reports/id/%d", queryID)

	resp, err := fc.makeFleetRequest(ctx, "DELETE", endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to delete report: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to delete report: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}
	return nil
}

// GetPoliciesForFleet lists the policies owned by one fleet. A nil teamID
// returns the global policies only — use GetPolicies for the union across
// every fleet.
func (fc *FleetClient) GetPoliciesForFleet(ctx context.Context, teamID *uint) ([]Policy, error) {
	endpoint := "/api/v1/fleet/global/policies"
	if teamID != nil {
		endpoint = fmt.Sprintf("/api/v1/fleet/fleets/%d/policies", *teamID)
	}

	resp, err := fc.makeFleetRequest(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get policies: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get policies: %s", fleetErrMsg(resp.StatusCode, bodyBytes))
	}

	var result struct {
		Policies []Policy `json:"policies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode policies: %w", err)
	}
	return result.Policies, nil
}
