package service

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"github.com/fleetdm/fleet/v4/pkg/optjson"
	"golang.org/x/text/unicode/norm"

	"github.com/fleetdm/fleet/v4/server"
	"github.com/fleetdm/fleet/v4/server/authz"
	"github.com/fleetdm/fleet/v4/server/contexts/ctxerr"
	"github.com/fleetdm/fleet/v4/server/contexts/viewer"
	"github.com/fleetdm/fleet/v4/server/fleet"
)

////////////////////////////////////////////////////////////////////////////////
// List Teams
////////////////////////////////////////////////////////////////////////////////

type listTeamsRequest struct {
	ListOptions fleet.ListOptions `url:"list_options"`
}

type listTeamsResponse struct {
	Teams []fleet.Team `json:"teams" renameto:"fleets"`
	Err   error        `json:"error,omitempty"`
}

func (r listTeamsResponse) Error() error { return r.Err }

func listTeamsEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*listTeamsRequest)
	teams, err := svc.ListTeams(ctx, req.ListOptions)
	if err != nil {
		return listTeamsResponse{Err: err}, nil
	}

	resp := listTeamsResponse{Teams: []fleet.Team{}}
	for _, team := range teams {
		resp.Teams = append(resp.Teams, *team)
	}
	return resp, nil
}

func (svc *Service) ListTeams(ctx context.Context, opt fleet.ListOptions) ([]*fleet.Team, error) {
	if err := svc.authz.Authorize(ctx, &fleet.Team{}, fleet.ActionRead); err != nil {
		return nil, err
	}

	vc, ok := viewer.FromContext(ctx)
	if !ok {
		return nil, fleet.ErrNoContext
	}

	teams, err := svc.ds.ListTeams(ctx, fleet.TeamFilter{User: vc.User, IncludeObserver: true}, opt)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "list fleets")
	}
	return teams, nil
}

////////////////////////////////////////////////////////////////////////////////
// Get Team
////////////////////////////////////////////////////////////////////////////////

type getTeamRequest struct {
	ID uint `url:"id"`
}

type getTeamResponse struct {
	Team *fleet.Team `json:"team" renameto:"fleet"`
	Err  error       `json:"error,omitempty"`
}

func (r getTeamResponse) Error() error { return r.Err }

type defaultTeamResponse struct {
	Team *fleet.DefaultTeam `json:"team" renameto:"fleet"`
	Err  error              `json:"error,omitempty"`
}

func (r defaultTeamResponse) Error() error { return r.Err }

func getTeamEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*getTeamRequest)

	team, err := svc.GetTeam(ctx, req.ID)
	if err != nil {
		return getTeamResponse{Err: err}, nil
	}

	// Special handling for team ID 0 - return DefaultTeam structure
	if team.ID == 0 {
		defaultTeam := &fleet.DefaultTeam{
			ID:   team.ID,
			Name: team.Name,
			DefaultTeamConfig: fleet.DefaultTeamConfig{
				WebhookSettings: fleet.DefaultTeamWebhookSettings{
					FailingPoliciesWebhook: team.Config.WebhookSettings.FailingPoliciesWebhook,
					HostActivitiesWebhook:  team.Config.WebhookSettings.HostActivitiesWebhook,
				},
				Integrations: fleet.DefaultTeamIntegrations{
					Jira:    team.Config.Integrations.Jira,
					Zendesk: team.Config.Integrations.Zendesk,
				},
			},
		}
		return defaultTeamResponse{Team: defaultTeam}, nil
	}

	return getTeamResponse{Team: team}, nil
}

func (svc *Service) GetTeam(ctx context.Context, tid uint) (*fleet.Team, error) {
	if err := svc.authz.Authorize(ctx, &fleet.Team{ID: tid}, fleet.ActionRead); err != nil {
		return nil, err
	}

	if tid == 0 {
		return svc.unassignedFleet(ctx)
	}

	team, err := svc.ds.TeamWithExtras(ctx, tid)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "get fleet")
	}
	return team, nil
}

// hostFeatures resolves the feature set that applies to a host: its fleet's
// config when it belongs to one, the global config otherwise. Like
// teamByIDOrName it runs no authorization of its own — it is an internal hook
// reached from already authorized callers, including the osquery flow where the
// caller is the host itself.
func (svc *Service) hostFeatures(ctx context.Context, host *fleet.Host) (*fleet.Features, error) {
	if host != nil && host.TeamID != nil {
		features, err := svc.ds.TeamFeatures(ctx, *host.TeamID)
		if err != nil {
			return nil, ctxerr.Wrap(ctx, err, "get fleet features for host")
		}
		return features, nil
	}

	appConfig, err := svc.ds.AppConfig(ctx)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "get app config for host features")
	}
	return &appConfig.Features, nil
}

