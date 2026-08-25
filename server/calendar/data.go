package calendar

import (
	"encoding/json"

	"github.com/fleetdm/fleet/v4/server/fleet"
)

// unmarshalEventData decodes the JSON blob Fleet stores alongside a calendar
// event. An event that has never had data saved decodes as an empty object
// rather than an error.
func unmarshalEventData(event *fleet.CalendarEvent, out interface{}) error {
	if len(event.Data) == 0 {
		return json.Unmarshal([]byte("{}"), out)
	}
	return json.Unmarshal(event.Data, out)
}
