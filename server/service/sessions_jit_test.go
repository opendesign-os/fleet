package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/fleetdm/fleet/v4/server/mock"
)

// stubAuth is a verified SSO assertion carrying the attributes an IdP sends.
type stubAuth struct {
	email       string
	displayName string
	attributes  []fleet.SAMLAttribute
}

func (a stubAuth) UserID() string                             { return a.email }
func (a stubAuth) UserDisplayName() string                    { return a.displayName }
func (a stubAuth) AssertionAttributes() []fleet.SAMLAttribute { return a.attributes }

func roleAttribute(name, value string) fleet.SAMLAttribute {
	return fleet.SAMLAttribute{
		Name:   name,
		Values: []fleet.SAMLAttributeValue{{Value: value}},
	}
}

func jitTestStore(t *testing.T, jitEnabled bool) *mock.Store {
	t.Helper()
	ds := new(mock.Store)
	ds.AppConfigFunc = func(ctx context.Context) (*fleet.AppConfig, error) {
		return &fleet.AppConfig{
			SSOSettings: &fleet.SSOSettings{EnableSSO: true, EnableJITProvisioning: jitEnabled},
		}, nil
	}
	ds.UserByEmailFunc = func(ctx context.Context, email string) (*fleet.User, error) {
		return nil, newNotFoundError()
	}
	ds.NewUserFunc = func(ctx context.Context, user *fleet.User) (*fleet.User, error) {
		user.ID = 1
		return user, nil
	}
	ds.SaveUserFunc = func(ctx context.Context, user *fleet.User) error { return nil }
	return ds
}

func TestGetSSOUserProvisionsOnFirstLogin(t *testing.T) {
	ds := jitTestStore(t, true)
	svc, ctx := newTestService(t, ds, nil, nil)

	user, err := svc.GetSSOUser(ctx, stubAuth{
		email:       "bob@example.com",
		displayName: "Bob Stone",
		attributes:  []fleet.SAMLAttribute{roleAttribute("FLEET_JIT_USER_ROLE_GLOBAL", fleet.RoleMaintainer)},
	})
	require.NoError(t, err)
	require.True(t, ds.NewUserFuncInvoked)
	require.Equal(t, "bob@example.com", user.Email)
	require.Equal(t, "Bob Stone", user.Name)
	require.True(t, user.SSOEnabled)
	require.Equal(t, fleet.RoleMaintainer, *user.GlobalRole)
}

func TestGetSSOUserProvisionsWithFleetRoles(t *testing.T) {
	ds := jitTestStore(t, true)
	svc, ctx := newTestService(t, ds, nil, nil)

	user, err := svc.GetSSOUser(ctx, stubAuth{
		email:      "bob@example.com",
		attributes: []fleet.SAMLAttribute{roleAttribute("FLEET_JIT_USER_ROLE_FLEET_3", fleet.RoleObserver)},
	})
	require.NoError(t, err)
	require.Nil(t, user.GlobalRole)
	require.Len(t, user.Teams, 1)
	require.Equal(t, uint(3), user.Teams[0].ID)
	require.Equal(t, fleet.RoleObserver, user.Teams[0].Role)
	// Falls back to the email when the assertion carries no display name.
	require.Equal(t, "bob@example.com", user.Name)
}

// An assertion with no role attributes must not mint an admin. Observer is the
// safe landing spot until someone grants more.
func TestGetSSOUserProvisionsObserverWithoutRoleAttributes(t *testing.T) {
	ds := jitTestStore(t, true)
	svc, ctx := newTestService(t, ds, nil, nil)

	user, err := svc.GetSSOUser(ctx, stubAuth{email: "bob@example.com"})
	require.NoError(t, err)
	require.Equal(t, fleet.RoleObserver, *user.GlobalRole)
}

func TestGetSSOUserWithoutJITRejectsUnknownUser(t *testing.T) {
	ds := jitTestStore(t, false)
	svc, ctx := newTestService(t, ds, nil, nil)

	_, err := svc.GetSSOUser(ctx, stubAuth{email: "bob@example.com"})
	require.Error(t, err)
	require.False(t, ds.NewUserFuncInvoked)
}

