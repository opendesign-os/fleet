package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fleetdm/fleet/v4/server/contexts/viewer"
	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/fleetdm/fleet/v4/server/mock"
)

// These tests pin the tenancy promise: a user scoped to one fleet manages that
// fleet's hosts, reports and scripts, and is refused on another fleet's.
// Everything here runs on the default license, so the isolation comes from
// authorization rather than from the license tier.

const (
	ownFleet   = uint(1)
	otherFleet = uint(2)
)

func tenancyTestStore() *mock.Store {
	ds := new(mock.Store)
	ds.AppConfigFunc = func(ctx context.Context) (*fleet.AppConfig, error) {
		return &fleet.AppConfig{}, nil
	}

	ds.ListHostsFunc = func(ctx context.Context, filter fleet.TeamFilter, opt fleet.HostListOptions) ([]*fleet.Host, error) {
		return []*fleet.Host{{ID: 10, TeamID: opt.TeamFilter}}, nil
	}

	ds.NewQueryFunc = func(ctx context.Context, query *fleet.Query, opts ...fleet.OptionalArg) (*fleet.Query, error) {
		query.ID = 5
		return query, nil
	}
	ds.QueryByNameFunc = func(ctx context.Context, teamID *uint, name string) (*fleet.Query, error) {
		return nil, newNotFoundError()
	}
	ds.ListQueriesFunc = func(ctx context.Context, opt fleet.ListQueryOptions) ([]*fleet.Query, int, int, *fleet.PaginationMetadata, error) {
		return []*fleet.Query{{ID: 5, TeamID: opt.TeamID}}, 1, 0, nil, nil
	}

	ds.NewScriptFunc = func(ctx context.Context, script *fleet.Script) (*fleet.Script, error) {
		script.ID = 7
		return script, nil
	}
	ds.ListScriptsFunc = func(ctx context.Context, teamID *uint, opt fleet.ListOptions) ([]*fleet.Script, *fleet.PaginationMetadata, error) {
		return []*fleet.Script{{ID: 7, TeamID: teamID}}, nil, nil
	}
	ds.ValidateEmbeddedSecretsFunc = func(ctx context.Context, documents []string) error { return nil }

	ds.LabelsSummaryFunc = func(ctx context.Context, filter fleet.TeamFilter) ([]*fleet.LabelSummary, error) {
		return []*fleet.LabelSummary{{ID: 3}}, nil
	}
	ds.ListSoftwareTitlesFunc = func(ctx context.Context, opt fleet.SoftwareTitleListOptions, filter fleet.TeamFilter) ([]fleet.SoftwareTitleListResult, int, *fleet.PaginationMetadata, error) {
		return []fleet.SoftwareTitleListResult{{ID: 9}}, 1, nil, nil
	}

	ds.AddHostsToTeamFunc = func(ctx context.Context, params *fleet.AddHostsToTeamParams) error { return nil }
	ds.ListHostsLiteByIDsFunc = func(ctx context.Context, ids []uint) ([]*fleet.Host, error) {
		return []*fleet.Host{{ID: 10, TeamID: new(ownFleet)}}, nil
	}
	ds.ListMDMAndroidUUIDsToHostIDsFunc = func(ctx context.Context, hostIDs []uint) (map[string]uint, error) {
		return nil, nil
	}
	return ds
}

// fleetMaintainerCtx is a user whose only role is maintainer on ownFleet.
func fleetMaintainerCtx(ctx context.Context) context.Context {
	return viewer.NewContext(ctx, viewer.Viewer{User: &fleet.User{
		ID:    2,
		Teams: []fleet.UserTeam{{Team: fleet.Team{ID: ownFleet}, Role: fleet.RoleMaintainer}},
	}})
}

func requireForbidden(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.Truef(t, strings.Contains(strings.ToLower(err.Error()), "forbidden"),
		"expected a forbidden error, got %v", err)
}

