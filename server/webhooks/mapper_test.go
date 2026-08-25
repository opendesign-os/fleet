package webhooks

import (
	"net/url"
	"testing"
	"time"

	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/fleetdm/fleet/v4/server/ptr"
	"github.com/stretchr/testify/require"
)

func TestGetPaylaod(t *testing.T) {
	serverURL, err := url.Parse("http://mywebsite.com")
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	vuln := fleet.SoftwareVulnerability{
		CVE:        "cve-1",
		SoftwareID: 1,
	}
	meta := fleet.CVEMeta{
		CVE:              "cve-1",
		CVSSScore:        ptr.Float64(1),
		EPSSProbability:  ptr.Float64(0.5),
		CISAKnownExploit: ptr.Bool(true),
		Published:        ptr.Time(now),
	}

	sut := Mapper{}

	t.Run("includes vulnerability scores", func(t *testing.T) {
		result := sut.GetPayload(serverURL, nil, vuln.CVE, meta)
		require.Equal(t, meta.CISAKnownExploit, result.CISAKnownExploit)
		require.Equal(t, meta.EPSSProbability, result.EPSSProbability)
		require.Equal(t, meta.CVSSScore, result.CVSSScore)
		require.Equal(t, meta.Published, result.CVEPublished)
	})

	t.Run("omits scores when the CVE has no metadata", func(t *testing.T) {
		result := sut.GetPayload(serverURL, nil, vuln.CVE, fleet.CVEMeta{CVE: "cve-1"})
		require.Nil(t, result.CISAKnownExploit)
		require.Nil(t, result.EPSSProbability)
		require.Nil(t, result.CVSSScore)
		require.Nil(t, result.CVEPublished)
	})

	t.Run("host payload only includes valid software paths", func(t *testing.T) {
		hosts := []fleet.HostVulnerabilitySummary{
			{
				ID:          1,
				Hostname:    "host1",
				DisplayName: "d-host1",
				SoftwareInstalledPaths: []string{
					"",
					"/some/path",
				},
			},
			{
				ID:                     2,
				Hostname:               "host2",
				DisplayName:            "d-host2",
				SoftwareInstalledPaths: nil,
			},
		}
		result := sut.GetPayload(serverURL, hosts, vuln.CVE, meta)
		require.ElementsMatch(t, result.Hosts, []*hostPayloadPart{
			{
				ID:                     uint(1),
				Hostname:               "host1",
				DisplayName:            "d-host1",
				URL:                    "http://mywebsite.com/hosts/1",
				SoftwareInstalledPaths: []string{"/some/path"},
			},
			{
				ID:          uint(2),
				Hostname:    "host2",
				DisplayName: "d-host2",
				URL:         "http://mywebsite.com/hosts/2",
			},
		},
		)
	})
}
