package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestHandleDialog(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
		Locale:   "en",
		Timezone: map[string]string{"manualTimezone": "UTC"},
	}

	// A Thursday, so a weekly repeat has a weekday to name.
	picked := time.Date(2026, time.November, 5, 14, 30, 0, 0, time.UTC)

	// setupAPI records what the submission stores and posts, starting from the
	// reminders the user already has.
	setupAPI := func(t *testing.T, existing []Reminder) (*plugintest.API, *[]Reminder, *[]*model.Post) {
		t.Helper()

		stored := existing
		var posted []*model.Post

		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("LogError", mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything).Maybe()
		api.On("GetUser", mock.Anything).Return(user, nil)
		// Only the paths that store a reminder look the target up.
		api.On("GetUserByUsername", mock.Anything).Return(user, nil).Maybe()
		// The stored list is read back as it stands, so a submit that adds and
		// removes sees its own write, the way the real store would.
		api.On("KVGet", user.Username).Return(func(string) ([]byte, *model.AppError) {
			return mustMarshal(t, stored), nil
		}).Maybe()
		api.On("KVGet", mock.Anything).Return(nil, nil).Maybe()
		// The reminder list is written under the user's name; the fire-time
		// buckets it also writes are not what these tests are about.
		api.On("KVSet", user.Username, mock.Anything).Run(func(args mock.Arguments) {
			_ = json.Unmarshal(args.Get(1).([]byte), &stored)
		}).Return(nil).Maybe()
		api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
		api.On("SendEphemeralPost", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
			posted = append(posted, args.Get(1).(*model.Post))
		}).Return(nil).Maybe()

		return api, &stored, &posted
	}

	// refreshed fails unless the submission answered with a dialog, and gives
	// the elements of the one it answered with.
	refreshed := func(t *testing.T, response model.SubmitDialogResponse) map[string]model.DialogElement {
		t.Helper()

		require.Equal(t, string(model.SubmitDialogResponseTypeForm), response.Type,
			"the dialog is handed back rather than closed")
		require.NotNil(t, response.Form)
		require.NoError(t, response.Form.IsValid())

		elements := map[string]model.DialogElement{}
		for _, element := range response.Form.Elements {
			elements[element.Name] = element
		}

		return elements
	}

	newPlugin := func(api *plugintest.API) *Plugin {
		p := &Plugin{}
		p.API = api
		p.router = p.InitAPI()
		p.emptyTime = time.Time{}.AddDate(1, 1, 1)
		p.locales = map[string]string{"en": "/test/en.json"}

		return p
	}

	// rowFields are the names the nth new-reminder row's fields are submitted
	// under, read out of the dialog the way a client would see them.
	rowFields := func(t *testing.T, p *Plugin, row int) (string, string, string) {
		t.Helper()

		var message, when, repeat string
		for _, element := range p.scheduleDialog(user).Elements {
			field, rest, found := strings.Cut(element.Name, "_")
			if !found || rest[strings.LastIndex(rest, "_")+1:] != strconv.Itoa(row) {
				continue
			}

			switch field {
			case dialogElementMessage:
				message = element.Name
			case dialogElementWhen:
				when = element.Name
			case dialogElementRepeat:
				repeat = element.Name
			}
		}

		require.NotEmpty(t, message, "row %d has no message field", row)
		require.NotEmpty(t, when, "row %d has no time field", row)
		require.NotEmpty(t, repeat, "row %d has no repeat field", row)

		return message, when, repeat
	}

	submit := func(t *testing.T, p *Plugin, submission model.StringInterface) (int, model.SubmitDialogResponse) {
		t.Helper()

		request := &model.SubmitDialogRequest{
			UserId:     user.Id,
			ChannelId:  model.NewId(),
			Submission: submission,
		}
		body, err := json.Marshal(request)
		require.NoError(t, err)

		w := httptest.NewRecorder()
		p.ServeHTTP(nil, w, httptest.NewRequest("POST", "/dialog", bytes.NewReader(body)))

		var response model.SubmitDialogResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))

		return w.Code, response
	}

	t.Run("a row is stored at the instant that was picked", func(t *testing.T) {
		api, stored, _ := setupAPI(t, nil)
		defer api.AssertExpectations(t)

		p := newPlugin(api)
		message, when, _ := rowFields(t, p, 1)

		status, response := submit(t, p, model.StringInterface{
			message: "standup",
			when:    "2026-11-05T14:30:00Z",
		})

		assert.Equal(t, 200, status)
		assert.Empty(t, response.Errors)
		assert.Empty(t, response.Error)

		require.Len(t, *stored, 1)
		assert.Equal(t, "standup", (*stored)[0].Message)
		assert.Equal(t, "on 2026-11-05 at 2:30PM", (*stored)[0].When)
		require.Len(t, (*stored)[0].Occurrences, 1)
		assert.True(t, picked.Equal((*stored)[0].Occurrences[0].Occurrence))
		assert.Empty(t, (*stored)[0].Occurrences[0].Repeat)

		assert.NotEmpty(t, refreshed(t, response), "the dialog answers")
	})

	t.Run("every row that was filled in is added", func(t *testing.T) {
		api, stored, _ := setupAPI(t, nil)
		defer api.AssertExpectations(t)

		p := newPlugin(api)
		firstMessage, firstWhen, _ := rowFields(t, p, 1)
		secondMessage, secondWhen, secondRepeat := rowFields(t, p, 2)

		status, response := submit(t, p, model.StringInterface{
			firstMessage:  "standup",
			firstWhen:     "2026-11-05T14:30:00Z",
			secondMessage: "propa meeting",
			secondWhen:    "2026-11-06T09:00:00Z",
			secondRepeat:  repeatDaily,
		})

		assert.Equal(t, 200, status)
		assert.Empty(t, response.Errors)

		require.Len(t, *stored, 2)
		assert.Equal(t, "standup", (*stored)[0].Message)
		assert.Equal(t, "propa meeting", (*stored)[1].Message)
		assert.Equal(t, "every day at 9:00AM", (*stored)[1].When)

		// Both are listed in the dialog that comes back.
		elements := refreshed(t, response)
		for _, reminder := range *stored {
			assert.Contains(t, elements, dialogElementRemove+reminder.Id)
		}
	})

	t.Run("a row that was left alone is not added", func(t *testing.T) {
		api, stored, _ := setupAPI(t, nil)
		defer api.AssertExpectations(t)

		p := newPlugin(api)
		message, when, _ := rowFields(t, p, 1)
		_, _, secondRepeat := rowFields(t, p, 2)

		status, response := submit(t, p, model.StringInterface{
			message: "standup",
			when:    "2026-11-05T14:30:00Z",
			// A row nobody touched still submits its repeat's default.
			secondRepeat: repeatOnce,
		})

		assert.Equal(t, 200, status)
		assert.Empty(t, response.Errors)
		require.Len(t, *stored, 1, "an untouched row is not a reminder")
		assert.Equal(t, "standup", (*stored)[0].Message)
	})

	t.Run("a wall clock is read in the user's zone", func(t *testing.T) {
		api, stored, _ := setupAPI(t, nil)
		defer api.AssertExpectations(t)

		p := newPlugin(api)
		message, when, _ := rowFields(t, p, 1)

		status, _ := submit(t, p, model.StringInterface{
			message: "standup",
			when:    "2026-11-05T14:30:00",
		})

		assert.Equal(t, 200, status)
		require.Len(t, *stored, 1)
		// The user's zone is UTC here, so the wall clock is the instant.
		require.Len(t, (*stored)[0].Occurrences, 1)
		assert.True(t, picked.Equal((*stored)[0].Occurrences[0].Occurrence))
	})

	t.Run("a repeating one carries its phrase for the tick to re-read", func(t *testing.T) {
		api, stored, _ := setupAPI(t, nil)
		defer api.AssertExpectations(t)

		p := newPlugin(api)
		message, when, repeat := rowFields(t, p, 1)

		status, _ := submit(t, p, model.StringInterface{
			message: "standup",
			when:    "2026-11-05T14:30:00Z",
			repeat:  repeatWeekly,
		})

		assert.Equal(t, 200, status)
		require.Len(t, *stored, 1)
		assert.Equal(t, "every thursday at 2:30PM", (*stored)[0].When)
		require.Len(t, (*stored)[0].Occurrences, 1)
		assert.Equal(t, "every thursday at 2:30PM", (*stored)[0].Occurrences[0].Repeat)
	})

	t.Run("what cannot be acted on comes back on the field it belongs to", func(t *testing.T) {
		tests := []struct {
			name string
			// fill is given the names of row one's fields, read once: every call
			// to the dialog builder names them differently.
			fill func(message, when, repeat string) model.StringInterface
			// erroredOn is the field the error is expected on.
			erroredOn func(message, when, repeat string) string
		}{
			{
				name: "a row with a time and no message",
				fill: func(_, when, _ string) model.StringInterface {
					return model.StringInterface{when: "2026-11-05T14:30:00Z"}
				},
				erroredOn: func(message, _, _ string) string { return message },
			},
			{
				name: "a row with a message and no time",
				fill: func(message, _, _ string) model.StringInterface {
					return model.StringInterface{message: "standup"}
				},
				erroredOn: func(_, when, _ string) string { return when },
			},
			{
				name: "a time that cannot be read",
				fill: func(message, when, _ string) model.StringInterface {
					return model.StringInterface{message: "standup", when: "half past two"}
				},
				erroredOn: func(_, when, _ string) string { return when },
			},
			{
				name: "a time that has passed",
				fill: func(message, when, _ string) model.StringInterface {
					return model.StringInterface{message: "standup", when: "2020-01-01T09:00:00Z"}
				},
				erroredOn: func(_, when, _ string) string { return when },
			},
			{
				name: "a repeat aimed at someone else",
				fill: func(message, when, repeat string) model.StringInterface {
					return model.StringInterface{
						message:             "standup",
						when:                "2026-11-05T14:30:00Z",
						repeat:              repeatDaily,
						dialogElementTarget: "@someone",
					}
				},
				erroredOn: func(_, _, _ string) string { return dialogElementTarget },
			},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				api, stored, _ := setupAPI(t, nil)
				defer api.AssertExpectations(t)

				p := newPlugin(api)
				message, when, repeat := rowFields(t, p, 1)
				status, response := submit(t, p, test.fill(message, when, repeat))

				// The errors have to arrive on a successful submission: the
				// client only reads the body to show them when it is one.
				assert.Equal(t, 200, status)
				assert.Contains(t, response.Errors, test.erroredOn(message, when, repeat))
				assert.Empty(t, *stored, "nothing is stored")
			})
		}
	})

	t.Run("a submission that is not JSON is refused", func(t *testing.T) {
		// Nothing is asked of the API at all: the body is refused on its own.
		api := &plugintest.API{}
		defer api.AssertExpectations(t)

		p := newPlugin(api)

		w := httptest.NewRecorder()
		p.ServeHTTP(nil, w, httptest.NewRequest("POST", "/dialog", strings.NewReader("not json")))

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	// A reminder that is still going to fire, as the dialog lists it.
	existingReminder := func(t *testing.T, message string) Reminder {
		t.Helper()

		reminder := Reminder{
			Id:        model.NewId(),
			TeamId:    model.NewId(),
			Username:  user.Username,
			Message:   message,
			Target:    "me",
			When:      "in 1 hour",
			Completed: time.Time{}.AddDate(1, 1, 1),
		}
		reminder.Occurrences = []Occurrence{{
			Hostname:   "testhost",
			Id:         model.NewId(),
			Username:   user.Username,
			ReminderId: reminder.Id,
			Occurrence: time.Now().UTC().Round(time.Second).Add(time.Hour),
		}}

		return reminder
	}

	t.Run("ticking a reminder removes it", func(t *testing.T) {
		existing := existingReminder(t, "standup")
		api, stored, _ := setupAPI(t, []Reminder{existing})
		defer api.AssertExpectations(t)

		p := newPlugin(api)
		status, response := submit(t, p, model.StringInterface{
			dialogElementRemove + existing.Id: true,
		})

		assert.Equal(t, 200, status)
		assert.Empty(t, response.Errors)
		assert.Empty(t, *stored, "the reminder is gone")
		assert.NotContains(t, refreshed(t, response), dialogElementRemove+existing.Id,
			"and gone from the list the dialog hands back")
	})

	t.Run("a tick arrives as a bool or as the word true", func(t *testing.T) {
		for _, value := range []any{true, "true", "TRUE"} {
			existing := existingReminder(t, "standup")
			api, stored, _ := setupAPI(t, []Reminder{existing})
			defer api.AssertExpectations(t)

			p := newPlugin(api)
			_, _ = submit(t, p, model.StringInterface{dialogElementRemove + existing.Id: value})

			assert.Empty(t, *stored, "ticked as %#v", value)
		}
	})

	t.Run("a box that was not ticked removes nothing", func(t *testing.T) {
		existing := existingReminder(t, "standup")

		for _, value := range []any{false, "false", ""} {
			api, stored, _ := setupAPI(t, []Reminder{existing})
			defer api.AssertExpectations(t)

			p := newPlugin(api)
			status, response := submit(t, p, model.StringInterface{
				dialogElementRemove + existing.Id: value,
			})

			// Nothing ticked and nothing filled in is a submission that asks
			// for nothing.
			assert.Equal(t, 200, status)
			assert.Contains(t, response.Error, "schedule.error.nothing")
			assert.Len(t, *stored, 1, "still there")
		}
	})

	t.Run("a reminder that is gone by the time the dialog is submitted is skipped", func(t *testing.T) {
		api, stored, _ := setupAPI(t, nil)
		defer api.AssertExpectations(t)

		p := newPlugin(api)
		status, response := submit(t, p, model.StringInterface{
			dialogElementRemove + model.NewId(): true,
		})

		assert.Equal(t, 200, status)
		assert.Empty(t, response.Errors)
		assert.Empty(t, *stored)
		assert.NotEmpty(t, refreshed(t, response), "the dialog still answers")
	})

	t.Run("one submit adds a reminder and removes another", func(t *testing.T) {
		existing := existingReminder(t, "standup")
		api, stored, _ := setupAPI(t, []Reminder{existing})
		defer api.AssertExpectations(t)

		p := newPlugin(api)
		message, when, _ := rowFields(t, p, 1)

		status, response := submit(t, p, model.StringInterface{
			dialogElementRemove + existing.Id: true,
			message:                           "propa meeting",
			when:                              "2026-11-05T14:30:00Z",
		})

		assert.Equal(t, 200, status)
		assert.Empty(t, response.Errors)
		require.Len(t, *stored, 1)
		assert.Equal(t, "propa meeting", (*stored)[0].Message)
		assert.NotEqual(t, existing.Id, (*stored)[0].Id)

		elements := refreshed(t, response)
		assert.NotContains(t, elements, dialogElementRemove+existing.Id, "the ticked one is gone")
		assert.Contains(t, elements, dialogElementRemove+(*stored)[0].Id, "the added one is listed")
	})

	t.Run("a submit that asks for nothing says so", func(t *testing.T) {
		api, stored, _ := setupAPI(t, nil)
		defer api.AssertExpectations(t)

		p := newPlugin(api)
		status, response := submit(t, p, model.StringInterface{})

		assert.Equal(t, 200, status)
		assert.Contains(t, response.Error, "schedule.error.nothing")
		assert.Empty(t, *stored)
	})
}

func TestHandleViewEphmeral(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
	}

	testTime := time.Now().UTC().Round(time.Second)

	occurrences := []Occurrence{
		{
			Id:         model.NewId(),
			ReminderId: model.NewId(),
			Occurrence: testTime,
		},
	}
	reminders := []Reminder{
		{
			Id:          model.NewId(),
			TeamId:      model.NewId(),
			Username:    user.Username,
			Message:     "Hello",
			Target:      "me",
			When:        "in one minute",
			Occurrences: occurrences,
		},
	}
	stringReminders, _ := json.Marshal(reminders)

	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("GetUser", mock.Anything).Return(user, nil)
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("KVGet", user.Username).Return(stringReminders, nil)
		api.On("CreatePost", mock.Anything).Maybe()
		api.On("SendEphemeralPost", mock.Anything, mock.Anything).Return(nil)

		return api
	}

	t.Run("view ephemeral", func(t *testing.T) {
		api := setupAPI()
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.router = p.InitAPI()
		p.API = api

		request := &model.PostActionIntegrationRequest{
			UserId: "userID1",
			PostId: "postID1",
			Context: model.StringInterface{
				"reminder_id":   model.NewId(),
				"occurrence_id": model.NewId(),
			},
		}

		w := httptest.NewRecorder()
		requestJSON, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/view/ephemeral", bytes.NewReader(requestJSON))
		p.ServeHTTP(nil, w, r)

		result := w.Result()
		assert.NotNil(t, result)

		bodyBytes, err := io.ReadAll(result.Body)
		assert.Nil(t, err)
		bodyString := string(bodyBytes)
		assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
	})
}

