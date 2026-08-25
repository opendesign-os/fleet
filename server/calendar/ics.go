// Package calendar provides the calendar backends Fleet can schedule
// maintenance windows on.
//
// The ICS backend needs no calendar API: it emails the end user a standard
// iCalendar invitation, which Outlook, Google Calendar, Apple Calendar and any
// other RFC 5545 client add to the user's calendar on accept. Fleet's own
// calendar_events table is the source of truth for the reservation, so this
// backend never has to read state back from a remote calendar.
package calendar

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"sync"
	"time"

	ics "github.com/arran4/golang-ical"

	"github.com/fleetdm/fleet/v4/server/fleet"
)

const (
	// The maintenance window runs from startHour to endHour in the configured
	// timezone; each reservation takes eventDuration.
	startHour     = 9
	endHour       = 17
	eventDuration = 30 * time.Minute

	productID = "-//Fleet//Maintenance Windows//EN"
)

// ICSConfig is what the ICS backend needs to send an invitation.
type ICSConfig struct {
	// Mailer sends the invitation. Required.
	Mailer fleet.MailService
	// SMTPSettings is the mail configuration the invitation is sent with.
	SMTPSettings fleet.SMTPSettings
	// OrganizerEmail is the From address and the iCalendar ORGANIZER.
	OrganizerEmail string
	// OrgName titles the event, e.g. "Acme: scheduled maintenance".
	OrgName string
	// ServerURL is the Fleet URL shown in the invitation.
	ServerURL string
	// Location is the timezone the maintenance window is expressed in. Defaults
	// to UTC when nil.
	Location *time.Location
	Logger   *slog.Logger
}

// ICSCalendar is a fleet.UserCalendar that delivers invitations by email.
//
// Not safe for concurrent use by multiple goroutines: Configure sets the user
// the subsequent calls act on, matching how the calendar cron drives the
// interface one user at a time.
type ICSCalendar struct {
	config    ICSConfig
	userEmail string

	// sequence tracks the iCalendar SEQUENCE per event UID so an updated
	// invitation supersedes the previous one in the recipient's client.
	sequence sync.Map
}

