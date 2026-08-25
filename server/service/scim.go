package service

import (
	"context"

	"github.com/fleetdm/fleet/v4/server/contexts/ctxerr"
	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/fleetdm/fleet/v4/server/service/contract"
)

func getScimDetailsEndpoint(ctx context.Context, _ interface{}, svc fleet.Service) (fleet.Errorer, error) {
	details, err := svc.ScimDetails(ctx)
	if err != nil {
		return contract.ScimDetailsResponse{Err: err}, nil
	}
	return contract.ScimDetailsResponse{
		ScimDetails: details,
	}, nil
}

func (svc *Service) ScimDetails(ctx context.Context) (fleet.ScimDetails, error) {
	if err := svc.authz.Authorize(ctx, &fleet.ScimUser{}, fleet.ActionRead); err != nil {
		return fleet.ScimDetails{}, err
	}

	request, err := svc.ds.ScimLastRequest(ctx)
	if err != nil {
		return fleet.ScimDetails{}, ctxerr.Wrap(ctx, err, "get last SCIM request")
	}
	return fleet.ScimDetails{LastRequest: request}, nil
}
