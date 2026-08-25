package fleet

// EnterpriseFeatures reports which enterprise features this build serves from
// the open-source core rather than from a Fleet Premium license. The UI reads
// it to decide which enterprise surfaces to show, so a field is true only when
// the matching API is actually implemented here.
//
// Features that stay behind the license — per-fleet MDM settings, Apple
// Business Manager, volume purchasing, disk-encryption escrow and the setup
// experience — are deliberately absent.
type EnterpriseFeatures struct {
	// Fleets is host grouping with fleet-scoped roles: fleet CRUD, membership,
	// enroll secrets, agent options and GitOps specs. Covers the observer+,
	// technician and GitOps roles, globally and per fleet.
	Fleets bool `json:"fleets"`
	// VulnerabilityScores is CVSS, EPSS and CISA known-exploit data on hosts,
	// software and vulnerability webhooks.
	VulnerabilityScores bool `json:"vulnerability_scores"`
	// CriticalPolicies is marking a policy critical, plus label targeting,
	// continuous automations and patch-when-closed.
	CriticalPolicies bool `json:"critical_policies"`
	// Scim is SCIM 2.0 user and group provisioning together with SSO
	// just-in-time account creation.
	Scim bool `json:"scim"`
	// MaintenanceWindows is reserving time on an end user's calendar for a
	// failing policy.
	MaintenanceWindows bool `json:"maintenance_windows"`
	// ConditionalAccess is publishing a device's compliance verdict to Entra so
	// a Conditional Access policy can act on it.
	ConditionalAccess bool `json:"conditional_access"`
}

// CoreEnterpriseFeatures is what this build implements. It is a fixed set
// rather than a runtime probe: every field corresponds to code compiled into
// the server.
func CoreEnterpriseFeatures() EnterpriseFeatures {
	return EnterpriseFeatures{
		Fleets:              true,
		VulnerabilityScores: true,
		CriticalPolicies:    true,
		Scim:                true,
		MaintenanceWindows:  true,
		ConditionalAccess:   true,
	}
}