func TestHandleComplete(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
	}
	post := &model.Post{
		Id:        model.NewId(),
		ChannelId: model.NewId(),
	}
	testTime := time.Now().UTC().Round(time.Second)

	occurrences := []Occurrence{
		{
			Id:         model.NewId(),
			ReminderId: model.NewId(),
			Occurrence: testTime,
		},
	}
	reminders := []Reminder{
		{
			Id:          model.NewId(),
			TeamId:      model.NewId(),
			Username:    user.Username,
			Message:     "Hello",
			Target:      "me",
			When:        "in one minute",
			Occurrences: occurrences,
		},
	}
	stringReminders, _ := json.Marshal(reminders)
	channel := &model.Channel{
		Id: model.NewId(),
	}

	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("GetPost", mock.Anything).Return(post, nil)
		api.On("CreatePost", mock.Anything).Return(post, nil)
		api.On("UpdatePost", mock.Anything).Return(post, nil)
		api.On("GetUser", mock.Anything).Return(user, nil)
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("KVGet", user.Username).Return(stringReminders, nil)
		api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
		api.On("GetDirectChannel", mock.Anything, mock.Anything).Return(channel, nil)

		return api
	}

	t.Run("complete", func(t *testing.T) {
		api := setupAPI()
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.router = p.InitAPI()
		p.API = api

		request := &model.PostActionIntegrationRequest{
			UserId: "userID1",
			PostId: "postID1",
			Context: model.StringInterface{
				"orig_user_id":  "foobar",
				"reminder_id":   model.NewId(),
				"occurrence_id": model.NewId(),
			},
		}

		w := httptest.NewRecorder()
		requestJSON, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/complete", bytes.NewReader(requestJSON))
		p.ServeHTTP(nil, w, r)

		result := w.Result()
		assert.NotNil(t, result)

		bodyBytes, err := io.ReadAll(result.Body)
		assert.Nil(t, err)
		bodyString := string(bodyBytes)
		assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
	})
}