// resolveTeam looks up a fleet for callers that need its name in activity
// details. The enterprise service installs a richer lookup when present;
// otherwise the core implementation reads the datastore directly.
func (svc *Service) resolveTeam(ctx context.Context, id *uint, name *string) (*fleet.Team, error) {
	if svc.EnterpriseOverrides != nil && svc.EnterpriseOverrides.TeamByIDOrName != nil {
		return svc.EnterpriseOverrides.TeamByIDOrName(ctx, id, name)
	}
	return svc.teamByIDOrName(ctx, id, name)
}

// teamByIDOrName resolves a fleet for internal callers that need its name, for
// activity details and MDM scoping. It runs no authorization of its own: it is
// wired into EnterpriseOverrides and only ever reached from an already
// authorized caller.
func (svc *Service) teamByIDOrName(ctx context.Context, id *uint, name *string) (*fleet.Team, error) {
	switch {
	case id != nil:
		if *id == 0 {
			return svc.unassignedFleet(ctx)
		}
		return svc.ds.TeamWithExtras(ctx, *id)
	case name != nil:
		return svc.ds.TeamByName(ctx, *name)
	default:
		return nil, ctxerr.Wrap(ctx, fleet.NewInvalidArgumentError("fleet", "either id or name must be provided"))
	}
}

// unassignedFleet builds the synthetic fleet that stands for hosts with no
// fleet assignment. It has no `teams` row, so its config lives in the separate
// default-config record.
func (svc *Service) unassignedFleet(ctx context.Context) (*fleet.Team, error) {
	config, err := svc.ds.DefaultTeamConfig(ctx)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "get default fleet config")
	}
	return &fleet.Team{ID: 0, Name: fleet.ReservedNameNoTeam, Config: *config}, nil
}

////////////////////////////////////////////////////////////////////////////////
// Create Team
////////////////////////////////////////////////////////////////////////////////

type createTeamRequest struct {
	fleet.TeamPayload
}

type teamResponse struct {
	Team *fleet.Team `json:"team,omitempty" renameto:"fleet"`
	Err  error       `json:"error,omitempty"`
}

func (r teamResponse) Error() error { return r.Err }

func createTeamEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*createTeamRequest)

	team, err := svc.NewTeam(ctx, req.TeamPayload)
	if err != nil {
		return teamResponse{Err: err}, nil
	}
	return teamResponse{Team: team}, nil
}

func (svc *Service) NewTeam(ctx context.Context, p fleet.TeamPayload) (*fleet.Team, error) {
	if err := svc.authz.Authorize(ctx, &fleet.Team{}, fleet.ActionWrite); err != nil {
		return nil, err
	}

	if p.MDM != nil {
		return nil, ctxerr.Wrap(ctx, errPerFleetMDMUnsupported())
	}

	if p.Name == nil {
		return nil, ctxerr.Wrap(ctx, fleet.NewInvalidArgumentError("name", "missing required argument"))
	}

	team := &fleet.Team{Name: norm.NFC.String(*p.Name)}
	if err := svc.verifyFleetName(ctx, team.Name, 0); err != nil {
		return nil, ctxerr.Wrap(ctx, err)
	}

	if p.Description != nil {
		team.Description = *p.Description
	}

	secrets, err := resolveNewFleetSecrets(p.Secrets)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err)
	}
	team.Secrets = secrets

	applyFleetConfigPayload(&team.Config, p)

	team, err = svc.ds.NewTeam(ctx, team)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "create fleet")
	}

	if err := svc.NewActivity(
		ctx,
		authz.UserFromContext(ctx),
		fleet.ActivityTypeCreatedTeam{ID: team.ID, Name: team.Name},
	); err != nil {
		return nil, ctxerr.Wrap(ctx, err, "create activity for fleet creation")
	}
	return team, nil
}

func errPerFleetMDMUnsupported() error {
	return fleet.NewInvalidArgumentError("mdm",
		"per-fleet MDM settings are not supported by this endpoint; manage them through the MDM endpoints")
}

// verifyFleetName rejects empty, over-long, reserved and already-taken names.
// excludeID is the fleet being renamed, so an unchanged name isn't reported as
// a conflict with itself.
func (svc *Service) verifyFleetName(ctx context.Context, name string, excludeID uint) error {
	if strings.TrimSpace(name) == "" {
		return fleet.NewInvalidArgumentError("name", "may not be empty")
	}
	if len(name) > fleet.MaxTeamNameLength {
		return fleet.NewInvalidArgumentError("name",
			fmt.Sprintf("may not exceed %d characters", fleet.MaxTeamNameLength))
	}
	if fleet.IsReservedTeamName(name) {
		return fleet.NewInvalidArgumentError("name", fmt.Sprintf("%q is a reserved name", name))
	}

	conflict, err := svc.ds.TeamConflictsWithName(ctx, name, excludeID)
	if err != nil {
		return ctxerr.Wrap(ctx, err, "check fleet name conflict")
	}
	if conflict != nil {
		return &fleet.ConflictError{Message: fmt.Sprintf("a fleet named %q already exists", name)}
	}
	return nil
}

