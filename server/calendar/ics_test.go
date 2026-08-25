package calendar

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/fleetdm/fleet/v4/server/fleet"
)

type recordingMailer struct {
	sent []fleet.Email
	err  error
}

func (m *recordingMailer) SendEmail(_ context.Context, e fleet.Email) error {
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, e)
	return nil
}

func (m *recordingMailer) CanSendEmail(fleet.SMTPSettings) bool { return true }

func (m *recordingMailer) lastMessage(t *testing.T) string {
	t.Helper()
	require.NotEmpty(t, m.sent)
	message, err := m.sent[len(m.sent)-1].Mailer.Message()
	require.NoError(t, err)
	return string(message)
}

func newTestCalendar(t *testing.T) (*ICSCalendar, *recordingMailer) {
	t.Helper()
	mailer := &recordingMailer{}
	cal := NewICSCalendar(ICSConfig{
		Mailer:         mailer,
		SMTPSettings:   fleet.SMTPSettings{SMTPSenderAddress: "fleet@example.com"},
		OrganizerEmail: "fleet@example.com",
		OrgName:        "Acme",
		ServerURL:      "https://fleet.example.com",
	})
	require.NoError(t, cal.Configure("bob@example.com"))
	return cal, mailer
}

func body(conflict bool) (string, bool, error) {
	if conflict {
		return "conflict body", true, nil
	}
	return "Acme <b>needs</b> to update your computer.<br />Open Fleet.", true, nil
}

func TestCreateEventSendsInvitation(t *testing.T) {
	cal, mailer := newTestCalendar(t)

	tomorrow := time.Now().AddDate(0, 0, 1)
	event, err := cal.CreateEvent(tomorrow, body, fleet.CalendarCreateEventOpts{})
	require.NoError(t, err)

	require.Equal(t, "bob@example.com", event.Email)
	require.NotEmpty(t, event.UUID)
	require.Equal(t, eventDuration, event.EndTime.Sub(event.StartTime))
	require.Equal(t, startHour, event.StartTime.In(time.UTC).Hour())

	require.Len(t, mailer.sent, 1)
	require.Equal(t, []string{"bob@example.com"}, mailer.sent[0].To)
	require.Equal(t, "Acme: scheduled maintenance", mailer.sent[0].Subject)

	message := mailer.lastMessage(t)
	require.Contains(t, message, "text/calendar")
	require.Contains(t, message, "method=REQUEST")
	require.Contains(t, message, "BEGIN:VEVENT")
	require.Contains(t, message, "UID:"+event.UUID)
	require.Contains(t, message, "ATTENDEE")
	// The HTML Fleet generates is flattened for the plain-text parts.
	require.NotContains(t, message, "<b>")
}

func TestCreateEventHonorsRequestedUUID(t *testing.T) {
	cal, _ := newTestCalendar(t)

	event, err := cal.CreateEvent(time.Now().AddDate(0, 0, 1), body,
		fleet.CalendarCreateEventOpts{EventUUID: "fixed-uuid"})
	require.NoError(t, err)
	require.Equal(t, "fixed-uuid", event.UUID)
}

func TestCreateEventReportsDayEnded(t *testing.T) {
	cal, mailer := newTestCalendar(t)

	// A date whose maintenance window is long past must ask the caller to move
	// to the next business day rather than scheduling in the past.
	_, err := cal.CreateEvent(time.Now().AddDate(0, 0, -2), body, fleet.CalendarCreateEventOpts{})
	var dayEnded fleet.DayEndedError
	require.ErrorAs(t, err, &dayEnded)
	require.Empty(t, mailer.sent)
}

func TestCreateEventRequiresConfiguredUser(t *testing.T) {
	cal := NewICSCalendar(ICSConfig{Mailer: &recordingMailer{}})
	_, err := cal.CreateEvent(time.Now().AddDate(0, 0, 1), body, fleet.CalendarCreateEventOpts{})
	require.Error(t, err)

	require.Error(t, cal.Configure(" "))
}

func TestUpdateEventBodyBumpsSequence(t *testing.T) {
	cal, mailer := newTestCalendar(t)

	event, err := cal.CreateEvent(time.Now().AddDate(0, 0, 1), body, fleet.CalendarCreateEventOpts{})
	require.NoError(t, err)
	require.Contains(t, mailer.lastMessage(t), "SEQUENCE:1")

	etag, err := cal.UpdateEventBody(event, body)
	require.NoError(t, err)
	require.Equal(t, "seq-2", etag)
	require.Contains(t, mailer.lastMessage(t), "SEQUENCE:2")
	require.Len(t, mailer.sent, 2)
}

func TestDeleteEventCancels(t *testing.T) {
	cal, mailer := newTestCalendar(t)

	event, err := cal.CreateEvent(time.Now().AddDate(0, 0, 1), body, fleet.CalendarCreateEventOpts{})
	require.NoError(t, err)

	require.NoError(t, cal.DeleteEvent(event))
	message := mailer.lastMessage(t)
	require.Contains(t, message, "method=CANCEL")
	require.Contains(t, message, "STATUS:CANCELLED")
	require.True(t, strings.HasPrefix(mailer.sent[len(mailer.sent)-1].Subject, "Canceled:"))
}

// An emailed invitation has no remote copy, so Fleet's stored event is always
// authoritative and never needs reconciling.
func TestGetAndUpdateEventReportsNoChange(t *testing.T) {
	cal, mailer := newTestCalendar(t)

	stored := &fleet.CalendarEvent{UUID: "abc", Email: "bob@example.com"}
	got, updated, err := cal.GetAndUpdateEvent(stored, body, fleet.CalendarGetAndUpdateEventOpts{})
	require.NoError(t, err)
	require.False(t, updated)
	require.Same(t, stored, got)
	require.Empty(t, mailer.sent)
}

func TestStopEventChannelIsNoop(t *testing.T) {
	cal, _ := newTestCalendar(t)
	require.NoError(t, cal.StopEventChannel(&fleet.CalendarEvent{UUID: "abc"}))
}

func TestGetReadsStoredEventData(t *testing.T) {
	cal, _ := newTestCalendar(t)

	event := &fleet.CalendarEvent{UUID: "abc"}
	require.NoError(t, event.SaveDataItems("body_tag", "default"))

	value, err := cal.Get(event, "body_tag")
	require.NoError(t, err)
	require.Equal(t, "default", value)

	_, err = cal.Get(event, "missing")
	require.Error(t, err)
}

func TestSendSurfacesMailerFailure(t *testing.T) {
	mailer := &recordingMailer{err: errors.New("smtp down")}
	cal := NewICSCalendar(ICSConfig{Mailer: mailer, OrganizerEmail: "fleet@example.com"})
	require.NoError(t, cal.Configure("bob@example.com"))

	_, err := cal.CreateEvent(time.Now().AddDate(0, 0, 1), body, fleet.CalendarCreateEventOpts{})
	require.ErrorContains(t, err, "smtp down")
}

func TestSendRequiresMailer(t *testing.T) {
	cal := NewICSCalendar(ICSConfig{})
	require.NoError(t, cal.Configure("bob@example.com"))

	_, err := cal.CreateEvent(time.Now().AddDate(0, 0, 1), body, fleet.CalendarCreateEventOpts{})
	require.ErrorContains(t, err, "no mail service")
}

func TestPlainText(t *testing.T) {
	require.Equal(t,
		"Acme needs to update.\nOpen Fleet & click 'My device'",
		plainText(`<p>Acme needs to update.</p><br />Open Fleet &amp; click &apos;My device&apos;`),
	)
}
