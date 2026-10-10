package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestScheduleReminders(_ *testing.T) {
	// user := &model.User{
	//	Email:    "-@-.-",
	//	Nickname: "TestUser",
	//	Password: model.NewId(),
	//	Username: "testuser",
	//	Roles:    model.SystemUserRoleId,
	//	Locale:   "en",
	//}
	//
	// occurrences := []Occurrence{
	//	{
	//		Id: model.NewId(),
	//		ReminderId: model.NewId(),
	//		Occurrence:  time.Now(),
	//	},
	//}
	//
	// stringOccurrences, _ := json.Marshal(occurrences)
	// channel := &model.Channel{
	//	Id: model.NewId(),
	//}
	//
	// setupAPI := func() *plugintest.API {
	//	api := &plugintest.API{}
	//	api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
	//	api.On("LogInfo", mock.Anything).Maybe()
	//	api.On("GetUserByUsername", mock.AnythingOfType("string")).Return(user, nil)
	//	api.On("KVGet", mock.AnythingOfType("string")).Return(stringOccurrences, nil)
	//	api.On("KVSet", mock.AnythingOfType("string"), stringOccurrences).Maybe()
	//	return api
	//}
	//
	// t.Run("if scheduled reminder" , func(t *testing.T) {
	//
	//	api := setupAPI()
	//	defer api.AssertExpectations(t)
	//
	//	p := &Plugin{}
	//	p.API = api
	//
	//	request := &ReminderRequest{
	//		Username:   user.Username,
	//		Payload:    "me foo in 1 seconds",
	//	}
	//	post, err := p.ScheduleReminder(request, channel.Id)
	//	assert.Nil(t, err)
	//	assert.Equal(t, len(post.Attachments()), 2)
	//
	// })
}