// resolveNewFleetSecrets returns the enroll secrets a new fleet starts with,
// generating one when the caller didn't supply any — a fleet with no secret
// can't enroll hosts.
func resolveNewFleetSecrets(provided []*fleet.EnrollSecret) ([]*fleet.EnrollSecret, error) {
	if len(provided) == 0 {
		secret, err := server.GenerateRandomText(fleet.EnrollSecretDefaultLength)
		if err != nil {
			return nil, fmt.Errorf("generate enroll secret: %w", err)
		}
		return []*fleet.EnrollSecret{{Secret: secret}}, nil
	}
	if err := validateFleetSecrets(provided); err != nil {
		return nil, err
	}
	return provided, nil
}

func validateFleetSecrets(secrets []*fleet.EnrollSecret) error {
	if len(secrets) > fleet.MaxEnrollSecretsCount {
		return fleet.NewInvalidArgumentError("secrets",
			fmt.Sprintf("may not exceed %d secrets", fleet.MaxEnrollSecretsCount))
	}
	for _, s := range secrets {
		if s == nil || strings.TrimSpace(s.Secret) == "" {
			return fleet.NewInvalidArgumentError("secrets", "enroll secret must not be empty")
		}
	}
	return nil
}

// applyFleetConfigPayload merges the tenancy-scoped settings of a PATCH body
// into a fleet's config. Absent keys keep their stored value.
func applyFleetConfigPayload(config *fleet.TeamConfig, p fleet.TeamPayload) {
	if p.WebhookSettings != nil {
		config.WebhookSettings = *p.WebhookSettings
	}
	if p.Integrations != nil {
		config.Integrations = *p.Integrations
	}
	if p.HostExpirySettings != nil {
		config.HostExpirySettings = *p.HostExpirySettings
	}
	if p.Features != nil {
		applyFleetFeaturesPayload(&config.Features, *p.Features)
	}
}

func applyFleetFeaturesPayload(features *fleet.Features, p fleet.TeamPayloadFeatures) {
	if p.EnableSoftwareInventory.Valid {
		features.EnableSoftwareInventory = p.EnableSoftwareInventory.Value
	}
	if p.HistoricalData == nil {
		return
	}
	if p.HistoricalData.Uptime.Valid {
		features.HistoricalData.Uptime = p.HistoricalData.Uptime.Value
	}
	if p.HistoricalData.Vulnerabilities.Valid {
		features.HistoricalData.Vulnerabilities = p.HistoricalData.Vulnerabilities.Value
	}
}

////////////////////////////////////////////////////////////////////////////////
// Modify Team
////////////////////////////////////////////////////////////////////////////////

type modifyTeamRequest struct {
	ID uint `json:"-" url:"id"`
	fleet.TeamPayload
}

func modifyTeamEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*modifyTeamRequest)

	// AppleOSUpdateSettings.UpdateNewHosts is only used in macOS ... so ignore any values sent for iOS/iPadOS
	if req.TeamPayload.MDM != nil {
		if req.TeamPayload.MDM.IOSUpdates != nil {
			req.TeamPayload.MDM.IOSUpdates.UpdateNewHosts = optjson.Bool{}
		}
		if req.TeamPayload.MDM.IPadOSUpdates != nil {
			req.TeamPayload.MDM.IPadOSUpdates.UpdateNewHosts = optjson.Bool{}
		}
	}

	team, err := svc.ModifyTeam(ctx, req.ID, req.TeamPayload)
	if err != nil {
		return teamResponse{Err: err}, nil
	}

	// Special handling for team ID 0 - return limited fields
	if req.ID == 0 {
		// Convert to DefaultTeam with limited fields
		defaultTeam := &fleet.DefaultTeam{
			ID:   team.ID,
			Name: team.Name,
			DefaultTeamConfig: fleet.DefaultTeamConfig{
				WebhookSettings: fleet.DefaultTeamWebhookSettings{
					FailingPoliciesWebhook: team.Config.WebhookSettings.FailingPoliciesWebhook,
					HostActivitiesWebhook:  team.Config.WebhookSettings.HostActivitiesWebhook,
				},
				Integrations: fleet.DefaultTeamIntegrations{
					Jira:    team.Config.Integrations.Jira,
					Zendesk: team.Config.Integrations.Zendesk,
				},
			},
		}
		return defaultTeamResponse{Team: defaultTeam}, nil
	}

	return teamResponse{Team: team}, err
}

