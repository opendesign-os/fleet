package scim

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	escim "github.com/elimity-com/scim"
	scimerrors "github.com/elimity-com/scim/errors"
	"github.com/elimity-com/scim/optional"

	"github.com/fleetdm/fleet/v4/server/fleet"
)

// enterpriseUserURN is the schema extension Entra ID and Okta use to carry
// organizational attributes; Fleet only stores department from it.
const enterpriseUserURN = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"

type userHandler struct {
	ds     fleet.Datastore
	logger *slog.Logger
}

func newUserHandler(ds fleet.Datastore, logger *slog.Logger) escim.ResourceHandler {
	return &userHandler{ds: ds, logger: logger.With("component", "scim-users")}
}

func (h *userHandler) Create(r *http.Request, attributes escim.ResourceAttributes) (escim.Resource, error) {
	user, err := userFromAttributes(attributes, nil)
	if err != nil {
		return escim.Resource{}, err
	}

	id, err := h.ds.CreateScimUser(r.Context(), user)
	if err != nil {
		if isConflict(err) {
			return escim.Resource{}, scimerrors.ScimErrorUniqueness
		}
		logHandlerError(r.Context(), h.logger, "create SCIM user", err)
		return escim.Resource{}, scimerrors.ScimErrorInternal
	}
	user.ID = id

	return userResource(user), nil
}

func (h *userHandler) Get(r *http.Request, id string) (escim.Resource, error) {
	user, err := h.userByID(r, id)
	if err != nil {
		return escim.Resource{}, err
	}
	return userResource(user), nil
}

func (h *userHandler) GetAll(r *http.Request, params escim.ListRequestParams) (escim.Page, error) {
	opts := fleet.ScimUsersListOptions{ScimListOptions: listOptions(params)}

	switch f, err := parseUserFilter(r.URL.Query().Get("filter")); {
	case err != nil:
		return escim.Page{}, err
	case f != nil:
		opts.UserNameFilter = f.userName
		opts.EmailTypeFilter = f.emailType
		opts.EmailValueFilter = f.emailValue
	}

	users, total, err := h.ds.ListScimUsers(r.Context(), opts)
	if err != nil {
		logHandlerError(r.Context(), h.logger, "list SCIM users", err)
		return escim.Page{}, scimerrors.ScimErrorInternal
	}

	resources := make([]escim.Resource, 0, len(users))
	for i := range users {
		resources = append(resources, userResource(&users[i]))
	}
	return escim.Page{
		TotalResults: int(total), //nolint:gosec // row counts fit an int
		Resources:    resources,
	}, nil
}

func (h *userHandler) Replace(r *http.Request, id string, attributes escim.ResourceAttributes) (escim.Resource, error) {
	existing, err := h.userByID(r, id)
	if err != nil {
		return escim.Resource{}, err
	}

	user, err := userFromAttributes(attributes, existing)
	if err != nil {
		return escim.Resource{}, err
	}

	if _, err := h.ds.ReplaceScimUser(r.Context(), user); err != nil {
		if isConflict(err) {
			return escim.Resource{}, scimerrors.ScimErrorUniqueness
		}
		logHandlerError(r.Context(), h.logger, "replace SCIM user", err)
		return escim.Resource{}, scimerrors.ScimErrorInternal
	}
	return userResource(user), nil
}

func (h *userHandler) Patch(r *http.Request, id string, operations []escim.PatchOperation) (escim.Resource, error) {
	user, err := h.userByID(r, id)
	if err != nil {
		return escim.Resource{}, err
	}

	changed := false
	for _, op := range operations {
		applied, err := applyUserPatch(user, op)
		if err != nil {
			return escim.Resource{}, err
		}
		changed = changed || applied
	}

	// Returning no attributes tells the library to answer 204 No Content, which
	// RFC 7644 §3.5.2 allows when the operation was a no-op.
	if !changed {
		return escim.Resource{}, nil
	}

	if _, err := h.ds.ReplaceScimUser(r.Context(), user); err != nil {
		logHandlerError(r.Context(), h.logger, "patch SCIM user", err)
		return escim.Resource{}, scimerrors.ScimErrorInternal
	}
	return userResource(user), nil
}

func (h *userHandler) Delete(r *http.Request, id string) error {
	userID, err := parseResourceID(id)
	if err != nil {
		return scimerrors.ScimErrorResourceNotFound(id)
	}
	if _, err := h.ds.DeleteScimUser(r.Context(), userID); err != nil {
		if fleet.IsNotFound(err) {
			return scimerrors.ScimErrorResourceNotFound(id)
		}
		logHandlerError(r.Context(), h.logger, "delete SCIM user", err)
		return scimerrors.ScimErrorInternal
	}
	return nil
}