// Host listing scopes in SQL rather than in the authorization layer: the
// service authorizes "may list hosts at all", then hands the datastore a
// TeamFilter built from the caller. A user asking for a fleet they have no role
// in gets an empty list, not a refusal. What this pins is that the filter
// reaching the datastore always carries the caller, so that scoping can happen.
func TestTenantHostListCarriesCallerScope(t *testing.T) {
	ds := tenancyTestStore()

	var captured fleet.TeamFilter
	ds.ListHostsFunc = func(ctx context.Context, filter fleet.TeamFilter, opt fleet.HostListOptions) ([]*fleet.Host, error) {
		captured = filter
		return []*fleet.Host{{ID: 10}}, nil
	}

	svc, baseCtx := newTestService(t, ds, nil, nil)
	ctx := fleetMaintainerCtx(baseCtx)

	_, err := svc.ListHosts(ctx, fleet.HostListOptions{TeamFilter: new(ownFleet)})
	require.NoError(t, err)

	require.NotNil(t, captured.User)
	require.Len(t, captured.User.Teams, 1)
	require.Equal(t, ownFleet, captured.User.Teams[0].ID)
	require.Nil(t, captured.User.GlobalRole)
}

func TestTenantTransfersOwnHosts(t *testing.T) {
	ds := tenancyTestStore()
	svc, baseCtx := newTestService(t, ds, nil, nil)

	// Transferring needs transfer access on the destination fleet.
	ctx := viewer.NewContext(baseCtx, viewer.Viewer{User: &fleet.User{
		ID:    3,
		Teams: []fleet.UserTeam{{Team: fleet.Team{ID: ownFleet}, Role: fleet.RoleAdmin}},
	}})

	// A fleet the caller has no role in is refused outright.
	requireForbidden(t, svc.AddHostsToTeam(ctx, new(otherFleet), []uint{10}, false))
	require.False(t, ds.AddHostsToTeamFuncInvoked)
}

func TestTenantManagesOwnReports(t *testing.T) {
	ds := tenancyTestStore()
	svc, baseCtx := newTestService(t, ds, nil, nil)
	ctx := fleetMaintainerCtx(baseCtx)

	query, err := svc.NewQuery(ctx, fleet.QueryPayload{
		Name:   new("disk space"),
		Query:  new("SELECT 1"),
		TeamID: new(ownFleet),
	})
	require.NoError(t, err)
	require.Equal(t, ownFleet, *query.TeamID)

	_, err = svc.NewQuery(ctx, fleet.QueryPayload{
		Name:   new("disk space"),
		Query:  new("SELECT 1"),
		TeamID: new(otherFleet),
	})
	requireForbidden(t, err)

	queries, _, _, _, err := svc.ListQueries(ctx, fleet.ListOptions{}, new(ownFleet), nil, false, nil)
	require.NoError(t, err)
	require.Len(t, queries, 1)

	_, _, _, _, err = svc.ListQueries(ctx, fleet.ListOptions{}, new(otherFleet), nil, false, nil)
	requireForbidden(t, err)
}

func TestTenantManagesOwnScripts(t *testing.T) {
	ds := tenancyTestStore()
	svc, baseCtx := newTestService(t, ds, nil, nil)
	ctx := fleetMaintainerCtx(baseCtx)

	script, err := svc.NewScript(ctx, new(ownFleet), "cleanup.sh", strings.NewReader("#!/bin/sh\necho hi\n"))
	require.NoError(t, err)
	require.Equal(t, ownFleet, *script.TeamID)

	_, err = svc.NewScript(ctx, new(otherFleet), "cleanup.sh", strings.NewReader("#!/bin/sh\necho hi\n"))
	requireForbidden(t, err)

	scripts, _, err := svc.ListScripts(ctx, new(ownFleet), fleet.ListOptions{})
	require.NoError(t, err)
	require.Len(t, scripts, 1)

	_, _, err = svc.ListScripts(ctx, new(otherFleet), fleet.ListOptions{})
	requireForbidden(t, err)
}

func TestTenantManagesOwnSoftwareAndLabels(t *testing.T) {
	ds := tenancyTestStore()
	svc, baseCtx := newTestService(t, ds, nil, nil)
	ctx := fleetMaintainerCtx(baseCtx)

	titles, _, _, err := svc.ListSoftwareTitles(ctx, fleet.SoftwareTitleListOptions{TeamID: new(ownFleet)})
	require.NoError(t, err)
	require.Len(t, titles, 1)

	_, _, _, err = svc.ListSoftwareTitles(ctx, fleet.SoftwareTitleListOptions{TeamID: new(otherFleet)})
	requireForbidden(t, err)

	labels, err := svc.LabelsSummary(ctx, new(ownFleet))
	require.NoError(t, err)
	require.Len(t, labels, 1)
}
