package scim

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	escim "github.com/elimity-com/scim"
	scimerrors "github.com/elimity-com/scim/errors"
	"github.com/elimity-com/scim/optional"

	"github.com/fleetdm/fleet/v4/server/fleet"
)

type groupHandler struct {
	ds     fleet.Datastore
	logger *slog.Logger
}

func newGroupHandler(ds fleet.Datastore, logger *slog.Logger) escim.ResourceHandler {
	return &groupHandler{ds: ds, logger: logger.With("component", "scim-groups")}
}

func (h *groupHandler) Create(r *http.Request, attributes escim.ResourceAttributes) (escim.Resource, error) {
	group, err := groupFromAttributes(attributes, nil)
	if err != nil {
		return escim.Resource{}, err
	}
	if err := h.verifyMembers(r.Context(), group); err != nil {
		return escim.Resource{}, err
	}

	id, err := h.ds.CreateScimGroup(r.Context(), group)
	if err != nil {
		if isConflict(err) {
			return escim.Resource{}, scimerrors.ScimErrorUniqueness
		}
		logHandlerError(r.Context(), h.logger, "create SCIM group", err)
		return escim.Resource{}, scimerrors.ScimErrorInternal
	}
	group.ID = id

	return groupResource(group), nil
}

func (h *groupHandler) Get(r *http.Request, id string) (escim.Resource, error) {
	group, err := h.groupByID(r, id)
	if err != nil {
		return escim.Resource{}, err
	}
	return groupResource(group), nil
}

func (h *groupHandler) GetAll(r *http.Request, params escim.ListRequestParams) (escim.Page, error) {
	opts := fleet.ScimGroupsListOptions{ScimListOptions: listOptions(params)}

	displayName, err := parseGroupFilter(r.URL.Query().Get("filter"))
	if err != nil {
		return escim.Page{}, err
	}
	opts.DisplayNameFilter = displayName

	groups, total, err := h.ds.ListScimGroups(r.Context(), opts)
	if err != nil {
		logHandlerError(r.Context(), h.logger, "list SCIM groups", err)
		return escim.Page{}, scimerrors.ScimErrorInternal
	}

	resources := make([]escim.Resource, 0, len(groups))
	for i := range groups {
		resources = append(resources, groupResource(&groups[i]))
	}
	return escim.Page{
		TotalResults: int(total), //nolint:gosec // row counts fit an int
		Resources:    resources,
	}, nil
}

func (h *groupHandler) Replace(r *http.Request, id string, attributes escim.ResourceAttributes) (escim.Resource, error) {
	existing, err := h.groupByID(r, id)
	if err != nil {
		return escim.Resource{}, err
	}

	group, err := groupFromAttributes(attributes, existing)
	if err != nil {
		return escim.Resource{}, err
	}
	if err := h.verifyMembers(r.Context(), group); err != nil {
		return escim.Resource{}, err
	}

	if err := h.ds.ReplaceScimGroup(r.Context(), group); err != nil {
		if isConflict(err) {
			return escim.Resource{}, scimerrors.ScimErrorUniqueness
		}
		logHandlerError(r.Context(), h.logger, "replace SCIM group", err)
		return escim.Resource{}, scimerrors.ScimErrorInternal
	}
	return groupResource(group), nil
}

func (h *groupHandler) Patch(r *http.Request, id string, operations []escim.PatchOperation) (escim.Resource, error) {
	group, err := h.groupByID(r, id)
	if err != nil {
		return escim.Resource{}, err
	}

	changed := false
	for _, op := range operations {
		applied, err := applyGroupPatch(group, op)
		if err != nil {
			return escim.Resource{}, err
		}
		changed = changed || applied
	}

	if !changed {
		return escim.Resource{}, nil
	}
	if err := h.verifyMembers(r.Context(), group); err != nil {
		return escim.Resource{}, err
	}

	if err := h.ds.ReplaceScimGroup(r.Context(), group); err != nil {
		logHandlerError(r.Context(), h.logger, "patch SCIM group", err)
		return escim.Resource{}, scimerrors.ScimErrorInternal
	}
	return groupResource(group), nil
}

