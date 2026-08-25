package service

import (
	"context"
	"encoding/json"
	"testing"

	activity_api "github.com/fleetdm/fleet/v4/server/activity/api"
	"github.com/fleetdm/fleet/v4/server/contexts/viewer"
	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/fleetdm/fleet/v4/server/mock"
	"github.com/stretchr/testify/require"
)

func teamsTestStore() *mock.Store {
	ds := new(mock.Store)
	ds.AppConfigFunc = func(ctx context.Context) (*fleet.AppConfig, error) {
		return &fleet.AppConfig{}, nil
	}
	ds.TeamConflictsWithNameFunc = func(ctx context.Context, name string, excludeID uint) (*fleet.Team, error) {
		return nil, nil
	}
	ds.NewTeamFunc = func(ctx context.Context, team *fleet.Team) (*fleet.Team, error) {
		team.ID = 1
		return team, nil
	}
	ds.SaveTeamFunc = func(ctx context.Context, team *fleet.Team) (*fleet.Team, error) {
		return team, nil
	}
	ds.TeamWithExtrasFunc = func(ctx context.Context, tid uint) (*fleet.Team, error) {
		return &fleet.Team{ID: tid, Name: "fleet1"}, nil
	}
	ds.TeamLiteFunc = func(ctx context.Context, tid uint) (*fleet.TeamLite, error) {
		return &fleet.TeamLite{ID: tid, Name: "fleet1"}, nil
	}
	ds.DeleteTeamFunc = func(ctx context.Context, tid uint) error { return nil }
	ds.ApplyEnrollSecretsFunc = func(ctx context.Context, teamID *uint, secrets []*fleet.EnrollSecret) error {
		return nil
	}
	ds.GetEnrollSecretsFunc = func(ctx context.Context, teamID *uint) ([]*fleet.EnrollSecret, error) {
		return []*fleet.EnrollSecret{{Secret: "abc", TeamID: teamID}}, nil
	}
	ds.DefaultTeamConfigFunc = func(ctx context.Context) (*fleet.TeamConfig, error) {
		return &fleet.TeamConfig{}, nil
	}
	ds.SaveDefaultTeamConfigFunc = func(ctx context.Context, config *fleet.TeamConfig) error { return nil }
	ds.ListTeamsFunc = func(ctx context.Context, filter fleet.TeamFilter, opt fleet.ListOptions) ([]*fleet.Team, error) {
		return []*fleet.Team{{ID: 1, Name: "fleet1"}}, nil
	}
	ds.ListUsersFunc = func(ctx context.Context, opt fleet.UserListOptions) ([]*fleet.User, error) {
		return []*fleet.User{{ID: 7}}, nil
	}
	ds.TeamByNameFunc = func(ctx context.Context, name string) (*fleet.Team, error) {
		return nil, newNotFoundError()
	}
	return ds
}

func adminViewerCtx(ctx context.Context) context.Context {
	return viewer.NewContext(ctx, viewer.Viewer{
		User: &fleet.User{ID: 1, GlobalRole: new(fleet.RoleAdmin)},
	})
}

func TestNewTeamGeneratesEnrollSecret(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	team, err := svc.NewTeam(ctx, fleet.TeamPayload{Name: new("fleet1")})
	require.NoError(t, err)
	require.Equal(t, "fleet1", team.Name)
	require.Len(t, team.Secrets, 1)
	require.NotEmpty(t, team.Secrets[0].Secret)
	require.True(t, ds.NewTeamFuncInvoked)
}

func TestNewTeamRejectsInvalidNames(t *testing.T) {
	for name, teamName := range map[string]string{
		"empty":                       "  ",
		"reserved":                    "No team",
		"reserved unicode-normalized": "unassigned",
	} {
		t.Run(name, func(t *testing.T) {
			ds := teamsTestStore()
			svc, ctx := newTestService(t, ds, nil, nil)
			ctx = adminViewerCtx(ctx)

			_, err := svc.NewTeam(ctx, fleet.TeamPayload{Name: &teamName})
			require.Error(t, err)
			require.False(t, ds.NewTeamFuncInvoked)
		})
	}
}