func (svc *Service) ModifyTeam(ctx context.Context, id uint, payload fleet.TeamPayload) (*fleet.Team, error) {
	if err := svc.authz.Authorize(ctx, &fleet.Team{ID: id}, fleet.ActionWrite); err != nil {
		return nil, err
	}

	if payload.MDM != nil {
		return nil, ctxerr.Wrap(ctx, errPerFleetMDMUnsupported())
	}

	if id == 0 {
		return svc.modifyUnassignedFleet(ctx, payload)
	}

	team, err := svc.ds.TeamWithExtras(ctx, id)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "get fleet")
	}

	if payload.Name != nil {
		name := norm.NFC.String(*payload.Name)
		if err := svc.verifyFleetName(ctx, name, id); err != nil {
			return nil, ctxerr.Wrap(ctx, err)
		}
		team.Name = name
	}
	if payload.Description != nil {
		team.Description = *payload.Description
	}
	applyFleetConfigPayload(&team.Config, payload)

	// SaveTeam persists the row and its user roles but not enroll secrets, so
	// those go through their own datastore call.
	if payload.Secrets != nil {
		if err := validateFleetSecrets(payload.Secrets); err != nil {
			return nil, ctxerr.Wrap(ctx, err)
		}
		if err := svc.ds.ApplyEnrollSecrets(ctx, &id, payload.Secrets); err != nil {
			return nil, ctxerr.Wrap(ctx, err, "apply fleet enroll secrets")
		}
		team.Secrets = payload.Secrets
	}

	team, err = svc.ds.SaveTeam(ctx, team)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "save fleet")
	}
	return team, nil
}

// modifyUnassignedFleet updates the config of the synthetic fleet 0. Only the
// settings DefaultTeamConfig carries apply — it has no name, description or
// enroll secrets of its own.
func (svc *Service) modifyUnassignedFleet(ctx context.Context, payload fleet.TeamPayload) (*fleet.Team, error) {
	if payload.Name != nil || payload.Description != nil || payload.Secrets != nil {
		return nil, ctxerr.Wrap(ctx, fleet.NewInvalidArgumentError("fleet_id",
			"the unassigned fleet has no name, description or enroll secrets"))
	}

	team, err := svc.unassignedFleet(ctx)
	if err != nil {
		return nil, err
	}
	applyFleetConfigPayload(&team.Config, payload)

	if err := svc.ds.SaveDefaultTeamConfig(ctx, &team.Config); err != nil {
		return nil, ctxerr.Wrap(ctx, err, "save default fleet config")
	}
	return team, nil
}

////////////////////////////////////////////////////////////////////////////////
// Delete Team
////////////////////////////////////////////////////////////////////////////////

type deleteTeamRequest struct {
	ID uint `url:"id"`
}

type deleteTeamResponse struct {
	Err error `json:"error,omitempty"`
}

func (r deleteTeamResponse) Error() error { return r.Err }

func deleteTeamEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*deleteTeamRequest)
	err := svc.DeleteTeam(ctx, req.ID)
	if err != nil {
		return deleteTeamResponse{Err: err}, nil
	}
	return deleteTeamResponse{}, nil
}

func (svc *Service) DeleteTeam(ctx context.Context, tid uint) error {
	if err := svc.authz.Authorize(ctx, &fleet.Team{ID: tid}, fleet.ActionWrite); err != nil {
		return err
	}

	if tid == 0 {
		return ctxerr.Wrap(ctx, fleet.NewInvalidArgumentError("fleet_id", "the unassigned fleet cannot be deleted"))
	}

	team, err := svc.ds.TeamLite(ctx, tid)
	if err != nil {
		return ctxerr.Wrap(ctx, err, "get fleet")
	}

	if err := svc.ds.DeleteTeam(ctx, tid); err != nil {
		return ctxerr.Wrap(ctx, err, "delete fleet")
	}

	if err := svc.NewActivity(
		ctx,
		authz.UserFromContext(ctx),
		fleet.ActivityTypeDeletedTeam{ID: tid, Name: team.Name},
	); err != nil {
		return ctxerr.Wrap(ctx, err, "create activity for fleet deletion")
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////
// Apply Team Specs
////////////////////////////////////////////////////////////////////////////////

type applyTeamSpecsRequest struct {
	Force             bool                              `json:"-" query:"force,optional"`   // if true, bypass strict incoming json validation
	DryRun            bool                              `json:"-" query:"dry_run,optional"` // if true, apply validation but do not save changes
	DryRunAssumptions *fleet.TeamSpecsDryRunAssumptions `json:"dry_run_assumptions,omitempty"`
	Specs             []*fleet.TeamSpec                 `json:"specs"`
}

func (req *applyTeamSpecsRequest) DecodeBody(ctx context.Context, r io.Reader, u url.Values, c []*x509.Certificate) error {
	if err := fleet.JSONStrictDecode(r, req); err != nil {
		err = fleet.NewUserMessageError(err, http.StatusBadRequest)
		if !req.Force || !fleet.IsJSONUnknownFieldError(err) {
			// only unknown field errors can be forced at this point (other errors
			// can be forced later, after agent options' validations)
			return ctxerr.Wrap(ctx, err, "strict decode team specs")
		}
	}

	// the MacOSSettings field must be validated separately, since it
	// JSON-decodes into a free-form map.
	for _, spec := range req.Specs {
		if spec == nil || spec.MDM.MacOSSettings == nil {
			continue
		}

		var macOSSettings fleet.MacOSSettings
		validMap := macOSSettings.ToMap()

		// the keys provided must be valid
		for k := range spec.MDM.MacOSSettings {
			if _, ok := validMap[k]; !ok {
				return ctxerr.Wrap(ctx, fleet.NewUserMessageError(
					fmt.Errorf("json: unknown field %q", k),
					http.StatusBadRequest), "strict decode team specs")
			}
		}
	}
	return nil
}

type applyTeamSpecsResponse struct {
	Err           error           `json:"error,omitempty"`
	TeamIDsByName map[string]uint `json:"team_ids_by_name,omitempty" renameto:"fleet_ids_by_name"`
}

func (r applyTeamSpecsResponse) Error() error { return r.Err }

func applyTeamSpecsEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*applyTeamSpecsRequest)
	if !req.DryRun {
		req.DryRunAssumptions = nil
	}

	// remove any nil spec (may happen in conversion from YAML to JSON with fleetctl, but also
	// with the API should someone send such JSON)
	actualSpecs := make([]*fleet.TeamSpec, 0, len(req.Specs))
	for _, spec := range req.Specs {
		if spec != nil {
			// Normalize the team name for full Unicode support to prevent potential issue further in the spec flow
			spec.Name = norm.NFC.String(spec.Name)
			actualSpecs = append(actualSpecs, spec)
		}
	}

	idsByName, err := svc.ApplyTeamSpecs(
		ctx, actualSpecs, fleet.ApplyTeamSpecOptions{
			ApplySpecOptions: fleet.ApplySpecOptions{
				Force:  req.Force,
				DryRun: req.DryRun,
			},
			DryRunAssumptions: req.DryRunAssumptions,
		})
	if err != nil {
		return applyTeamSpecsResponse{Err: err}, nil
	}
	return applyTeamSpecsResponse{TeamIDsByName: idsByName}, nil
}