func NewICSCalendar(config ICSConfig) *ICSCalendar {
	if config.Location == nil {
		config.Location = time.UTC
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	return &ICSCalendar{config: config}
}

func (c *ICSCalendar) Configure(userEmail string) error {
	if strings.TrimSpace(userEmail) == "" {
		return errors.New("user email is required")
	}
	c.userEmail = userEmail
	return nil
}

func (c *ICSCalendar) CreateEvent(
	dateOfEvent time.Time,
	genBodyFn fleet.CalendarGenBodyFn,
	opts fleet.CalendarCreateEventOpts,
) (*fleet.CalendarEvent, error) {
	if c.userEmail == "" {
		return nil, errors.New("calendar not configured for a user")
	}

	startTime, err := c.nextSlot(dateOfEvent)
	if err != nil {
		return nil, err
	}

	body, ok, err := genBodyFn(false)
	if err != nil {
		return nil, fmt.Errorf("generate event body: %w", err)
	}
	if !ok {
		return nil, errors.New("event body generation declined")
	}

	uuid := opts.EventUUID
	if uuid == "" {
		uuid, err = newEventUUID()
		if err != nil {
			return nil, err
		}
	}

	timeZone := c.config.Location.String()
	event := &fleet.CalendarEvent{
		UUID:      uuid,
		Email:     c.userEmail,
		StartTime: startTime,
		EndTime:   startTime.Add(eventDuration),
		TimeZone:  &timeZone,
	}

	if err := c.send(event, body, ics.MethodRequest); err != nil {
		return nil, err
	}
	return event, nil
}

// GetAndUpdateEvent reports the event unchanged. An emailed invitation has no
// remote copy for the user to move or delete, so Fleet's stored event is
// always current.
func (c *ICSCalendar) GetAndUpdateEvent(
	event *fleet.CalendarEvent,
	_ fleet.CalendarGenBodyFn,
	_ fleet.CalendarGetAndUpdateEventOpts,
) (*fleet.CalendarEvent, bool, error) {
	return event, false, nil
}

func (c *ICSCalendar) UpdateEventBody(event *fleet.CalendarEvent, genBodyFn fleet.CalendarGenBodyFn) (string, error) {
	body, ok, err := genBodyFn(false)
	if err != nil {
		return "", fmt.Errorf("generate event body: %w", err)
	}
	if !ok {
		return "", errors.New("event body generation declined")
	}

	if err := c.send(event, body, ics.MethodRequest); err != nil {
		return "", err
	}
	// The ETag is what the caller stores to detect a later change. The sequence
	// number is the iCalendar equivalent, so it doubles as the tag.
	return fmt.Sprintf("seq-%d", c.currentSequence(event.UUID)), nil
}

func (c *ICSCalendar) DeleteEvent(event *fleet.CalendarEvent) error {
	if err := c.Configure(event.Email); err != nil {
		return err
	}
	if err := c.send(event, "", ics.MethodCancel); err != nil {
		return err
	}
	c.sequence.Delete(event.UUID)
	return nil
}

// StopEventChannel is a no-op: there is no push subscription to tear down,
// since the calendar cron polls its own table rather than a remote calendar.
func (c *ICSCalendar) StopEventChannel(_ *fleet.CalendarEvent) error {
	return nil
}

func (c *ICSCalendar) Get(event *fleet.CalendarEvent, key string) (interface{}, error) {
	var data map[string]interface{}
	if err := unmarshalEventData(event, &data); err != nil {
		return nil, err
	}
	value, ok := data[key]
	if !ok {
		return nil, fmt.Errorf("key %q not found in event data", key)
	}
	return value, nil
}

// nextSlot returns the start of the first free slot on the requested date.
// DayEndedError tells the caller to try the next business day, which is how the
// cron walks forward when a date is already spent.
func (c *ICSCalendar) nextSlot(dateOfEvent time.Time) (time.Time, error) {
	year, month, day := dateOfEvent.Date()
	windowStart := time.Date(year, month, day, startHour, 0, 0, 0, c.config.Location)
	windowEnd := time.Date(year, month, day, endHour, 0, 0, 0, c.config.Location)

	start := windowStart
	if now := time.Now().In(c.config.Location); now.After(start) {
		// Round up to the next slot boundary so invitations don't land in the past.
		start = now.Truncate(eventDuration).Add(eventDuration)
	}

	if start.Add(eventDuration).After(windowEnd) {
		return time.Time{}, fleet.DayEndedError{
			Msg: fmt.Sprintf("no time left on %s", dateOfEvent.Format(time.DateOnly)),
		}
	}
	return start, nil
}

func (c *ICSCalendar) send(event *fleet.CalendarEvent, body string, method ics.Method) error {
	if c.config.Mailer == nil {
		return errors.New("no mail service configured for calendar invitations")
	}

	sequence := c.bumpSequence(event.UUID, method)
	invitation := c.buildInvitation(event, body, method, sequence)

	summary := c.summary()
	subject := summary
	if method == ics.MethodCancel {
		subject = "Canceled: " + summary
	}

	email := fleet.Email{
		Subject:      subject,
		To:           []string{event.Email},
		ServerURL:    c.config.ServerURL,
		SMTPSettings: c.config.SMTPSettings,
		Mailer: &invitationMailer{
			invitation: invitation,
			body:       body,
			method:     method,
		},
	}

	if err := c.config.Mailer.SendEmail(context.Background(), email); err != nil {
		return fmt.Errorf("send calendar invitation: %w", err)
	}
	return nil
}

func (c *ICSCalendar) buildInvitation(event *fleet.CalendarEvent, body string, method ics.Method, sequence int) string {
	cal := ics.NewCalendarFor(productID)
	cal.SetMethod(method)

	vevent := cal.AddEvent(event.UUID)
	vevent.SetDtStampTime(time.Now().UTC())
	vevent.SetStartAt(event.StartTime.UTC())
	vevent.SetEndAt(event.EndTime.UTC())
	vevent.SetSummary(c.summary())
	vevent.SetSequence(sequence)
	vevent.SetOrganizer("mailto:" + c.config.OrganizerEmail)
	vevent.AddAttendee(event.Email,
		ics.ParticipationStatusNeedsAction,
		ics.ParticipationRoleReqParticipant,
		ics.WithRSVP(true),
	)

	if method == ics.MethodCancel {
		vevent.SetStatus(ics.ObjectStatusCancelled)
	} else {
		vevent.SetStatus(ics.ObjectStatusConfirmed)
		vevent.SetDescription(plainText(body))
		if c.config.ServerURL != "" {
			vevent.SetURL(c.config.ServerURL)
		}
	}

	return cal.Serialize()
}

func (c *ICSCalendar) summary() string {
	if c.config.OrgName == "" {
		return "Scheduled maintenance"
	}
	return c.config.OrgName + ": scheduled maintenance"
}

// bumpSequence returns the SEQUENCE for the next invitation of an event.
// Calendar clients treat a higher sequence as superseding the previous one.
func (c *ICSCalendar) bumpSequence(uuid string, method ics.Method) int {
	if method == ics.MethodRequest {
		next := c.currentSequence(uuid) + 1
		c.sequence.Store(uuid, next)
		return next
	}
	return c.currentSequence(uuid)
}

func (c *ICSCalendar) currentSequence(uuid string) int {
	if value, ok := c.sequence.Load(uuid); ok {
		if sequence, ok := value.(int); ok {
			return sequence
		}
	}
	return 0
}

func newEventUUID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate event uuid: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// invitationMailer renders the multipart message calendar clients expect: a
// readable text part plus the invitation itself as text/calendar, which is what
// makes the mail show up as an invitation rather than an attachment.
type invitationMailer struct {
	invitation string
	body       string
	method     ics.Method
}

func (m *invitationMailer) Message() ([]byte, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", writer.Boundary())

	text, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {`text/plain; charset="utf-8"`},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return nil, err
	}
	if err := writeQuotedPrintable(text, plainText(m.body)); err != nil {
		return nil, err
	}

	calendarPart, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {fmt.Sprintf(`text/calendar; charset="utf-8"; method=%s`, m.method)},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return nil, err
	}
	if err := writeQuotedPrintable(calendarPart, m.invitation); err != nil {
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeQuotedPrintable(w interface{ Write([]byte) (int, error) }, content string) error {
	encoder := quotedprintable.NewWriter(w)
	if _, err := encoder.Write([]byte(content)); err != nil {
		return err
	}
	return encoder.Close()
}

// plainText strips the HTML Fleet generates for calendar bodies, since the
// iCalendar DESCRIPTION property and the text part are both plain text.
func plainText(body string) string {
	replacer := strings.NewReplacer(
		"<br />", "\n", "<br/>", "\n", "<br>", "\n",
		"&nbsp;", " ", "&apos;", "'", "&amp;", "&", "&lt;", "<", "&gt;", ">",
	)
	body = replacer.Replace(body)

	var out strings.Builder
	inTag := false
	for _, r := range body {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			out.WriteRune(r)
		}
	}
	return strings.TrimSpace(out.String())
}