func (h *groupHandler) Delete(r *http.Request, id string) error {
	groupID, err := parseResourceID(id)
	if err != nil {
		return scimerrors.ScimErrorResourceNotFound(id)
	}
	if err := h.ds.DeleteScimGroup(r.Context(), groupID); err != nil {
		if fleet.IsNotFound(err) {
			return scimerrors.ScimErrorResourceNotFound(id)
		}
		logHandlerError(r.Context(), h.logger, "delete SCIM group", err)
		return scimerrors.ScimErrorInternal
	}
	return nil
}

func (h *groupHandler) groupByID(r *http.Request, id string) (*fleet.ScimGroup, error) {
	groupID, err := parseResourceID(id)
	if err != nil {
		return nil, scimerrors.ScimErrorResourceNotFound(id)
	}
	group, err := h.ds.ScimGroupByID(r.Context(), groupID, false)
	if err != nil {
		if fleet.IsNotFound(err) {
			return nil, scimerrors.ScimErrorResourceNotFound(id)
		}
		logHandlerError(r.Context(), h.logger, "get SCIM group", err)
		return nil, scimerrors.ScimErrorInternal
	}
	return group, nil
}

// verifyMembers rejects membership referencing resources that don't exist, so a
// mistyped member id fails the request instead of creating a dangling row.
func (h *groupHandler) verifyMembers(ctx context.Context, group *fleet.ScimGroup) error {
	if len(group.ScimUsers) > 0 {
		exist, err := h.ds.ScimUsersExist(ctx, group.ScimUsers)
		if err != nil {
			logHandlerError(ctx, h.logger, "verify SCIM group users", err)
			return scimerrors.ScimErrorInternal
		}
		if !exist {
			return scimerrors.ScimErrorBadRequest("members reference a user that does not exist")
		}
	}
	if len(group.ChildGroups) > 0 {
		exist, err := h.ds.ScimGroupsExist(ctx, group.ChildGroups)
		if err != nil {
			logHandlerError(ctx, h.logger, "verify SCIM child groups", err)
			return scimerrors.ScimErrorInternal
		}
		if !exist {
			return scimerrors.ScimErrorBadRequest("members reference a group that does not exist")
		}
	}
	return nil
}

func groupFromAttributes(attributes escim.ResourceAttributes, existing *fleet.ScimGroup) (*fleet.ScimGroup, error) {
	displayName, _ := attributes["displayName"].(string)
	if strings.TrimSpace(displayName) == "" {
		return nil, scimerrors.ScimErrorBadParams([]string{"displayName"})
	}
	if err := validateLength("displayName", displayName); err != nil {
		return nil, scimerrors.ScimErrorBadRequest(err.Error())
	}

	group := &fleet.ScimGroup{DisplayName: displayName}
	if existing != nil {
		group.ID = existing.ID
	}

	if externalID, ok := attributes["externalId"].(string); ok && externalID != "" {
		if err := validateLength("externalId", externalID); err != nil {
			return nil, scimerrors.ScimErrorBadRequest(err.Error())
		}
		group.ExternalID = &externalID
	}

	users, childGroups, err := membersFromAttributes(attributes["members"])
	if err != nil {
		return nil, err
	}
	group.ScimUsers = users
	group.ChildGroups = childGroups

	return group, nil
}

// membersFromAttributes splits a SCIM member list into user and nested-group
// ids. Entra ID provisions nested groups as group-type members rather than
// flattening them, so both kinds arrive on the same list.
func membersFromAttributes(raw interface{}) (users []uint, childGroups []uint, err error) {
	values, ok := raw.([]interface{})
	if !ok {
		return nil, nil, nil
	}

	for _, value := range values {
		entry, ok := value.(map[string]interface{})
		if !ok {
			continue
		}
		id, ok := entry["value"].(string)
		if !ok || id == "" {
			continue
		}
		parsed, parseErr := parseResourceID(id)
		if parseErr != nil {
			return nil, nil, scimerrors.ScimErrorBadRequest("members contain an invalid id: " + id)
		}

		memberType, _ := entry["type"].(string)
		if strings.EqualFold(memberType, "Group") {
			childGroups = append(childGroups, parsed)
			continue
		}
		users = append(users, parsed)
	}
	return users, childGroups, nil
}