func TestNewTeamRejectsDuplicateName(t *testing.T) {
	ds := teamsTestStore()
	ds.TeamConflictsWithNameFunc = func(ctx context.Context, name string, excludeID uint) (*fleet.Team, error) {
		return &fleet.Team{ID: 9, Name: name}, nil
	}
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	_, err := svc.NewTeam(ctx, fleet.TeamPayload{Name: new("fleet1")})
	require.Error(t, err)
	require.False(t, ds.NewTeamFuncInvoked)
}

func TestNewTeamRejectsMDMPayload(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	_, err := svc.NewTeam(ctx, fleet.TeamPayload{
		Name: new("fleet1"),
		MDM:  &fleet.TeamPayloadMDM{},
	})
	require.Error(t, err)
	require.False(t, ds.NewTeamFuncInvoked)
}

func TestModifyTeamAppliesConfig(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	team, err := svc.ModifyTeam(ctx, 1, fleet.TeamPayload{
		Description: new("support desk"),
		WebhookSettings: &fleet.TeamWebhookSettings{
			FailingPoliciesWebhook: fleet.FailingPoliciesWebhookSettings{
				Enable:         true,
				DestinationURL: "https://example.com/hook",
			},
		},
		HostExpirySettings: &fleet.HostExpirySettings{HostExpiryEnabled: true, HostExpiryWindow: 30},
	})
	require.NoError(t, err)
	require.Equal(t, "support desk", team.Description)
	require.True(t, team.Config.WebhookSettings.FailingPoliciesWebhook.Enable)
	require.Equal(t, 30, team.Config.HostExpirySettings.HostExpiryWindow)
	require.True(t, ds.SaveTeamFuncInvoked)
}

func TestModifyTeamAppliesSecretsSeparately(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	// SaveTeam doesn't persist secrets, so a payload carrying them must reach
	// ApplyEnrollSecrets or they would be silently dropped.
	_, err := svc.ModifyTeam(ctx, 1, fleet.TeamPayload{
		Secrets: []*fleet.EnrollSecret{{Secret: "new-secret"}},
	})
	require.NoError(t, err)
	require.True(t, ds.ApplyEnrollSecretsFuncInvoked)
}

func TestUnassignedFleetIsReadableAndNotDeletable(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	team, err := svc.GetTeam(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, uint(0), team.ID)
	require.Equal(t, fleet.ReservedNameNoTeam, team.Name)
	require.False(t, ds.TeamWithExtrasFuncInvoked)

	require.Error(t, svc.DeleteTeam(ctx, 0))
	require.False(t, ds.DeleteTeamFuncInvoked)
}

func TestModifyUnassignedFleetSavesDefaultConfig(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	_, err := svc.ModifyTeam(ctx, 0, fleet.TeamPayload{
		WebhookSettings: &fleet.TeamWebhookSettings{
			FailingPoliciesWebhook: fleet.FailingPoliciesWebhookSettings{Enable: true},
		},
	})
	require.NoError(t, err)
	require.True(t, ds.SaveDefaultTeamConfigFuncInvoked)
	require.False(t, ds.SaveTeamFuncInvoked)

	// It has no name of its own to rename.
	_, err = svc.ModifyTeam(ctx, 0, fleet.TeamPayload{Name: new("renamed")})
	require.Error(t, err)
}

func TestDeleteTeamEmitsActivity(t *testing.T) {
	ds := teamsTestStore()
	opts := &TestServerOpts{}
	svc, ctx := newTestService(t, ds, nil, nil, opts)
	ctx = adminViewerCtx(ctx)

	var recorded string
	opts.ActivityMock.NewActivityFunc = func(_ context.Context, _ *activity_api.User, details activity_api.ActivityDetails) error {
		recorded = details.ActivityName()
		return nil
	}

	require.NoError(t, svc.DeleteTeam(ctx, 1))
	require.True(t, ds.DeleteTeamFuncInvoked)
	require.Equal(t, "deleted_team", recorded)
}

