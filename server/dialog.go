package main

import (
	"errors"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

// The repeat choices the dialog offers. They are the values of the select, so
// they stay stable and untranslated; only the labels are translated.
const (
	repeatOnce     = "once"
	repeatDaily    = "daily"
	repeatWeekdays = "weekdays"
	repeatWeekly   = "weekly"
	repeatMonthly  = "monthly"
)

// The elements of the schedule dialog, named here because the submission is read
// back by these names.
const (
	dialogElementMessage = "message"
	dialogElementTarget  = "target"
	dialogElementWhen    = "when"
	dialogElementRepeat  = "repeat"
)

var errDialogDateTime = errors.New("unrecognized datetime")

// parseDialogDateTime reads the datetime a dialog submits. The API documents
// four layouts for it: two carry an offset and are instants, two are wall clocks
// that only mean anything in the time zone of whoever reads them.
func parseDialogDateTime(value string, location *time.Location) (time.Time, error) {
	for _, layout := range []string{
		model.ISODateTimeFormat,
		model.ISODateTimeWithTimezoneFormat,
		time.RFC3339Nano,
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}

	for _, layout := range []string{
		model.ISODateTimeNoTimezoneFormat,
		model.ISODateTimeNoSecondsFormat,
	} {
		if parsed, err := time.ParseInLocation(layout, value, location); err == nil {
			return parsed, nil
		}
	}

	return time.Time{}, errDialogDateTime
}

// composeWhen turns the instant the picker returned and the repeat choice into
// the phrase the reminder engine already parses, so the dialog schedules through
// the same code the command line does and the list renders the result the same
// way.
//
// Only a one-off lands exactly on the picked instant; a repeat starts at the
// next matching time, which is what the repeat help text says.
func (p *Plugin) composeWhen(user *model.User, picked time.Time, repeat string) string {
	T, _ := p.translation(user)

	wall := picked.In(p.location(user))
	// time.Kitchen is the form the parser resolves deterministically. A bare
	// "09:30" reaches the 12 hour branch, which takes the meridiem from the
	// current wall clock, so it means morning or evening depending on when it is
	// read.
	at := wall.Format(time.Kitchen)

	switch repeat {
	case repeatDaily:
		return T("every") + " " + T("day") + " " + T("at") + " " + at
	case repeatWeekdays:
		return T("every") + " " + T("weekdays") + " " + T("at") + " " + at
	case repeatWeekly:
		return T("every") + " " + p.weekdayName(user, wall.Weekday()) + " " + T("at") + " " + at
	case repeatMonthly:
		// A day of the month the date parser rolls forward when it has passed,
		// which is what makes the phrase resolve to the following month when the
		// occurrence is rescheduled.
		return T("every") + " " + p.daySuffixFromInt(user, wall.Day()) + " " + T("at") + " " + at
	default:
		return T("on") + " " + wall.Format("2006-01-02") + " " + T("at") + " " + at
	}
}

// weekdayName is the key the parser's weekday table uses for a day, so a
// composed phrase resolves in every locale.
func (p *Plugin) weekdayName(user *model.User, day time.Weekday) string {
	T, _ := p.translation(user)

	keys := [...]string{
		"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday",
	}

	return T(keys[day])
}

// dialogTimezone is the IANA name the picker should show its times in. A user
// who has not set a time zone is on the server's, which Go reports as "Local" -
// not a name the picker understands, so it is left empty and the dialog says the
// device's zone is used instead.
func (p *Plugin) dialogTimezone(user *model.User) string {
	name := p.location(user).String()
	if name == "Local" {
		return ""
	}

	return name
}

// dialogTarget is the target the engine expects: yourself when nothing was
// given, and otherwise an @user or ~channel.
func (p *Plugin) dialogTarget(user *model.User, target string) string {
	T, _ := p.translation(user)

	target = strings.TrimSpace(target)
	if target == "" || target == T("me") || target == "@"+T("me") {
		return T("me")
	}

	if !strings.HasPrefix(target, "@") && !strings.HasPrefix(target, "~") {
		return "@" + target
	}

	return target
}

// dialogRepeat is the repeat choice the submission asked for, with anything
// unrecognized - including a field the client left out - read as a one-off.
func dialogRepeat(value string) string {
	switch value {
	case repeatDaily, repeatWeekdays, repeatWeekly, repeatMonthly:
		return value
	default:
		return repeatOnce
	}
}

// submissionString reads a dialog field that may be absent or not a string at
// all, which is how every field of a submission has to be read: the body comes
// from the client.
func submissionString(submission map[string]any, name string) string {
	value, ok := submission[name]
	if !ok {
		return ""
	}

	text, _ := value.(string)

	return text
}

// whenHelpText says which zone the picked time is read in, which matters most
// when the user has not set one and the server's zone is used.
func (p *Plugin) whenHelpText(user *model.User, zone string) string {
	T, _ := p.translation(user)

	if zone == "" {
		return T("schedule.when.help")
	}

	return T("schedule.when.help.zone", map[string]interface{}{"Zone": zone})
}
