package scim

import (
	"testing"

	escim "github.com/elimity-com/scim"
	"github.com/stretchr/testify/require"

	"github.com/fleetdm/fleet/v4/server/fleet"
)

func TestParseUserFilter(t *testing.T) {
	t.Run("blank filter matches everything", func(t *testing.T) {
		f, err := parseUserFilter("   ")
		require.NoError(t, err)
		require.Nil(t, f)
	})

	t.Run("by user name", func(t *testing.T) {
		f, err := parseUserFilter(`userName eq "bob@example.com"`)
		require.NoError(t, err)
		require.NotNil(t, f.userName)
		require.Equal(t, "bob@example.com", *f.userName)
		require.Nil(t, f.emailValue)
	})

	// Entra ID looks a user up by work email before provisioning them.
	t.Run("by work email", func(t *testing.T) {
		f, err := parseUserFilter(`emails[type eq "work"].value eq "bob@example.com"`)
		require.NoError(t, err)
		require.Equal(t, "work", *f.emailType)
		require.Equal(t, "bob@example.com", *f.emailValue)
		require.Nil(t, f.userName)
	})

	t.Run("unsupported filters are rejected", func(t *testing.T) {
		for _, raw := range []string{
			`displayName eq "bob"`,
			`userName sw "bob"`,
			`not a filter at all`,
		} {
			_, err := parseUserFilter(raw)
			require.Errorf(t, err, "filter %q", raw)
		}
	})
}

func TestParseGroupFilter(t *testing.T) {
	displayName, err := parseGroupFilter(`displayName eq "Engineering"`)
	require.NoError(t, err)
	require.Equal(t, "Engineering", *displayName)

	_, err = parseGroupFilter(`userName eq "bob"`)
	require.Error(t, err)
}

func TestUserFromAttributes(t *testing.T) {
	attributes := escim.ResourceAttributes{
		"userName":   "bob@example.com",
		"externalId": "ext-1",
		"active":     true,
		"name": map[string]interface{}{
			"givenName":  "Bob",
			"familyName": "Stone",
		},
		"emails": []interface{}{
			map[string]interface{}{"value": "bob@example.com", "type": "work", "primary": true},
			map[string]interface{}{"value": "", "type": "home"},
		},
		enterpriseUserURN: map[string]interface{}{"department": "IT"},
	}

	user, err := userFromAttributes(attributes, nil)
	require.NoError(t, err)
	require.Equal(t, "bob@example.com", user.UserName)
	require.Equal(t, "ext-1", *user.ExternalID)
	require.Equal(t, "Bob", *user.GivenName)
	require.Equal(t, "Stone", *user.FamilyName)
	require.Equal(t, "IT", *user.Department)
	require.True(t, *user.Active)
	// The blank address is dropped rather than stored as an empty email.
	require.Len(t, user.Emails, 1)
	require.Equal(t, "work", *user.Emails[0].Type)

	user, err = userFromAttributes(attributes, &fleet.ScimUser{ID: 42})
	require.NoError(t, err)
	require.Equal(t, uint(42), user.ID)
}

func TestUserFromAttributesRejectsBadInput(t *testing.T) {
	_, err := userFromAttributes(escim.ResourceAttributes{}, nil)
	require.Error(t, err)

	_, err = userFromAttributes(escim.ResourceAttributes{"userName": "   "}, nil)
	require.Error(t, err)

	tooLong := make([]byte, fleet.SCIMMaxFieldLength+1)
	for i := range tooLong {
		tooLong[i] = 'a'
	}
	_, err = userFromAttributes(escim.ResourceAttributes{"userName": string(tooLong)}, nil)
	require.Error(t, err)
}

func TestApplyUserAttribute(t *testing.T) {
	newUser := func() *fleet.ScimUser {
		return &fleet.ScimUser{
			UserName:   "bob@example.com",
			GivenName:  new("Bob"),
			Department: new("IT"),
			Active:     new(true),
		}
	}

	t.Run("deactivate", func(t *testing.T) {
		user := newUser()
		changed, err := applyUserAttribute(user, "active", "", false, "replace")
		require.NoError(t, err)
		require.True(t, changed)
		require.False(t, *user.Active)
	})

	t.Run("setting the same value is a no-op", func(t *testing.T) {
		user := newUser()
		changed, err := applyUserAttribute(user, "active", "", true, "replace")
		require.NoError(t, err)
		require.False(t, changed)
	})

	t.Run("remove clears the field", func(t *testing.T) {
		user := newUser()
		changed, err := applyUserAttribute(user, "department", "", nil, "remove")
		require.NoError(t, err)
		require.True(t, changed)
		require.Nil(t, user.Department)
	})

	t.Run("nested name sub-attribute", func(t *testing.T) {
		user := newUser()
		changed, err := applyUserAttribute(user, "name", "givenName", "Robert", "replace")
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "Robert", *user.GivenName)
	})

	// IdPs send attributes Fleet doesn't store; ignoring them keeps provisioning
	// moving instead of failing the whole request.
	t.Run("unknown attributes are ignored", func(t *testing.T) {
		user := newUser()
		changed, err := applyUserAttribute(user, "nickName", "", "Bobby", "replace")
		require.NoError(t, err)
		require.False(t, changed)
	})

	t.Run("wrong type is rejected", func(t *testing.T) {
		user := newUser()
		_, err := applyUserAttribute(user, "active", "", "yes", "replace")
		require.Error(t, err)
	})
}