func TestAddAndDeleteTeamUsers(t *testing.T) {
	ds := teamsTestStore()
	ds.TeamWithExtrasFunc = func(ctx context.Context, tid uint) (*fleet.Team, error) {
		return &fleet.Team{
			ID:    tid,
			Name:  "fleet1",
			Users: []fleet.TeamUser{{User: fleet.User{ID: 7}, Role: fleet.RoleObserver}},
		}, nil
	}
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	// Re-adding an existing member updates the role instead of duplicating it.
	team, err := svc.AddTeamUsers(ctx, 1, []fleet.TeamUser{
		{User: fleet.User{ID: 7}, Role: fleet.RoleAdmin},
		{User: fleet.User{ID: 8}, Role: fleet.RoleMaintainer},
	})
	require.NoError(t, err)
	require.Len(t, team.Users, 2)
	require.Equal(t, uint(7), team.Users[0].ID)
	require.Equal(t, fleet.RoleAdmin, team.Users[0].Role)

	ds.TeamWithExtrasFunc = func(ctx context.Context, tid uint) (*fleet.Team, error) {
		return &fleet.Team{
			ID:   tid,
			Name: "fleet1",
			Users: []fleet.TeamUser{
				{User: fleet.User{ID: 7}, Role: fleet.RoleAdmin},
				{User: fleet.User{ID: 8}, Role: fleet.RoleMaintainer},
			},
		}, nil
	}

	team, err = svc.DeleteTeamUsers(ctx, 1, []fleet.TeamUser{{User: fleet.User{ID: 7}}})
	require.NoError(t, err)
	require.Len(t, team.Users, 1)
	require.Equal(t, uint(8), team.Users[0].ID)
}

func TestAddTeamUsersRejectsUnknownRole(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	_, err := svc.AddTeamUsers(ctx, 1, []fleet.TeamUser{{User: fleet.User{ID: 7}, Role: "superuser"}})
	require.Error(t, err)
	require.False(t, ds.SaveTeamFuncInvoked)
}

func TestTeamPremiumRolesAreAvailable(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	for _, role := range []string{fleet.RoleObserverPlus, fleet.RoleTechnician, fleet.RoleGitOps} {
		_, err := svc.AddTeamUsers(ctx, 1, []fleet.TeamUser{{User: fleet.User{ID: 7}, Role: role}})
		require.NoErrorf(t, err, "role %s", role)
	}
}

func TestModifyTeamAgentOptions(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	opts := json.RawMessage(`{"config":{"options":{"distributed_interval":10}}}`)
	team, err := svc.ModifyTeamAgentOptions(ctx, 1, opts, fleet.ApplySpecOptions{})
	require.NoError(t, err)
	require.NotNil(t, team.Config.AgentOptions)
	require.JSONEq(t, string(opts), string(*team.Config.AgentOptions))

	// A dry run validates without persisting.
	ds.SaveTeamFuncInvoked = false
	_, err = svc.ModifyTeamAgentOptions(ctx, 1, opts, fleet.ApplySpecOptions{DryRun: true})
	require.NoError(t, err)
	require.False(t, ds.SaveTeamFuncInvoked)

	// Unknown keys are rejected unless forced.
	bad := json.RawMessage(`{"config":{"options":{"not_a_real_option":1}}}`)
	_, err = svc.ModifyTeamAgentOptions(ctx, 1, bad, fleet.ApplySpecOptions{})
	require.Error(t, err)

	_, err = svc.ModifyTeamAgentOptions(ctx, 1, bad, fleet.ApplySpecOptions{Force: true})
	require.NoError(t, err)
}

func TestApplyTeamSpecsCreatesAndUpdates(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	// Unknown name -> created, with a generated enroll secret.
	idsByName, err := svc.ApplyTeamSpecs(ctx, []*fleet.TeamSpec{{Name: "fleet1"}}, fleet.ApplyTeamSpecOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]uint{"fleet1": 1}, idsByName)
	require.True(t, ds.NewTeamFuncInvoked)

	// Known name -> updated in place.
	ds.NewTeamFuncInvoked = false
	ds.TeamByNameFunc = func(ctx context.Context, name string) (*fleet.Team, error) {
		return &fleet.Team{ID: 4, Name: name}, nil
	}
	idsByName, err = svc.ApplyTeamSpecs(ctx, []*fleet.TeamSpec{{
		Name:               "fleet1",
		HostExpirySettings: &fleet.HostExpirySettings{HostExpiryEnabled: true, HostExpiryWindow: 15},
	}}, fleet.ApplyTeamSpecOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]uint{"fleet1": 4}, idsByName)
	require.False(t, ds.NewTeamFuncInvoked)
	require.True(t, ds.SaveTeamFuncInvoked)
}