func (h *userHandler) userByID(r *http.Request, id string) (*fleet.ScimUser, error) {
	userID, err := parseResourceID(id)
	if err != nil {
		return nil, scimerrors.ScimErrorResourceNotFound(id)
	}
	user, err := h.ds.ScimUserByID(r.Context(), userID)
	if err != nil {
		if fleet.IsNotFound(err) {
			return nil, scimerrors.ScimErrorResourceNotFound(id)
		}
		logHandlerError(r.Context(), h.logger, "get SCIM user", err)
		return nil, scimerrors.ScimErrorInternal
	}
	return user, nil
}

// userFromAttributes maps a SCIM payload onto a Fleet user. When existing is
// non-nil its ID is carried over so the result can replace it.
func userFromAttributes(attributes escim.ResourceAttributes, existing *fleet.ScimUser) (*fleet.ScimUser, error) {
	userName, _ := attributes["userName"].(string)
	if strings.TrimSpace(userName) == "" {
		return nil, scimerrors.ScimErrorBadParams([]string{"userName"})
	}
	if err := validateLength("userName", userName); err != nil {
		return nil, scimerrors.ScimErrorBadRequest(err.Error())
	}

	user := &fleet.ScimUser{UserName: userName}
	if existing != nil {
		user.ID = existing.ID
	}

	if externalID, ok := attributes["externalId"].(string); ok && externalID != "" {
		if err := validateLength("externalId", externalID); err != nil {
			return nil, scimerrors.ScimErrorBadRequest(err.Error())
		}
		user.ExternalID = &externalID
	}

	if name, ok := attributes["name"].(map[string]interface{}); ok {
		if given, ok := name["givenName"].(string); ok && given != "" {
			if err := validateLength("givenName", given); err != nil {
				return nil, scimerrors.ScimErrorBadRequest(err.Error())
			}
			user.GivenName = &given
		}
		if family, ok := name["familyName"].(string); ok && family != "" {
			if err := validateLength("familyName", family); err != nil {
				return nil, scimerrors.ScimErrorBadRequest(err.Error())
			}
			user.FamilyName = &family
		}
	}

	if active, ok := attributes["active"].(bool); ok {
		user.Active = &active
	}

	if enterprise, ok := attributes[enterpriseUserURN].(map[string]interface{}); ok {
		if department, ok := enterprise["department"].(string); ok && department != "" {
			if err := validateLength("department", department); err != nil {
				return nil, scimerrors.ScimErrorBadRequest(err.Error())
			}
			user.Department = &department
		}
	}

	emails, err := emailsFromAttributes(attributes["emails"])
	if err != nil {
		return nil, err
	}
	user.Emails = emails

	return user, nil
}

func emailsFromAttributes(raw interface{}) ([]fleet.ScimUserEmail, error) {
	values, ok := raw.([]interface{})
	if !ok {
		return nil, nil
	}

	emails := make([]fleet.ScimUserEmail, 0, len(values))
	for _, value := range values {
		entry, ok := value.(map[string]interface{})
		if !ok {
			continue
		}
		address, _ := entry["value"].(string)
		if strings.TrimSpace(address) == "" {
			continue
		}
		if err := validateLength("emails.value", address); err != nil {
			return nil, scimerrors.ScimErrorBadRequest(err.Error())
		}

		email := fleet.ScimUserEmail{Email: address}
		if emailType, ok := entry["type"].(string); ok && emailType != "" {
			if err := validateLength("emails.type", emailType); err != nil {
				return nil, scimerrors.ScimErrorBadRequest(err.Error())
			}
			email.Type = &emailType
		}
		if primary, ok := entry["primary"].(bool); ok {
			email.Primary = &primary
		}
		emails = append(emails, email)
	}
	return emails, nil
}

// applyUserPatch mutates the user for one PATCH operation and reports whether
// anything actually changed.
func applyUserPatch(user *fleet.ScimUser, op escim.PatchOperation) (bool, error) {
	// A path-less add/replace carries a map of attributes to merge.
	if op.Path == nil {
		values, ok := op.Value.(map[string]interface{})
		if !ok {
			return false, scimerrors.ScimErrorInvalidValue
		}
		changed := false
		for attribute, value := range values {
			applied, err := applyUserAttribute(user, attribute, "", value, op.Op)
			if err != nil {
				return false, err
			}
			changed = changed || applied
		}
		return changed, nil
	}

	attribute := op.Path.AttributePath.AttributeName
	sub := op.Path.AttributePath.SubAttributeName()
	if sub == "" {
		sub = op.Path.SubAttributeName()
	}
	return applyUserAttribute(user, attribute, sub, op.Value, op.Op)
}