func TestInteractiveSchedule(t *testing.T) {
	user := &model.User{
		Email:    "-@-.-",
		Nickname: "TestUser",
		Password: model.NewId(),
		Username: "testuser",
		Roles:    model.SystemUserRoleId,
		Locale:   "en",
		Timezone: map[string]string{"manualTimezone": "America/Denver"},
	}

	openDialog := func(t *testing.T, user *model.User, reminders []Reminder) model.Dialog {
		t.Helper()

		stored, err := json.Marshal(reminders)
		require.NoError(t, err)

		var opened model.OpenDialogRequest
		api := &plugintest.API{}
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("KVGet", user.Username).Return(stored, nil)
		api.On("OpenInteractiveDialog", mock.AnythingOfType("model.OpenDialogRequest")).
			Run(func(args mock.Arguments) { opened = args.Get(0).(model.OpenDialogRequest) }).
			Return(nil)
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.API = api
		// What "not completed" and "not snoozed" are stored as.
		p.emptyTime = time.Time{}.AddDate(1, 1, 1)

		p.InteractiveSchedule(model.NewId(), user)

		// IsValid holds the dialog to every limit the API sets: the label and
		// help text lengths, a select default that is one of its options, and
		// the date/time configuration. A dialog the server would refuse cannot
		// be built without this failing.
		require.NoError(t, opened.Dialog.IsValid())

		return opened.Dialog
	}

	elementsByName := func(dialog model.Dialog) map[string]model.DialogElement {
		elements := map[string]model.DialogElement{}
		for _, element := range dialog.Elements {
			elements[element.Name] = element
		}

		return elements
	}

	t.Run("the time is picked, and shown in the user's zone", func(t *testing.T) {
		elements := elementsByName(openDialog(t, user, nil))

		when, ok := elements[dialogElementWhen]
		require.True(t, ok)
		assert.Equal(t, "datetime", when.Type)
		require.NotNil(t, when.DateTimeConfig)
		assert.Equal(t, "today", when.DateTimeConfig.MinDate)
		assert.Equal(t, 15, when.DateTimeConfig.TimeInterval)
		assert.True(t, when.DateTimeConfig.ManualTimeEntry)
		assert.Equal(t, "America/Denver", when.DateTimeConfig.LocationTimezone)
		// The help text names the zone rather than leaving it to the device.
		assert.NotEqual(t, TfuncWithFallback("en")("schedule.when.help"), when.HelpText)
	})

	t.Run("the repeat offers each cadence the engine can express", func(t *testing.T) {
		elements := elementsByName(openDialog(t, user, nil))

		repeat, ok := elements[dialogElementRepeat]
		require.True(t, ok)
		assert.Equal(t, "select", repeat.Type)
		assert.Equal(t, repeatOnce, repeat.Default)

		var offered []string
		for _, option := range repeat.Options {
			offered = append(offered, option.Value)
		}
		assert.Equal(t, []string{repeatOnce, repeatDaily, repeatWeekdays, repeatWeekly, repeatMonthly}, offered)
	})

	t.Run("a user with no zone is left to the device's", func(t *testing.T) {
		elements := elementsByName(openDialog(t, &model.User{Username: "testuser", Locale: "en"}, nil))

		when := elements[dialogElementWhen]
		require.NotNil(t, when.DateTimeConfig)
		// "Local" is what Go calls the host's zone, and it is not a name the
		// picker can be handed.
		assert.Empty(t, when.DateTimeConfig.LocationTimezone)
		assert.Equal(t, TfuncWithFallback("en")("schedule.when.help"), when.HelpText)
	})

	t.Run("every reminder that can still fire is a checkbox named after it", func(t *testing.T) {
		soon := time.Now().UTC().Round(time.Second).Add(time.Hour)
		gone := time.Now().UTC().Round(time.Second).Add(-time.Hour)

		open := time.Time{}.AddDate(1, 1, 1)
		newReminder := func(message, target, when, repeat string, fires, completed time.Time) Reminder {
			reminder := Reminder{
				Id: model.NewId(), Username: user.Username, Target: target, Message: message,
				When: when, Completed: completed,
			}
			reminder.Occurrences = []Occurrence{
				{Id: model.NewId(), ReminderId: reminder.Id, Occurrence: fires, Repeat: repeat},
			}

			return reminder
		}

		upcoming := newReminder("propa meeting", "me", "in 1 hour", "", soon, open)
		repeating := newReminder("standup", "me", "every monday at 9am", "every monday at 9am", soon, open)
		lingering := newReminder("deploy", "~town-square", "in 1 hour", "", gone, open)
		done := newReminder("old news", "me", "in 1 hour", "", soon, time.Now().UTC())

		elements := elementsByName(openDialog(t, user, []Reminder{upcoming, repeating, lingering, done}))

		for _, reminder := range []Reminder{upcoming, repeating, lingering} {
			element, ok := elements[dialogElementRemove+reminder.Id]
			require.True(t, ok, "reminder %q is not listed", reminder.Message)
			assert.Equal(t, "bool", element.Type)
			assert.LessOrEqual(t, len(element.DisplayName), 24)
			// A checkbox takes the text beside it from the placeholder; leaving
			// it empty is a checkbox with nothing next to it.
			assert.NotEmpty(t, element.Placeholder, "a checkbox with no label is not a list")
			assert.LessOrEqual(t, len(element.Placeholder), 150)
		}
		assert.NotContains(t, elements, dialogElementRemove+done.Id, "a completed reminder is not listed")

		// A checkbox is labeled with when it fires and described by what it
		// says, with the target in front of a reminder that is not for you. A
		// repeating reminder is labeled with how it repeats instead.
		for _, reminder := range []Reminder{upcoming, lingering} {
			label := elements[dialogElementRemove+reminder.Id].DisplayName
			assert.NotEmpty(t, label)
			assert.NotEqual(t, reminder.Message, label, "the message is the help text, not the label")
		}
		assert.Equal(t, "every monday at 9am", elements[dialogElementRemove+repeating.Id].DisplayName)
		assert.Equal(t, "standup", elements[dialogElementRemove+repeating.Id].Placeholder)
		assert.Equal(t, "~town-square: deploy", elements[dialogElementRemove+lingering.Id].Placeholder)
	})

	t.Run("a long label is cut to what the API accepts, on a rune boundary", func(t *testing.T) {
		long := Reminder{
			Id: model.NewId(), Username: user.Username, Target: "me",
			Message: strings.Repeat("ü", 300),
			When:    "in 1 hour", Completed: time.Time{}.AddDate(1, 1, 1),
			Occurrences: []Occurrence{{Id: model.NewId(), Occurrence: time.Now().UTC().Add(time.Hour)}},
		}

		element := elementsByName(openDialog(t, user, []Reminder{long}))[dialogElementRemove+long.Id]

		assert.LessOrEqual(t, len(element.Placeholder), 150)
		assert.True(t, utf8.ValidString(element.Placeholder), "the cut left half a rune behind")
	})
}