func TestHandleDelete(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
	}
	post := &model.Post{
		Id:        model.NewId(),
		ChannelId: model.NewId(),
	}
	testTime := time.Now().UTC().Round(time.Second)

	occurrences := []Occurrence{
		{
			Id:         model.NewId(),
			ReminderId: model.NewId(),
			Occurrence: testTime,
		},
	}
	reminders := []Reminder{
		{
			Id:          model.NewId(),
			TeamId:      model.NewId(),
			Username:    user.Username,
			Message:     "Hello",
			Target:      "me",
			When:        "in one minute",
			Occurrences: occurrences,
		},
	}
	stringReminders, _ := json.Marshal(reminders)

	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("GetPost", mock.Anything).Return(post, nil)
		api.On("UpdatePost", mock.Anything).Return(post, nil)
		api.On("GetUser", mock.Anything).Return(user, nil)
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("KVGet", user.Username).Return(stringReminders, nil)
		api.On("KVSet", mock.Anything, mock.Anything).Return(nil)

		return api
	}

	t.Run("delete", func(t *testing.T) {
		api := setupAPI()
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.router = p.InitAPI()
		p.API = api

		request := &model.PostActionIntegrationRequest{
			UserId: "userID1",
			PostId: "postID1",
			Context: model.StringInterface{
				"orig_user_id":  "foobar",
				"reminder_id":   model.NewId(),
				"occurrence_id": model.NewId(),
			},
		}

		w := httptest.NewRecorder()
		requestJSON, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/delete", bytes.NewReader(requestJSON))
		p.ServeHTTP(nil, w, r)

		result := w.Result()
		assert.NotNil(t, result)

		bodyBytes, err := io.ReadAll(result.Body)
		assert.Nil(t, err)
		bodyString := string(bodyBytes)
		assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
	})
}

