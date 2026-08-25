package scim

import (
	"regexp"
	"strings"

	scimerrors "github.com/elimity-com/scim/errors"
	filter "github.com/scim2/filter-parser/v2"
)

// Identity providers only ever send Fleet a handful of filter shapes when they
// look a resource up before writing it. Rather than translate the whole SCIM
// filter grammar into SQL, the supported shapes are recognized here and mapped
// onto the datastore's dedicated filter options; anything else is rejected so
// the IdP sees a clear error instead of a silently unfiltered result set.

type userFilter struct {
	userName   *string
	emailType  *string
	emailValue *string
}

// emailFilterPattern matches the value-path filter Entra ID uses to find a user
// by work email. It is matched textually because the filter grammar in
// scim2/filter-parser rejects a value path carrying a trailing sub-attribute,
// which is exactly this shape.
var emailFilterPattern = regexp.MustCompile(
	`^(?i)emails\[\s*type\s+eq\s+"([^"]*)"\s*\]\.value\s+eq\s+"([^"]*)"$`)

// parseUserFilter recognizes `userName eq "..."` and Entra ID's
// `emails[type eq "work"].value eq "..."`. A blank filter returns nil.
func parseUserFilter(raw string) (*userFilter, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	if match := emailFilterPattern.FindStringSubmatch(raw); match != nil {
		return &userFilter{emailType: &match[1], emailValue: &match[2]}, nil
	}

	attrExp, err := parseEqualsFilter(raw)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(attrExp.AttributePath.AttributeName, "userName") ||
		attrExp.AttributePath.SubAttribute != nil {
		return nil, scimerrors.ScimErrorInvalidFilter
	}

	value, ok := attrExp.CompareValue.(string)
	if !ok {
		return nil, scimerrors.ScimErrorInvalidFilter
	}
	return &userFilter{userName: &value}, nil
}

// parseGroupFilter recognizes `displayName eq "..."`.
func parseGroupFilter(raw string) (*string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	attrExp, err := parseEqualsFilter(raw)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(attrExp.AttributePath.AttributeName, "displayName") {
		return nil, scimerrors.ScimErrorInvalidFilter
	}

	value, ok := attrExp.CompareValue.(string)
	if !ok {
		return nil, scimerrors.ScimErrorInvalidFilter
	}
	return &value, nil
}

func parseEqualsFilter(raw string) (*filter.AttributeExpression, error) {
	expression, err := filter.ParseFilter([]byte(raw))
	if err != nil {
		return nil, scimerrors.ScimErrorInvalidFilter
	}
	attrExp, ok := expression.(*filter.AttributeExpression)
	if !ok || !strings.EqualFold(string(attrExp.Operator), string(filter.EQ)) {
		return nil, scimerrors.ScimErrorInvalidFilter
	}
	return attrExp, nil
}