func TestGroupFromAttributes(t *testing.T) {
	group, err := groupFromAttributes(escim.ResourceAttributes{
		"displayName": "Engineering",
		"externalId":  "grp-1",
		"members": []interface{}{
			map[string]interface{}{"value": "1", "type": "User"},
			map[string]interface{}{"value": "2"},
			map[string]interface{}{"value": "9", "type": "Group"},
		},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "Engineering", group.DisplayName)
	require.Equal(t, "grp-1", *group.ExternalID)
	// A member with no type defaults to a user.
	require.Equal(t, []uint{1, 2}, group.ScimUsers)
	require.Equal(t, []uint{9}, group.ChildGroups)

	_, err = groupFromAttributes(escim.ResourceAttributes{"displayName": " "}, nil)
	require.Error(t, err)

	_, err = groupFromAttributes(escim.ResourceAttributes{
		"displayName": "Engineering",
		"members":     []interface{}{map[string]interface{}{"value": "not-a-number"}},
	}, nil)
	require.Error(t, err)
}

func TestApplyMembersPatch(t *testing.T) {
	newGroup := func() *fleet.ScimGroup {
		return &fleet.ScimGroup{DisplayName: "Engineering", ScimUsers: []uint{1, 2}}
	}

	t.Run("add is idempotent", func(t *testing.T) {
		group := newGroup()
		changed, err := applyMembersPatch(group, []interface{}{
			map[string]interface{}{"value": "2"},
			map[string]interface{}{"value": "3"},
		}, "add")
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, []uint{1, 2, 3}, group.ScimUsers)

		changed, err = applyMembersPatch(group, []interface{}{map[string]interface{}{"value": "3"}}, "add")
		require.NoError(t, err)
		require.False(t, changed)
	})

	// Entra removes a single member as a bare object rather than a list.
	t.Run("remove one member sent as an object", func(t *testing.T) {
		group := newGroup()
		changed, err := applyMembersPatch(group, map[string]interface{}{"value": "1"}, "remove")
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, []uint{2}, group.ScimUsers)
	})

	t.Run("remove without a value clears membership", func(t *testing.T) {
		group := newGroup()
		changed, err := applyMembersPatch(group, nil, "remove")
		require.NoError(t, err)
		require.True(t, changed)
		require.Empty(t, group.ScimUsers)
	})

	t.Run("replace overwrites", func(t *testing.T) {
		group := newGroup()
		changed, err := applyMembersPatch(group, []interface{}{map[string]interface{}{"value": "7"}}, "replace")
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, []uint{7}, group.ScimUsers)
	})
}

func TestListOptions(t *testing.T) {
	opts := listOptions(escim.ListRequestParams{StartIndex: 0, Count: 0})
	require.Equal(t, uint(1), opts.StartIndex)
	require.Equal(t, uint(maxResults), opts.PerPage)

	opts = listOptions(escim.ListRequestParams{StartIndex: 5, Count: 10})
	require.Equal(t, uint(5), opts.StartIndex)
	require.Equal(t, uint(10), opts.PerPage)

	// A request for more than the page cap is clamped rather than honored.
	opts = listOptions(escim.ListRequestParams{StartIndex: 1, Count: 10_000})
	require.Equal(t, uint(maxResults), opts.PerPage)
}

func TestUserResourceRoundTrip(t *testing.T) {
	user := &fleet.ScimUser{
		ID:         7,
		UserName:   "bob@example.com",
		ExternalID: new("ext-1"),
		GivenName:  new("Bob"),
		FamilyName: new("Stone"),
		Department: new("IT"),
		Active:     new(true),
		Emails:     []fleet.ScimUserEmail{{Email: "bob@example.com", Type: new("work"), Primary: new(true)}},
		Groups:     []fleet.ScimUserGroup{{ID: 3, DisplayName: "Engineering"}},
	}

	resource := userResource(user)
	require.Equal(t, "7", resource.ID)
	require.Equal(t, "ext-1", resource.ExternalID.Value())
	require.Equal(t, "bob@example.com", resource.Attributes["userName"])
	require.Equal(t, "Bob Stone", resource.Attributes["displayName"])
	require.Len(t, resource.Attributes["emails"], 1)
	require.Len(t, resource.Attributes["groups"], 1)
	require.Equal(t,
		map[string]interface{}{"department": "IT"},
		resource.Attributes[enterpriseUserURN],
	)
}