func TestHandleDeleteEphemeral(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
	}
	post := &model.Post{
		Id:        model.NewId(),
		ChannelId: model.NewId(),
	}
	testTime := time.Now().UTC().Round(time.Second)

	occurrences := []Occurrence{
		{
			Id:         model.NewId(),
			ReminderId: model.NewId(),
			Occurrence: testTime,
		},
	}
	reminders := []Reminder{
		{
			Id:          model.NewId(),
			TeamId:      model.NewId(),
			Username:    user.Username,
			Message:     "Hello",
			Target:      "me",
			When:        "in one minute",
			Occurrences: occurrences,
		},
	}
	stringReminders, _ := json.Marshal(reminders)

	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("GetUser", mock.Anything).Return(user, nil)
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("KVGet", user.Username).Return(stringReminders, nil)
		api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
		api.On("UpdateEphemeralPost", mock.Anything, mock.Anything).Return(post)

		return api
	}

	t.Run("delete ephemeral", func(t *testing.T) {
		api := setupAPI()
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.router = p.InitAPI()
		p.API = api

		request := &model.PostActionIntegrationRequest{
			UserId: "userID1",
			PostId: "postID1",
			Context: model.StringInterface{
				"reminder_id":   model.NewId(),
				"occurrence_id": model.NewId(),
			},
		}

		w := httptest.NewRecorder()
		requestJSON, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/delete/ephemeral", bytes.NewReader(requestJSON))
		p.ServeHTTP(nil, w, r)

		result := w.Result()
		assert.NotNil(t, result)

		bodyBytes, err := io.ReadAll(result.Body)
		assert.Nil(t, err)
		bodyString := string(bodyBytes)
		assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
	})
}

