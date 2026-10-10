package main

import (
	"testing"

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

	openDialog := func(t *testing.T, user *model.User) model.Dialog {
		t.Helper()

		var opened model.OpenDialogRequest
		api := &plugintest.API{}
		api.On("OpenInteractiveDialog", mock.AnythingOfType("model.OpenDialogRequest")).
			Run(func(args mock.Arguments) { opened = args.Get(0).(model.OpenDialogRequest) }).
			Return(nil)
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.API = api

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
		elements := elementsByName(openDialog(t, user))

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
		elements := elementsByName(openDialog(t, user))

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
		elements := elementsByName(openDialog(t, &model.User{Username: "testuser", Locale: "en"}))

		when := elements[dialogElementWhen]
		require.NotNil(t, when.DateTimeConfig)
		// "Local" is what Go calls the host's zone, and it is not a name the
		// picker can be handed.
		assert.Empty(t, when.DateTimeConfig.LocationTimezone)
		assert.Equal(t, TfuncWithFallback("en")("schedule.when.help"), when.HelpText)
	})
}