func (svc Service) ApplyTeamSpecs(ctx context.Context, specs []*fleet.TeamSpec, opts fleet.ApplyTeamSpecOptions) (map[string]uint, error) {
	if err := svc.authz.Authorize(ctx, &fleet.Team{}, fleet.ActionWrite); err != nil {
		return nil, err
	}

	// Validate the whole batch before writing anything, so a bad spec late in
	// the list can't leave earlier fleets half-applied.
	for _, spec := range specs {
		if strings.TrimSpace(spec.Name) == "" {
			return nil, ctxerr.Wrap(ctx, fleet.NewInvalidArgumentError("name", "fleet name may not be empty"))
		}
		if len(spec.Name) > fleet.MaxTeamNameLength {
			return nil, ctxerr.Wrap(ctx, fleet.NewInvalidArgumentError("name",
				fmt.Sprintf("fleet name may not exceed %d characters", fleet.MaxTeamNameLength)))
		}
		if fleet.IsReservedTeamName(spec.Name) {
			return nil, ctxerr.Wrap(ctx, fleet.NewInvalidArgumentError("name",
				fmt.Sprintf("%q is a reserved name", spec.Name)))
		}
		if !reflect.DeepEqual(spec.MDM, fleet.TeamSpecMDM{}) {
			return nil, ctxerr.Wrap(ctx, errPerFleetMDMUnsupported())
		}
		if spec.Secrets != nil {
			secrets := make([]*fleet.EnrollSecret, len(*spec.Secrets))
			for i := range *spec.Secrets {
				secrets[i] = &(*spec.Secrets)[i]
			}
			if err := validateFleetSecrets(secrets); err != nil {
				return nil, ctxerr.Wrap(ctx, err)
			}
		}
		if len(spec.AgentOptions) > 0 && !bytes.Equal(spec.AgentOptions, []byte("null")) {
			if err := fleet.ValidateJSONAgentOptions(ctx, svc.ds, spec.AgentOptions, true, 0); err != nil && !opts.Force {
				return nil, ctxerr.Wrap(ctx, fleet.NewUserMessageError(err, http.StatusBadRequest))
			}
		}
	}

	idsByName := make(map[string]uint, len(specs))
	applied := make([]fleet.TeamActivityDetail, 0, len(specs))

	for _, spec := range specs {
		team, created, err := svc.applyTeamSpec(ctx, spec, opts.DryRun)
		if err != nil {
			return nil, ctxerr.Wrapf(ctx, err, "apply spec for fleet %q", spec.Name)
		}
		idsByName[spec.Name] = team.ID
		if !created {
			applied = append(applied, fleet.TeamActivityDetail{ID: team.ID, Name: team.Name})
		}
	}

	if opts.DryRun {
		return idsByName, nil
	}

	if len(applied) > 0 {
		if err := svc.NewActivity(
			ctx,
			authz.UserFromContext(ctx),
			fleet.ActivityTypeAppliedSpecTeam{Teams: applied},
		); err != nil {
			return nil, ctxerr.Wrap(ctx, err, "create activity for applied fleet specs")
		}
	}
	return idsByName, nil
}