func TestHandleSnooze(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
	}
	post := &model.Post{
		Id:        model.NewId(),
		ChannelId: model.NewId(),
	}
	testTime := time.Now().UTC().Round(time.Second)

	occurrences := []Occurrence{
		{
			Id:         model.NewId(),
			ReminderId: model.NewId(),
			Occurrence: testTime,
		},
	}
	reminders := []Reminder{
		{
			Id:          model.NewId(),
			TeamId:      model.NewId(),
			Username:    user.Username,
			Message:     "Hello",
			Target:      "me",
			When:        "in one minute",
			Occurrences: occurrences,
		},
	}
	stringReminders, _ := json.Marshal(reminders)
	stringOccurrences, _ := json.Marshal(occurrences)
	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("GetPost", mock.Anything).Return(post, nil)
		api.On("UpdatePost", mock.Anything).Return(post, nil)
		api.On("GetUser", mock.Anything).Return(user, nil)
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("KVGet", user.Username).Return(stringReminders, nil)
		api.On("KVGet", mock.Anything).Return(stringOccurrences, nil)
		api.On("KVSet", mock.Anything, mock.Anything).Return(nil)

		return api
	}

	for name, test := range map[string]struct {
		SnoozeTime string
	}{
		"snoozes item 20min": {
			SnoozeTime: "20min",
		},
		"snoozes item 1hr": {
			SnoozeTime: "1hr",
		},
		"snoozes item 3hrs": {
			SnoozeTime: "3hrs",
		},
		"snoozes item tomorrow": {
			SnoozeTime: "tomorrow",
		},
		"snoozes item nextweek": {
			SnoozeTime: "nextweek",
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := setupAPI()
			defer api.AssertExpectations(t)

			p := &Plugin{}
			p.router = p.InitAPI()
			p.API = api

			request := &model.PostActionIntegrationRequest{
				UserId: "userID1",
				PostId: "postID1",
				Context: model.StringInterface{
					"orig_user_id":    "foobar",
					"reminder_id":     reminders[0].Id,
					"occurrence_id":   occurrences[0].Id,
					"selected_option": test.SnoozeTime,
				},
			}

			w := httptest.NewRecorder()
			requestJSON, _ := json.Marshal(request)
			r := httptest.NewRequest("POST", "/snooze", bytes.NewReader(requestJSON))
			p.ServeHTTP(nil, w, r)

			result := w.Result()
			assert.NotNil(t, result)

			bodyBytes, err := io.ReadAll(result.Body)
			assert.Nil(t, err)
			bodyString := string(bodyBytes)
			assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
		})
	}
}