func TestGetSSOUserRejectsConflictingRoleAttributes(t *testing.T) {
	ds := jitTestStore(t, true)
	svc, ctx := newTestService(t, ds, nil, nil)

	// A user cannot be both global and fleet-scoped.
	_, err := svc.GetSSOUser(ctx, stubAuth{
		email: "bob@example.com",
		attributes: []fleet.SAMLAttribute{
			roleAttribute("FLEET_JIT_USER_ROLE_GLOBAL", fleet.RoleAdmin),
			roleAttribute("FLEET_JIT_USER_ROLE_FLEET_1", fleet.RoleObserver),
		},
	})
	require.Error(t, err)
	require.False(t, ds.NewUserFuncInvoked)
}

func TestGetSSOUserSyncsRolesOnLogin(t *testing.T) {
	ds := jitTestStore(t, true)
	ds.UserByEmailFunc = func(ctx context.Context, email string) (*fleet.User, error) {
		return &fleet.User{ID: 1, Email: email, SSOEnabled: true, GlobalRole: new(fleet.RoleObserver)}, nil
	}
	svc, ctx := newTestService(t, ds, nil, nil)

	user, err := svc.GetSSOUser(ctx, stubAuth{
		email:      "bob@example.com",
		attributes: []fleet.SAMLAttribute{roleAttribute("FLEET_JIT_USER_ROLE_GLOBAL", fleet.RoleAdmin)},
	})
	require.NoError(t, err)
	require.True(t, ds.SaveUserFuncInvoked)
	require.Equal(t, fleet.RoleAdmin, *user.GlobalRole)
}

// A partial IdP configuration must not silently demote an existing user.
func TestGetSSOUserLeavesRolesAloneWithoutAttributes(t *testing.T) {
	ds := jitTestStore(t, true)
	ds.UserByEmailFunc = func(ctx context.Context, email string) (*fleet.User, error) {
		return &fleet.User{ID: 1, Email: email, SSOEnabled: true, GlobalRole: new(fleet.RoleAdmin)}, nil
	}
	svc, ctx := newTestService(t, ds, nil, nil)

	user, err := svc.GetSSOUser(ctx, stubAuth{email: "bob@example.com"})
	require.NoError(t, err)
	require.False(t, ds.SaveUserFuncInvoked)
	require.Equal(t, fleet.RoleAdmin, *user.GlobalRole)
}

func TestGetSSOUserSkipsSyncForUnchangedRoles(t *testing.T) {
	ds := jitTestStore(t, true)
	ds.UserByEmailFunc = func(ctx context.Context, email string) (*fleet.User, error) {
		return &fleet.User{ID: 1, Email: email, SSOEnabled: true, GlobalRole: new(fleet.RoleAdmin)}, nil
	}
	svc, ctx := newTestService(t, ds, nil, nil)

	_, err := svc.GetSSOUser(ctx, stubAuth{
		email:      "bob@example.com",
		attributes: []fleet.SAMLAttribute{roleAttribute("FLEET_JIT_USER_ROLE_GLOBAL", fleet.RoleAdmin)},
	})
	require.NoError(t, err)
	require.False(t, ds.SaveUserFuncInvoked)
}

// A local (non-SSO) account keeps its roles even when the IdP asserts others.
func TestGetSSOUserDoesNotSyncLocalAccounts(t *testing.T) {
	ds := jitTestStore(t, true)
	ds.UserByEmailFunc = func(ctx context.Context, email string) (*fleet.User, error) {
		return &fleet.User{ID: 1, Email: email, GlobalRole: new(fleet.RoleObserver)}, nil
	}
	svc, ctx := newTestService(t, ds, nil, nil)

	user, err := svc.GetSSOUser(ctx, stubAuth{
		email:      "bob@example.com",
		attributes: []fleet.SAMLAttribute{roleAttribute("FLEET_JIT_USER_ROLE_GLOBAL", fleet.RoleAdmin)},
	})
	require.NoError(t, err)
	require.False(t, ds.SaveUserFuncInvoked)
	require.Equal(t, fleet.RoleObserver, *user.GlobalRole)
}
