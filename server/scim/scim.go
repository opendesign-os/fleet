// Package scim serves the SCIM 2.0 provisioning API that identity providers
// (Entra ID, Okta, and others) call to keep Fleet's user directory in sync.
//
// The protocol itself is handled by github.com/elimity-com/scim; this package
// supplies the two resource handlers that map SCIM resources onto Fleet's
// scim_users and scim_groups tables, plus the authentication and request
// bookkeeping around them.
package scim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	escim "github.com/elimity-com/scim"
	"github.com/elimity-com/scim/optional"
	"github.com/elimity-com/scim/schema"

	"github.com/fleetdm/fleet/v4/server/config"
	"github.com/fleetdm/fleet/v4/server/contexts/viewer"
	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/fleetdm/fleet/v4/server/service/middleware/auth"
)

const (
	usersEndpoint  = "/Users"
	groupsEndpoint = "/Groups"

	// maxResults caps a single page. Entra ID asks for 100 at a time.
	maxResults = 100
)

// RegisterSCIM mounts the SCIM handler under both the versioned and the
// "latest" API prefixes.
func RegisterSCIM(
	mux *http.ServeMux,
	ds fleet.Datastore,
	svc fleet.Service,
	logger *slog.Logger,
	fleetConfig *config.FleetConfig,
) error {
	if fleetConfig == nil {
		return errors.New("fleet config is nil")
	}

	server, err := escim.NewServer(&escim.ServerArgs{
		ServiceProviderConfig: &escim.ServiceProviderConfig{
			MaxResults:       maxResults,
			SupportFiltering: true,
			SupportPatch:     true,
		},
		ResourceTypes: []escim.ResourceType{
			{
				ID:               optional.NewString("User"),
				Name:             "User",
				Endpoint:         usersEndpoint,
				Description:      optional.NewString("User Account"),
				Schema:           schema.CoreUserSchema(),
				SchemaExtensions: []escim.SchemaExtension{{Schema: schema.ExtensionEnterpriseUser()}},
				Handler:          newUserHandler(ds, logger),
			},
			{
				ID:          optional.NewString("Group"),
				Name:        "Group",
				Endpoint:    groupsEndpoint,
				Description: optional.NewString("Group"),
				Schema:      schema.CoreGroupSchema(),
				Handler:     newGroupHandler(ds, logger),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create SCIM server: %w", err)
	}

	for _, prefix := range []string{"/api/v1/fleet/scim", "/api/latest/fleet/scim"} {
		handler := http.StripPrefix(prefix, server)
		handler = recordLastRequest(ds, handler)
		handler = authenticate(svc, logger, handler)
		mux.Handle(prefix+"/", handler)
	}
	return nil
}

// authenticate resolves the bearer token to a Fleet user and requires the
// global admin role, matching the scim_user rule in policy.rego. The SCIM
// library owns its own routing, so this runs as plain HTTP middleware rather
// than through the endpoint authorization chain.
func authenticate(svc fleet.Service, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeSCIMError(w, http.StatusUnauthorized, "Authorization header required")
			return
		}

		v, err := auth.AuthViewer(r.Context(), token, svc)
		if err != nil {
			logger.DebugContext(r.Context(), "SCIM authentication failed", "err", err.Error())
			writeSCIMError(w, http.StatusUnauthorized, "invalid authentication token")
			return
		}
		if v.User == nil || v.User.GlobalRole == nil || *v.User.GlobalRole != fleet.RoleAdmin {
			writeSCIMError(w, http.StatusForbidden, "SCIM requires a global admin")
			return
		}

		next.ServeHTTP(w, r.WithContext(viewer.NewContext(r.Context(), *v)))
	})
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
}

// recordLastRequest stores the outcome of each SCIM request so the UI can show
// whether the IdP is reaching Fleet and what it got back.
func recordLastRequest(ds fleet.Datastore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		status := "success"
		if rec.status >= http.StatusBadRequest {
			status = "error"
		}
		details := fmt.Sprintf("%s %s: %d", r.Method, r.URL.Path, rec.status)
		// Bookkeeping must never fail the request the IdP just made.
		_ = ds.UpdateScimLastRequest(r.Context(), &fleet.ScimLastRequest{Status: status, Details: details})
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func writeSCIMError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(status)
	fmt.Fprintf(w,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:Error"],"detail":%q,"status":"%d"}`,
		detail, status)
}

// listOptions converts the library's paging parameters into the datastore's.
func listOptions(params escim.ListRequestParams) fleet.ScimListOptions {
	startIndex := params.StartIndex
	if startIndex < 1 {
		startIndex = 1
	}
	perPage := params.Count
	switch {
	case perPage < 0:
		perPage = 0
	case perPage == 0:
		perPage = maxResults
	case perPage > maxResults:
		perPage = maxResults
	}
	return fleet.ScimListOptions{
		StartIndex: uint(startIndex), //nolint:gosec // bounded above
		PerPage:    uint(perPage),    //nolint:gosec // bounded above
	}
}

// validateLength guards the varchar(255) columns so a long IdP value surfaces
// as a SCIM error instead of a MySQL truncation error.
func validateLength(field, value string) error {
	if len(value) > fleet.SCIMMaxFieldLength {
		return &fleet.SCIMValidationError{
			Field:   field,
			Message: fmt.Sprintf("exceeds maximum length of %d characters", fleet.SCIMMaxFieldLength),
		}
	}
	return nil
}

func logHandlerError(ctx context.Context, logger *slog.Logger, msg string, err error) {
	logger.ErrorContext(ctx, msg, "err", err.Error())
}

// isConflict reports whether the datastore rejected a write because the
// resource already exists, which SCIM answers with a uniqueness error.
func isConflict(err error) bool {
	var exists fleet.AlreadyExistsError
	return errors.As(err, &exists)
}