func TestApplyTeamSpecsValidatesWholeBatchBeforeWriting(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	// The second spec is invalid, so the first must not be written either.
	_, err := svc.ApplyTeamSpecs(ctx, []*fleet.TeamSpec{
		{Name: "fleet1"},
		{Name: "No team"},
	}, fleet.ApplyTeamSpecOptions{})
	require.Error(t, err)
	require.False(t, ds.NewTeamFuncInvoked)
	require.False(t, ds.SaveTeamFuncInvoked)
}

func TestApplyTeamSpecsDryRunDoesNotWrite(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	_, err := svc.ApplyTeamSpecs(ctx, []*fleet.TeamSpec{{Name: "fleet1"}},
		fleet.ApplyTeamSpecOptions{ApplySpecOptions: fleet.ApplySpecOptions{DryRun: true}})
	require.NoError(t, err)
	require.False(t, ds.NewTeamFuncInvoked)
	require.False(t, ds.SaveTeamFuncInvoked)
}

func TestTeamEnrollSecrets(t *testing.T) {
	ds := teamsTestStore()
	svc, ctx := newTestService(t, ds, nil, nil)
	ctx = adminViewerCtx(ctx)

	secrets, err := svc.TeamEnrollSecrets(ctx, 1)
	require.NoError(t, err)
	require.Len(t, secrets, 1)

	_, err = svc.ModifyTeamEnrollSecrets(ctx, 1, []fleet.EnrollSecret{{Secret: "replacement"}})
	require.NoError(t, err)
	require.True(t, ds.ApplyEnrollSecretsFuncInvoked)

	_, err = svc.ModifyTeamEnrollSecrets(ctx, 1, []fleet.EnrollSecret{{Secret: "  "}})
	require.Error(t, err)
}

func TestTeamAuthorization(t *testing.T) {
	ds := teamsTestStore()
	svc, baseCtx := newTestService(t, ds, nil, nil)

	observer := &fleet.User{ID: 2, GlobalRole: new(fleet.RoleObserver)}
	observerCtx := viewer.NewContext(baseCtx, viewer.Viewer{User: observer})

	_, err := svc.NewTeam(observerCtx, fleet.TeamPayload{Name: new("fleet1")})
	require.Error(t, err)

	_, err = svc.ListTeams(observerCtx, fleet.ListOptions{})
	require.NoError(t, err)

	// A member of one fleet may not administer another.
	teamAdmin := &fleet.User{
		ID:    3,
		Teams: []fleet.UserTeam{{Team: fleet.Team{ID: 1}, Role: fleet.RoleAdmin}},
	}
	teamAdminCtx := viewer.NewContext(baseCtx, viewer.Viewer{User: teamAdmin})

	_, err = svc.AddTeamUsers(teamAdminCtx, 1, []fleet.TeamUser{{User: fleet.User{ID: 9}, Role: fleet.RoleObserver}})
	require.NoError(t, err)

	_, err = svc.AddTeamUsers(teamAdminCtx, 2, []fleet.TeamUser{{User: fleet.User{ID: 9}, Role: fleet.RoleObserver}})
	require.Error(t, err)
}

func TestHostFeaturesComeFromTheHostsFleet(t *testing.T) {
	ds := teamsTestStore()
	ds.AppConfigFunc = func(ctx context.Context) (*fleet.AppConfig, error) {
		return &fleet.AppConfig{Features: fleet.Features{EnableHostUsers: false}}, nil
	}
	ds.TeamFeaturesFunc = func(ctx context.Context, teamID uint) (*fleet.Features, error) {
		return &fleet.Features{EnableHostUsers: true}, nil
	}
	// HostFeatures is not on the fleet.Service interface, so exercise the
	// concrete service directly.
	svc := &Service{ds: ds}
	ctx := t.Context()

	features, err := svc.HostFeatures(ctx, &fleet.Host{ID: 1})
	require.NoError(t, err)
	require.False(t, features.EnableHostUsers)

	features, err = svc.HostFeatures(ctx, &fleet.Host{ID: 1, TeamID: new(uint(3))})
	require.NoError(t, err)
	require.True(t, features.EnableHostUsers)
}