// applyTeamSpec upserts a single fleet from its spec. It reports whether the
// fleet was created, since creation emits its own activity and is therefore
// left out of the applied-spec activity.
func (svc Service) applyTeamSpec(ctx context.Context, spec *fleet.TeamSpec, dryRun bool) (*fleet.Team, bool, error) {
	team, err := svc.ds.TeamByName(ctx, spec.Name)
	switch {
	case err == nil:
	case fleet.IsNotFound(err):
		team = nil
	default:
		return nil, false, ctxerr.Wrap(ctx, err, "look up fleet by name")
	}

	created := team == nil
	if created {
		team = &fleet.Team{Name: norm.NFC.String(spec.Name)}
		if spec.Secrets == nil {
			secrets, err := resolveNewFleetSecrets(nil)
			if err != nil {
				return nil, false, ctxerr.Wrap(ctx, err)
			}
			team.Secrets = secrets
		}
	}

	if spec.Filename != nil {
		team.Filename = spec.Filename
	}
	applyTeamSpecConfig(&team.Config, spec)

	if spec.Features != nil {
		var features fleet.Features
		if err := json.Unmarshal(*spec.Features, &features); err != nil {
			return nil, false, ctxerr.Wrap(ctx, fleet.NewInvalidArgumentError("features", err.Error()))
		}
		team.Config.Features = features
	}

	var specSecrets []*fleet.EnrollSecret
	if spec.Secrets != nil {
		specSecrets = make([]*fleet.EnrollSecret, len(*spec.Secrets))
		for i := range *spec.Secrets {
			specSecrets[i] = &(*spec.Secrets)[i]
		}
		team.Secrets = specSecrets
	}

	if dryRun {
		return team, created, nil
	}

	if created {
		team, err = svc.ds.NewTeam(ctx, team)
		if err != nil {
			return nil, false, ctxerr.Wrap(ctx, err, "create fleet")
		}
		if err := svc.NewActivity(
			ctx,
			authz.UserFromContext(ctx),
			fleet.ActivityTypeCreatedTeam{ID: team.ID, Name: team.Name},
		); err != nil {
			return nil, false, ctxerr.Wrap(ctx, err, "create activity for fleet creation")
		}
		return team, true, nil
	}

	// NewTeam writes the secrets it is given; SaveTeam does not, so an update
	// has to apply them separately.
	if specSecrets != nil {
		if err := svc.ds.ApplyEnrollSecrets(ctx, &team.ID, specSecrets); err != nil {
			return nil, false, ctxerr.Wrap(ctx, err, "apply fleet enroll secrets")
		}
	}

	team, err = svc.ds.SaveTeam(ctx, team)
	if err != nil {
		return nil, false, ctxerr.Wrap(ctx, err, "save fleet")
	}
	return team, false, nil
}

// applyTeamSpecConfig merges the spec's settings into a fleet config. An absent
// key leaves the stored value alone; a present-but-empty one clears it.
func applyTeamSpecConfig(config *fleet.TeamConfig, spec *fleet.TeamSpec) {
	if spec.AgentOptions != nil {
		if bytes.Equal(spec.AgentOptions, []byte("null")) {
			config.AgentOptions = nil
		} else {
			opts := spec.AgentOptions
			config.AgentOptions = &opts
		}
	}
	if spec.HostExpirySettings != nil {
		config.HostExpirySettings = *spec.HostExpirySettings
	}
	if spec.WebhookSettings.HostStatusWebhook != nil {
		config.WebhookSettings.HostStatusWebhook = spec.WebhookSettings.HostStatusWebhook
	}
	if spec.WebhookSettings.FailingPoliciesWebhook != nil {
		config.WebhookSettings.FailingPoliciesWebhook = *spec.WebhookSettings.FailingPoliciesWebhook
	}
	if spec.WebhookSettings.HostActivitiesWebhook != nil {
		config.WebhookSettings.HostActivitiesWebhook = spec.WebhookSettings.HostActivitiesWebhook
	}
	if spec.Integrations.GoogleCalendar != nil {
		config.Integrations.GoogleCalendar = spec.Integrations.GoogleCalendar
	}
	if spec.Integrations.ConditionalAccessEnabled != nil {
		config.Integrations.ConditionalAccessEnabled = optjson.SetBool(*spec.Integrations.ConditionalAccessEnabled)
	}
	if spec.Scripts.Set {
		config.Scripts = spec.Scripts
	}
	if spec.Software != nil {
		config.Software = spec.Software
	}
}

////////////////////////////////////////////////////////////////////////////////
// Modify Team Agent Options
////////////////////////////////////////////////////////////////////////////////