func applyUserAttribute(user *fleet.ScimUser, attribute, sub string, value interface{}, op string) (bool, error) {
	remove := strings.EqualFold(op, "remove")

	switch strings.ToLower(attribute) {
	case "username":
		newName, ok := value.(string)
		if !ok || newName == "" {
			return false, scimerrors.ScimErrorInvalidValue
		}
		if user.UserName == newName {
			return false, nil
		}
		user.UserName = newName
		return true, nil

	case "externalid":
		if remove {
			return setStringPtr(&user.ExternalID, nil), nil
		}
		newID, ok := value.(string)
		if !ok {
			return false, scimerrors.ScimErrorInvalidValue
		}
		return setStringPtr(&user.ExternalID, &newID), nil

	case "active":
		active, ok := value.(bool)
		if !ok {
			return false, scimerrors.ScimErrorInvalidValue
		}
		if user.Active != nil && *user.Active == active {
			return false, nil
		}
		user.Active = &active
		return true, nil

	case "name":
		return applyNamePatch(user, sub, value, remove)

	case "department":
		if remove {
			return setStringPtr(&user.Department, nil), nil
		}
		department, ok := value.(string)
		if !ok {
			return false, scimerrors.ScimErrorInvalidValue
		}
		return setStringPtr(&user.Department, &department), nil

	case "emails":
		if remove {
			if len(user.Emails) == 0 {
				return false, nil
			}
			user.Emails = nil
			return true, nil
		}
		emails, err := emailsFromAttributes(value)
		if err != nil {
			return false, err
		}
		if emails == nil {
			return false, scimerrors.ScimErrorInvalidValue
		}
		user.Emails = emails
		return true, nil

	case enterpriseUserURN:
		values, ok := value.(map[string]interface{})
		if !ok {
			// Entra sends the department as a fully qualified path rather than a
			// nested object, e.g. "<urn>:department".
			if sub != "" {
				return applyUserAttribute(user, sub, "", value, op)
			}
			return false, scimerrors.ScimErrorInvalidValue
		}
		changed := false
		for nested, nestedValue := range values {
			applied, err := applyUserAttribute(user, nested, "", nestedValue, op)
			if err != nil {
				return false, err
			}
			changed = changed || applied
		}
		return changed, nil

	default:
		// Unknown attributes are ignored rather than rejected: IdPs routinely
		// send attributes Fleet doesn't store, and failing the whole request
		// would stall provisioning.
		return false, nil
	}
}

func applyNamePatch(user *fleet.ScimUser, sub string, value interface{}, remove bool) (bool, error) {
	if sub == "" {
		values, ok := value.(map[string]interface{})
		if !ok {
			return false, scimerrors.ScimErrorInvalidValue
		}
		changed := false
		for nested, nestedValue := range values {
			applied, err := applyNamePatch(user, nested, nestedValue, remove)
			if err != nil {
				return false, err
			}
			changed = changed || applied
		}
		return changed, nil
	}

	var target **string
	switch strings.ToLower(sub) {
	case "givenname":
		target = &user.GivenName
	case "familyname":
		target = &user.FamilyName
	default:
		return false, nil
	}

	if remove {
		return setStringPtr(target, nil), nil
	}
	newValue, ok := value.(string)
	if !ok {
		return false, scimerrors.ScimErrorInvalidValue
	}
	return setStringPtr(target, &newValue), nil
}

func setStringPtr(target **string, value *string) bool {
	switch {
	case *target == nil && value == nil:
		return false
	case *target != nil && value != nil && **target == *value:
		return false
	}
	*target = value
	return true
}

func userResource(user *fleet.ScimUser) escim.Resource {
	attributes := escim.ResourceAttributes{
		"userName": user.UserName,
	}

	name := map[string]interface{}{}
	if user.GivenName != nil {
		name["givenName"] = *user.GivenName
	}
	if user.FamilyName != nil {
		name["familyName"] = *user.FamilyName
	}
	if len(name) > 0 {
		attributes["name"] = name
		attributes["displayName"] = user.DisplayName()
	}

	if user.Active != nil {
		attributes["active"] = *user.Active
	}
	if user.Department != nil {
		attributes[enterpriseUserURN] = map[string]interface{}{"department": *user.Department}
	}

	if len(user.Emails) > 0 {
		emails := make([]interface{}, 0, len(user.Emails))
		for _, email := range user.Emails {
			entry := map[string]interface{}{"value": email.Email}
			if email.Type != nil {
				entry["type"] = *email.Type
			}
			if email.Primary != nil {
				entry["primary"] = *email.Primary
			}
			emails = append(emails, entry)
		}
		attributes["emails"] = emails
	}

	if len(user.Groups) > 0 {
		groups := make([]interface{}, 0, len(user.Groups))
		for _, group := range user.Groups {
			groups = append(groups, map[string]interface{}{
				"value":   strconv.FormatUint(uint64(group.ID), 10),
				"display": group.DisplayName,
			})
		}
		attributes["groups"] = groups
	}

	resource := escim.Resource{
		ID:         strconv.FormatUint(uint64(user.ID), 10),
		Attributes: attributes,
	}
	if user.ExternalID != nil {
		resource.ExternalID = optional.NewString(*user.ExternalID)
	}
	if !user.UpdatedAt.IsZero() {
		updatedAt := user.UpdatedAt
		resource.Meta = escim.Meta{LastModified: &updatedAt}
	}
	return resource
}

func parseResourceID(id string) (uint, error) {
	parsed, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid resource id %q: %w", id, err)
	}
	return uint(parsed), nil
}
