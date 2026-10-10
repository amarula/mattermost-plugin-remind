package main

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

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

// dialogReminderListMax is how many reminders the dialog lists for removal. It
// is a readability limit, not an API one: a dialog with fifty checkboxes is a
// scroll, and the list post is still there for everything.
const dialogReminderListMax = 20

// dialogElementRemove is the prefix of the checkbox that marks a reminder for
// removal: the rest of the name is the reminder's id.
const dialogElementRemove = "remove_"

// truncateBytes cuts a string to a byte limit without splitting a rune, which
// matters because the API measures these fields in bytes and a reminder's text
// is whatever the user typed.
func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}

	cut := limit - len("…")
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}

	return value[:cut] + "…"
}

// dialogReminders is what the dialog offers for removal: the reminders that can
// still fire, in the order the list post shows them, cut to a readable number.
func (p *Plugin) dialogReminders(reminders []Reminder) []Reminder {
	upcoming, recurring, past, channel := p.categorizeOccurrences(reminders)

	ordered := make([]Reminder, 0, len(reminders))
	for _, group := range [][]Occurrence{upcoming, recurring, past, channel} {
		for _, occurrence := range group {
			reminder, found := findReminderByID(reminders, occurrence.ReminderId)
			if !found {
				continue
			}

			ordered = append(ordered, reminder)
		}
	}

	if len(ordered) > dialogReminderListMax {
		ordered = ordered[:dialogReminderListMax]
	}

	return ordered
}

// dialogReminderElements is one checkbox per reminder, named after it.
//
// A checkbox takes the text beside it from its placeholder, not from its display
// name, which labels the field and is capped at 24 bytes: the "when" goes there
// and what the reminder says goes beside the box.
func (p *Plugin) dialogReminderElements(user *model.User, reminders []Reminder) []model.DialogElement {
	list := p.dialogReminders(reminders)

	elements := make([]model.DialogElement, 0, len(list))
	for _, reminder := range list {
		elements = append(elements, model.DialogElement{
			DisplayName: p.dialogReminderLabel(user, reminder),
			Name:        dialogElementRemove + reminder.Id,
			Type:        "bool",
			Placeholder: p.dialogReminderText(user, reminder),
			Optional:    true,
		})
	}

	return elements
}

// dialogReminderLabel is the short "when" a checkbox is labeled with.
func (p *Plugin) dialogReminderLabel(user *model.User, reminder Reminder) string {
	T, _ := p.translation(user)
	location := p.location(user)

	// A repeating reminder says how it repeats, which is what its When phrase is.
	if len(reminder.Occurrences) > 0 && reminder.Occurrences[0].Repeat != "" {
		return truncateBytes(reminder.When, model.DialogElementDisplayNameMaxLength)
	}

	if len(reminder.Occurrences) == 0 {
		return truncateBytes(reminder.Message, model.DialogElementDisplayNameMaxLength)
	}

	now := time.Now().In(location)
	fires := reminder.Occurrences[0].Occurrence.In(location)

	switch {
	case sameDay(fires, now):
		return T("today") + " " + fires.Format(time.Kitchen)
	case sameDay(fires, now.AddDate(0, 0, 1)):
		return T("tomorrow") + " " + fires.Format(time.Kitchen)
	default:
		return fires.Format("Mon 2 Jan 15:04")
	}
}

// dialogReminderText is what a checkbox says beside it: the reminder's message,
// with the target in front of it when it is not the user themselves.
func (p *Plugin) dialogReminderText(user *model.User, reminder Reminder) string {
	T, _ := p.translation(user)

	text := reminder.Message
	if reminder.Target != "" && reminder.Target != T("me") {
		text = reminder.Target + ": " + text
	}

	return truncateBytes(text, model.DialogElementBoolMaxLength)
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// tickedReminderIDs are the reminders a submission asked to remove. A checkbox
// arrives as a bool from one client and as the word "true" from another, and an
// unticked one may be absent altogether.
func tickedReminderIDs(submission map[string]any) []string {
	var ids []string
	for name, value := range submission {
		if !strings.HasPrefix(name, dialogElementRemove) || !submissionBool(value) {
			continue
		}

		ids = append(ids, strings.TrimPrefix(name, dialogElementRemove))
	}

	return ids
}

func submissionBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

// findReminderByID is the reminder with this id, if it is still there: one can
// be completed or deleted while the dialog it is listed in is open.
func findReminderByID(reminders []Reminder, id string) (Reminder, bool) {
	for _, reminder := range reminders {
		if reminder.Id == id {
			return reminder, true
		}
	}

	return Reminder{}, false
}

// removeReminder clears what a reminder has scheduled and forgets it, which is
// what the list post's Delete does.
func (p *Plugin) removeReminder(user *model.User, reminder Reminder) {
	for _, occurrence := range reminder.Occurrences {
		p.ClearScheduledOccurrence(reminder, occurrence)
	}

	if err := p.DeleteReminder(user.Id, reminder); err != nil {
		p.API.LogError("failed to delete reminder " + err.Error())
	}
}