type modifyTeamAgentOptionsRequest struct {
	ID     uint `json:"-" url:"id"`
	Force  bool `json:"-" query:"force,optional"`   // if true, bypass strict incoming json validation
	DryRun bool `json:"-" query:"dry_run,optional"` // if true, apply validation but do not save changes
	json.RawMessage
}

func modifyTeamAgentOptionsEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*modifyTeamAgentOptionsRequest)
	team, err := svc.ModifyTeamAgentOptions(ctx, req.ID, req.RawMessage, fleet.ApplySpecOptions{
		Force:  req.Force,
		DryRun: req.DryRun,
	})
	if err != nil {
		return teamResponse{Err: err}, nil
	}
	return teamResponse{Team: team}, err
}

func (svc *Service) ModifyTeamAgentOptions(ctx context.Context, id uint, teamOptions json.RawMessage, applyOptions fleet.ApplySpecOptions) (*fleet.Team, error) {
	if err := svc.authz.Authorize(ctx, &fleet.Team{ID: id}, fleet.ActionWrite); err != nil {
		return nil, err
	}

	if id == 0 {
		return nil, ctxerr.Wrap(ctx, fleet.NewInvalidArgumentError("fleet_id",
			"the unassigned fleet uses the global agent options"))
	}

	team, err := svc.ds.TeamWithExtras(ctx, id)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "get fleet")
	}

	if len(teamOptions) > 0 {
		if err := fleet.ValidateJSONAgentOptions(ctx, svc.ds, teamOptions, true, id); err != nil {
			// Force lets an operator save options this version doesn't recognize
			// yet, e.g. when rolling out an agent ahead of the server.
			if !applyOptions.Force {
				return nil, ctxerr.Wrap(ctx, fleet.NewUserMessageError(err, http.StatusBadRequest))
			}
		}
	}

	if applyOptions.DryRun {
		return team, nil
	}

	if len(teamOptions) > 0 {
		team.Config.AgentOptions = &teamOptions
	} else {
		team.Config.AgentOptions = nil
	}

	team, err = svc.ds.SaveTeam(ctx, team)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "save fleet agent options")
	}

	if err := svc.NewActivity(
		ctx,
		authz.UserFromContext(ctx),
		fleet.ActivityTypeEditedAgentOptions{TeamID: &team.ID, TeamName: &team.Name},
	); err != nil {
		return nil, ctxerr.Wrap(ctx, err, "create activity for fleet agent options")
	}
	return team, nil
}

////////////////////////////////////////////////////////////////////////////////
// List Team Users
////////////////////////////////////////////////////////////////////////////////

type listTeamUsersRequest struct {
	TeamID      uint              `url:"id"`
	ListOptions fleet.ListOptions `url:"list_options"`
}

func listTeamUsersEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*listTeamUsersRequest)
	users, err := svc.ListTeamUsers(ctx, req.TeamID, req.ListOptions)
	if err != nil {
		return listUsersResponse{Err: err}, nil
	}

	resp := listUsersResponse{Users: []fleet.User{}}
	for _, user := range users {
		resp.Users = append(resp.Users, *user)
	}
	return resp, nil
}

func (svc *Service) ListTeamUsers(ctx context.Context, teamID uint, opt fleet.ListOptions) ([]*fleet.User, error) {
	if err := svc.authz.Authorize(ctx, &fleet.Team{ID: teamID}, fleet.ActionRead); err != nil {
		return nil, err
	}

	users, err := svc.ds.ListUsers(ctx, fleet.UserListOptions{ListOptions: opt, TeamID: teamID})
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "list fleet users")
	}
	return users, nil
}

////////////////////////////////////////////////////////////////////////////////
// Add / Delete Team Users
////////////////////////////////////////////////////////////////////////////////

// same request struct for add and delete
type modifyTeamUsersRequest struct {
	TeamID uint `json:"-" url:"id"`
	// User ID and role must be specified for add users, user ID must be
	// specified for delete users.
	Users []fleet.TeamUser `json:"users"`
}

func addTeamUsersEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*modifyTeamUsersRequest)
	team, err := svc.AddTeamUsers(ctx, req.TeamID, req.Users)
	if err != nil {
		return teamResponse{Err: err}, nil
	}
	return teamResponse{Team: team}, err
}