func applyGroupPatch(group *fleet.ScimGroup, op escim.PatchOperation) (bool, error) {
	attribute := ""
	if op.Path != nil {
		attribute = op.Path.AttributePath.AttributeName
	}

	if attribute == "" {
		values, ok := op.Value.(map[string]interface{})
		if !ok {
			return false, scimerrors.ScimErrorInvalidValue
		}
		changed := false
		for name, value := range values {
			applied, err := applyGroupAttribute(group, name, value, op.Op)
			if err != nil {
				return false, err
			}
			changed = changed || applied
		}
		return changed, nil
	}

	return applyGroupAttribute(group, attribute, op.Value, op.Op)
}

func applyGroupAttribute(group *fleet.ScimGroup, attribute string, value interface{}, op string) (bool, error) {
	switch strings.ToLower(attribute) {
	case "displayname":
		displayName, ok := value.(string)
		if !ok || displayName == "" {
			return false, scimerrors.ScimErrorInvalidValue
		}
		if group.DisplayName == displayName {
			return false, nil
		}
		group.DisplayName = displayName
		return true, nil

	case "externalid":
		if strings.EqualFold(op, "remove") {
			return setStringPtr(&group.ExternalID, nil), nil
		}
		externalID, ok := value.(string)
		if !ok {
			return false, scimerrors.ScimErrorInvalidValue
		}
		return setStringPtr(&group.ExternalID, &externalID), nil

	case "members":
		return applyMembersPatch(group, value, op)

	default:
		return false, nil
	}
}

func applyMembersPatch(group *fleet.ScimGroup, value interface{}, op string) (bool, error) {
	// A remove with no value clears the whole membership.
	if strings.EqualFold(op, "remove") && value == nil {
		if len(group.ScimUsers) == 0 && len(group.ChildGroups) == 0 {
			return false, nil
		}
		group.ScimUsers, group.ChildGroups = nil, nil
		return true, nil
	}

	users, childGroups, err := membersFromAttributes(normalizeMemberValue(value))
	if err != nil {
		return false, err
	}

	switch {
	case strings.EqualFold(op, "add"):
		before := len(group.ScimUsers) + len(group.ChildGroups)
		group.ScimUsers = union(group.ScimUsers, users)
		group.ChildGroups = union(group.ChildGroups, childGroups)
		return len(group.ScimUsers)+len(group.ChildGroups) != before, nil

	case strings.EqualFold(op, "remove"):
		before := len(group.ScimUsers) + len(group.ChildGroups)
		group.ScimUsers = difference(group.ScimUsers, users)
		group.ChildGroups = difference(group.ChildGroups, childGroups)
		return len(group.ScimUsers)+len(group.ChildGroups) != before, nil

	default: // replace
		group.ScimUsers, group.ChildGroups = users, childGroups
		return true, nil
	}
}

// normalizeMemberValue accepts both a member list and a single member object,
// since IdPs send either form for add and remove.
func normalizeMemberValue(value interface{}) interface{} {
	if _, ok := value.(map[string]interface{}); ok {
		return []interface{}{value}
	}
	return value
}

func union(existing, added []uint) []uint {
	seen := make(map[uint]struct{}, len(existing))
	for _, id := range existing {
		seen[id] = struct{}{}
	}
	for _, id := range added {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		existing = append(existing, id)
	}
	return existing
}

func difference(existing, removed []uint) []uint {
	if len(removed) == 0 {
		return existing
	}
	drop := make(map[uint]struct{}, len(removed))
	for _, id := range removed {
		drop[id] = struct{}{}
	}
	kept := make([]uint, 0, len(existing))
	for _, id := range existing {
		if _, ok := drop[id]; !ok {
			kept = append(kept, id)
		}
	}
	return kept
}

func groupResource(group *fleet.ScimGroup) escim.Resource {
	members := make([]interface{}, 0, len(group.ScimUsers)+len(group.ChildGroups))
	for _, id := range group.ScimUsers {
		members = append(members, map[string]interface{}{
			"value": strconv.FormatUint(uint64(id), 10),
			"type":  "User",
		})
	}
	for _, id := range group.ChildGroups {
		members = append(members, map[string]interface{}{
			"value": strconv.FormatUint(uint64(id), 10),
			"type":  "Group",
		})
	}

	attributes := escim.ResourceAttributes{
		"displayName": group.DisplayName,
		"members":     members,
	}

	resource := escim.Resource{
		ID:         strconv.FormatUint(uint64(group.ID), 10),
		Attributes: attributes,
	}
	if group.ExternalID != nil {
		resource.ExternalID = optional.NewString(*group.ExternalID)
	}
	return resource
}