func TestHandleNextReminders(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
	}
	post := &model.Post{
		Id:        model.NewId(),
		ChannelId: model.NewId(),
	}
	testTime := time.Now().UTC().Round(time.Second)

	occurrences := []Occurrence{
		{
			Id:         model.NewId(),
			ReminderId: model.NewId(),
			Occurrence: testTime,
		},
	}
	reminders := []Reminder{
		{
			Id:          model.NewId(),
			TeamId:      model.NewId(),
			Username:    user.Username,
			Message:     "Hello",
			Target:      "me",
			When:        "in one minute",
			Occurrences: occurrences,
		},
	}
	stringReminders, _ := json.Marshal(reminders)

	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("GetUser", mock.Anything).Return(user, nil)
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("KVGet", user.Username).Return(stringReminders, nil)
		api.On("UpdateEphemeralPost", mock.Anything, mock.Anything).Return(post)

		return api
	}

	t.Run("next reminders", func(t *testing.T) {
		api := setupAPI()
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.router = p.InitAPI()
		p.API = api

		request := &model.PostActionIntegrationRequest{
			UserId: user.Id,
			PostId: post.Id,
			Context: model.StringInterface{
				"reminder_id":   model.NewId(),
				"occurrence_id": model.NewId(),
				"offset":        0,
			},
		}

		w := httptest.NewRecorder()
		requestJSON, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/next/reminders", bytes.NewReader(requestJSON))
		p.ServeHTTP(nil, w, r)

		result := w.Result()
		assert.NotNil(t, result)

		bodyBytes, err := io.ReadAll(result.Body)
		assert.Nil(t, err)
		bodyString := string(bodyBytes)
		assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
	})
}

func TestHandleCompleteList(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
	}

	post := &model.Post{
		Id:        model.NewId(),
		ChannelId: model.NewId(),
	}

	testTime := time.Now().UTC().Round(time.Second)

	occurrences := []Occurrence{
		{
			Id:         model.NewId(),
			ReminderId: model.NewId(),
			Occurrence: testTime,
		},
	}
	reminders := []Reminder{
		{
			Id:          model.NewId(),
			TeamId:      model.NewId(),
			Username:    user.Username,
			Message:     "Hello",
			Target:      "me",
			When:        "in one minute",
			Occurrences: occurrences,
		},
	}
	stringReminders, _ := json.Marshal(reminders)

	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("GetUser", mock.Anything).Return(user, nil)
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("KVGet", user.Username).Return(stringReminders, nil)
		api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
		api.On("UpdateEphemeralPost", mock.Anything, mock.Anything).Return(post)

		return api
	}

	t.Run("complete list", func(t *testing.T) {
		api := setupAPI()
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.router = p.InitAPI()
		p.API = api

		request := &model.PostActionIntegrationRequest{
			UserId: "userID1",
			PostId: "postID1",
			Context: model.StringInterface{
				"reminder_id":   model.NewId(),
				"occurrence_id": model.NewId(),
				"offset":        0,
			},
		}

		w := httptest.NewRecorder()
		requestJSON, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/complete/list", bytes.NewReader(requestJSON))
		p.ServeHTTP(nil, w, r)

		result := w.Result()
		assert.NotNil(t, result)

		bodyBytes, err := io.ReadAll(result.Body)
		assert.Nil(t, err)
		bodyString := string(bodyBytes)
		assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
	})
}

func TestHandleViewCompleteList(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
	}
	post := &model.Post{
		Id:        model.NewId(),
		ChannelId: model.NewId(),
	}
	testTime := time.Now().UTC().Round(time.Second)

	occurrences := []Occurrence{
		{
			Id:         model.NewId(),
			ReminderId: model.NewId(),
			Occurrence: testTime,
		},
	}
	reminders := []Reminder{
		{
			Id:          model.NewId(),
			TeamId:      model.NewId(),
			Username:    user.Username,
			Message:     "Hello",
			Target:      "me",
			When:        "in one minute",
			Occurrences: occurrences,
		},
	}
	stringReminders, _ := json.Marshal(reminders)

	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("GetUser", mock.Anything).Return(user, nil)
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("KVGet", user.Username).Return(stringReminders, nil)
		api.On("UpdateEphemeralPost", mock.Anything, mock.Anything).Return(post)

		return api
	}

	t.Run("view complete list", func(t *testing.T) {
		api := setupAPI()
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.router = p.InitAPI()
		p.API = api

		request := &model.PostActionIntegrationRequest{UserId: user.Id, PostId: post.Id}

		w := httptest.NewRecorder()
		requestJSON, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/view/complete/list", bytes.NewReader(requestJSON))
		p.ServeHTTP(nil, w, r)

		result := w.Result()
		assert.NotNil(t, result)

		bodyBytes, err := io.ReadAll(result.Body)
		assert.Nil(t, err)
		bodyString := string(bodyBytes)
		assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
	})
}