func (svc *Service) AddTeamUsers(ctx context.Context, teamID uint, users []fleet.TeamUser) (*fleet.Team, error) {
	if err := svc.authz.Authorize(ctx, &fleet.Team{ID: teamID}, fleet.ActionWriteMembers); err != nil {
		return nil, err
	}

	for _, u := range users {
		if !fleet.ValidTeamRole(u.Role) {
			return nil, ctxerr.Wrap(ctx, fleet.NewInvalidArgumentError("role",
				fmt.Sprintf("%q is not a valid fleet role", u.Role)))
		}
	}

	team, err := svc.ds.TeamWithExtras(ctx, teamID)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "get fleet")
	}

	// Adding a user already on the fleet updates their role rather than
	// producing a duplicate membership.
	byID := make(map[uint]int, len(team.Users))
	for i, existing := range team.Users {
		byID[existing.ID] = i
	}
	for _, u := range users {
		if i, ok := byID[u.ID]; ok {
			team.Users[i].Role = u.Role
			continue
		}
		byID[u.ID] = len(team.Users)
		team.Users = append(team.Users, u)
	}

	team, err = svc.ds.SaveTeam(ctx, team)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "save fleet users")
	}
	return team, nil
}

func deleteTeamUsersEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*modifyTeamUsersRequest)
	team, err := svc.DeleteTeamUsers(ctx, req.TeamID, req.Users)
	if err != nil {
		return teamResponse{Err: err}, nil
	}
	return teamResponse{Team: team}, err
}

func (svc *Service) DeleteTeamUsers(ctx context.Context, teamID uint, users []fleet.TeamUser) (*fleet.Team, error) {
	if err := svc.authz.Authorize(ctx, &fleet.Team{ID: teamID}, fleet.ActionWriteMembers); err != nil {
		return nil, err
	}

	team, err := svc.ds.TeamWithExtras(ctx, teamID)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "get fleet")
	}

	removing := make(map[uint]struct{}, len(users))
	for _, u := range users {
		removing[u.ID] = struct{}{}
	}

	kept := make([]fleet.TeamUser, 0, len(team.Users))
	for _, existing := range team.Users {
		if _, drop := removing[existing.ID]; !drop {
			kept = append(kept, existing)
		}
	}
	team.Users = kept

	team, err = svc.ds.SaveTeam(ctx, team)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "save fleet users")
	}
	return team, nil
}

////////////////////////////////////////////////////////////////////////////////
// Get enroll secrets for team
////////////////////////////////////////////////////////////////////////////////

type teamEnrollSecretsRequest struct {
	TeamID uint `url:"id"`
}

type teamEnrollSecretsResponse struct {
	Secrets []*fleet.EnrollSecret `json:"secrets"`
	Err     error                 `json:"error,omitempty"`
}

func (r teamEnrollSecretsResponse) Error() error { return r.Err }

func teamEnrollSecretsEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*teamEnrollSecretsRequest)
	secrets, err := svc.TeamEnrollSecrets(ctx, req.TeamID)
	if err != nil {
		return teamEnrollSecretsResponse{Err: err}, nil
	}

	return teamEnrollSecretsResponse{Secrets: secrets}, err
}

func (svc *Service) TeamEnrollSecrets(ctx context.Context, teamID uint) ([]*fleet.EnrollSecret, error) {
	if err := svc.authz.Authorize(ctx, &fleet.EnrollSecret{TeamID: &teamID}, fleet.ActionRead); err != nil {
		return nil, err
	}

	secrets, err := svc.ds.GetEnrollSecrets(ctx, &teamID)
	if err != nil {
		return nil, ctxerr.Wrap(ctx, err, "get fleet enroll secrets")
	}
	return secrets, nil
}

////////////////////////////////////////////////////////////////////////////////
// Modify enroll secrets for team
////////////////////////////////////////////////////////////////////////////////

type modifyTeamEnrollSecretsRequest struct {
	TeamID  uint                 `url:"fleet_id"`
	Secrets []fleet.EnrollSecret `json:"secrets"`
}

func modifyTeamEnrollSecretsEndpoint(ctx context.Context, request interface{}, svc fleet.Service) (fleet.Errorer, error) {
	req := request.(*modifyTeamEnrollSecretsRequest)
	secrets, err := svc.ModifyTeamEnrollSecrets(ctx, req.TeamID, req.Secrets)
	if err != nil {
		return teamEnrollSecretsResponse{Err: err}, nil
	}

	return teamEnrollSecretsResponse{Secrets: secrets}, err
}

func (svc *Service) ModifyTeamEnrollSecrets(ctx context.Context, teamID uint, secrets []fleet.EnrollSecret) ([]*fleet.EnrollSecret, error) {
	if err := svc.authz.Authorize(ctx, &fleet.EnrollSecret{TeamID: &teamID}, fleet.ActionWrite); err != nil {
		return nil, err
	}

	applied := make([]*fleet.EnrollSecret, len(secrets))
	for i := range secrets {
		applied[i] = &fleet.EnrollSecret{Secret: secrets[i].Secret}
	}
	if err := validateFleetSecrets(applied); err != nil {
		return nil, ctxerr.Wrap(ctx, err)
	}

	if err := svc.ds.ApplyEnrollSecrets(ctx, &teamID, applied); err != nil {
		return nil, ctxerr.Wrap(ctx, err, "apply fleet enroll secrets")
	}

	return svc.ds.GetEnrollSecrets(ctx, &teamID)
}
