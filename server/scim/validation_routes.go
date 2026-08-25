package scim

import (
	"net/http"

	kithttp "github.com/go-kit/kit/transport/http"
	"github.com/gorilla/mux"
)

// The SCIM handler is mounted on a path prefix and does its own routing, so
// gorilla/mux can't discover its endpoints the way it does for the rest of the
// API. RegisterValidationRoutes declares them explicitly for the startup check
// in server/api_endpoints that every documented endpoint is actually served.
func RegisterValidationRoutes(r *mux.Router, _ []kithttp.ServerOption) {
	const root = "/api/_version_/fleet/scim"

	for _, resource := range []string{usersEndpoint, groupsEndpoint} {
		r.Handle(root+resource, http.NotFoundHandler()).Methods(http.MethodGet)
		r.Handle(root+resource, http.NotFoundHandler()).Methods(http.MethodPost)
		for _, method := range []string{
			http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete,
		} {
			r.Handle(root+resource+"/{id}", http.NotFoundHandler()).Methods(method)
		}
	}

	for _, discovery := range []string{"/Schemas", "/ServiceProviderConfig", "/ResourceTypes"} {
		r.Handle(root+discovery, http.NotFoundHandler()).Methods(http.MethodGet)
	}
}