func TestHandleDeleteList(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
	}
	post := &model.Post{
		Id:        model.NewId(),
		ChannelId: model.NewId(),
	}
	testTime := time.Now().UTC().Round(time.Second)

	occurrences := []Occurrence{
		{
			Id:         model.NewId(),
			ReminderId: model.NewId(),
			Occurrence: testTime,
		},
	}
	reminders := []Reminder{
		{
			Id:          model.NewId(),
			TeamId:      model.NewId(),
			Username:    user.Username,
			Message:     "Hello",
			Target:      "me",
			When:        "in one minute",
			Occurrences: occurrences,
		},
	}
	stringReminders, _ := json.Marshal(reminders)

	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("GetUser", mock.Anything).Return(user, nil)
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("KVGet", user.Username).Return(stringReminders, nil)
		api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
		api.On("UpdateEphemeralPost", mock.Anything, mock.Anything).Return(post)

		return api
	}

	t.Run("delete list item", func(t *testing.T) {
		api := setupAPI()
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.router = p.InitAPI()
		p.API = api

		request := &model.PostActionIntegrationRequest{
			UserId: "userID1",
			PostId: "postID1",
			Context: model.StringInterface{
				"reminder_id":   model.NewId(),
				"occurrence_id": model.NewId(),
				"offset":        0,
			},
		}

		w := httptest.NewRecorder()
		requestJSON, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/delete/list", bytes.NewReader(requestJSON))
		p.ServeHTTP(nil, w, r)

		result := w.Result()
		assert.NotNil(t, result)

		bodyBytes, err := io.ReadAll(result.Body)
		assert.Nil(t, err)
		bodyString := string(bodyBytes)
		assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
	})
}

func TestHandleDeleteCompleteList(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
	}
	post := &model.Post{
		Id:        model.NewId(),
		ChannelId: model.NewId(),
	}
	testTime := time.Now().UTC().Round(time.Second)

	occurrences := []Occurrence{
		{
			Id:         model.NewId(),
			ReminderId: model.NewId(),
			Occurrence: testTime,
		},
	}
	reminders := []Reminder{
		{
			Id:          model.NewId(),
			TeamId:      model.NewId(),
			Username:    user.Username,
			Message:     "Hello",
			Target:      "me",
			When:        "in one minute",
			Occurrences: occurrences,
		},
	}
	stringReminders, _ := json.Marshal(reminders)

	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("GetUser", mock.Anything).Return(user, nil)
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("UpdateEphemeralPost", mock.Anything, mock.Anything).Return(post)
		api.On("KVGet", user.Username).Return(stringReminders, nil)

		return api
	}

	t.Run("delete completed list", func(t *testing.T) {
		api := setupAPI()
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.router = p.InitAPI()
		p.API = api

		request := &model.PostActionIntegrationRequest{
			UserId: "userID1",
			PostId: "postID1",
			Context: model.StringInterface{
				"reminder_id":   model.NewId(),
				"occurrence_id": model.NewId(),
				"offset":        0,
			},
		}

		w := httptest.NewRecorder()
		requestJSON, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/delete/complete/list", bytes.NewReader(requestJSON))
		p.ServeHTTP(nil, w, r)

		result := w.Result()
		assert.NotNil(t, result)

		bodyBytes, err := io.ReadAll(result.Body)
		assert.Nil(t, err)
		bodyString := string(bodyBytes)
		assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
	})
}

func TestHandleSnoozeList(t *testing.T) {
	user := &model.User{
		Id:       model.NewId(),
		Username: model.NewRandomString(10),
	}
	post := &model.Post{
		Id:        model.NewId(),
		ChannelId: model.NewId(),
	}
	testTime := time.Now().UTC().Round(time.Second)

	occurrences := []Occurrence{
		{
			Id:         model.NewId(),
			ReminderId: model.NewId(),
			Occurrence: testTime,
		},
	}
	reminders := []Reminder{
		{
			Id:          model.NewId(),
			TeamId:      model.NewId(),
			Username:    user.Username,
			Message:     "Hello",
			Target:      "me",
			When:        "in one minute",
			Occurrences: occurrences,
		},
	}
	stringReminders, _ := json.Marshal(reminders)
	stringOccurrences, _ := json.Marshal(occurrences)
	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("GetUser", mock.Anything).Return(user, nil)
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		api.On("KVGet", user.Username).Return(stringReminders, nil)
		api.On("KVGet", mock.Anything).Return(stringOccurrences, nil)
		api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
		api.On("UpdateEphemeralPost", mock.Anything, mock.Anything).Return(post)

		return api
	}

	for name, test := range map[string]struct {
		SnoozeTime string
	}{
		"snoozes list item 20min": {
			SnoozeTime: "20min",
		},
		"snoozes list item 1hr": {
			SnoozeTime: "1hr",
		},
		"snoozes list item 3hrs": {
			SnoozeTime: "3hrs",
		},
		"snoozes list item tomorrow": {
			SnoozeTime: "tomorrow",
		},
		"snoozes list item nextweek": {
			SnoozeTime: "nextweek",
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := setupAPI()
			defer api.AssertExpectations(t)

			p := &Plugin{}
			p.router = p.InitAPI()
			p.API = api

			request := &model.PostActionIntegrationRequest{
				UserId: "userID1",
				PostId: "postID1",
				Context: model.StringInterface{
					"reminder_id":     reminders[0].Id,
					"occurrence_id":   occurrences[0].Id,
					"selected_option": test.SnoozeTime,
					"offset":          0,
				},
			}

			w := httptest.NewRecorder()
			requestJSON, _ := json.Marshal(request)
			r := httptest.NewRequest("POST", "/snooze/list", bytes.NewReader(requestJSON))
			p.ServeHTTP(nil, w, r)

			result := w.Result()
			assert.NotNil(t, result)

			bodyBytes, err := io.ReadAll(result.Body)
			assert.Nil(t, err)
			bodyString := string(bodyBytes)
			assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
		})
	}
}

func TestHandleCloseList(t *testing.T) {
	setupAPI := func() *plugintest.API {
		api := &plugintest.API{}
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogInfo", mock.Anything).Maybe()
		api.On("DeleteEphemeralPost", mock.Anything, mock.Anything).Return(nil)
		return api
	}

	t.Run("closes list", func(t *testing.T) {
		api := setupAPI()
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.router = p.InitAPI()
		p.API = api

		request := &model.PostActionIntegrationRequest{UserId: "userID1", PostId: "postID1"}

		w := httptest.NewRecorder()
		requestJSON, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/close/list", bytes.NewReader(requestJSON))
		p.ServeHTTP(nil, w, r)

		result := w.Result()
		assert.NotNil(t, result)

		bodyBytes, err := io.ReadAll(result.Body)
		assert.Nil(t, err)
		bodyString := string(bodyBytes)
		assert.Equal(t, bodyString, "{\"update\":null,\"ephemeral_text\":\"\",\"skip_slack_parsing\":false}")
	})
}

// A one-off is completed when the tick delivers it, so snoozing one has to open
// it again: leaving it completed would keep it out of the list while the snooze
// is pending, and clearing the completed reminders would take it with them, with
// the snooze still to come.
func TestHandleSnoozeReopensACompletedReminder(t *testing.T) {
	user := &model.User{Id: model.NewId(), Username: model.NewRandomString(10), Locale: "en"}
	post := &model.Post{Id: model.NewId(), ChannelId: model.NewId()}

	occurrence := Occurrence{
		Id:         model.NewId(),
		ReminderId: model.NewId(),
		Username:   user.Username,
		Occurrence: time.Now().UTC().Round(time.Second),
	}
	reminder := Reminder{
		Id:          occurrence.ReminderId,
		TeamId:      model.NewId(),
		Username:    user.Username,
		Message:     "Hello",
		Target:      "me",
		When:        "in one minute",
		Completed:   time.Now().UTC().Add(-time.Minute),
		Occurrences: []Occurrence{occurrence},
	}

	var stored []Reminder

	api := &plugintest.API{}
	api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
	api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
	api.On("LogInfo", mock.Anything).Maybe()
	api.On("GetPost", mock.Anything).Return(post, nil)
	api.On("UpdatePost", mock.Anything).Return(post, nil)
	api.On("GetUser", mock.Anything).Return(user, nil)
	api.On("GetUserByUsername", mock.Anything).Return(user, nil)
	api.On("KVGet", user.Username).Return(mustMarshal(t, []Reminder{reminder}), nil)
	api.On("KVGet", mock.Anything).Return(nil, nil).Maybe()
	api.On("KVSet", user.Username, mock.Anything).Run(func(args mock.Arguments) {
		_ = json.Unmarshal(args.Get(1).([]byte), &stored)
	}).Return(nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
	defer api.AssertExpectations(t)

	p := &Plugin{}
	p.router = p.InitAPI()
	p.API = api
	p.emptyTime = time.Time{}.AddDate(1, 1, 1)

	request := &model.PostActionIntegrationRequest{
		UserId: user.Id,
		PostId: post.Id,
		Context: model.StringInterface{
			"orig_user_id":    user.Id,
			"reminder_id":     reminder.Id,
			"occurrence_id":   occurrence.Id,
			"selected_option": "20min",
		},
	}

	w := httptest.NewRecorder()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	p.ServeHTTP(nil, w, httptest.NewRequest("POST", "/snooze", bytes.NewReader(body)))

	require.Len(t, stored, 1)
	assert.Equal(t, p.emptyTime, stored[0].Completed, "the snooze opened it again")
	require.Len(t, stored[0].Occurrences, 1)
	assert.False(t, stored[0].Occurrences[0].Snoozed.IsZero(), "and filed it under the snooze")
}
