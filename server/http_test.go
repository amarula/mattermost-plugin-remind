package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

	submit := func(t *testing.T, api *plugintest.API, submission model.StringInterface) (int, model.SubmitDialogResponse) {
		t.Helper()

		p := &Plugin{}
		p.API = api
		p.router = p.InitAPI()
		p.emptyTime = time.Time{}.AddDate(1, 1, 1)
		p.locales = map[string]string{"en": "/test/en.json"}

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

	t.Run("a one-off is stored at the instant that was picked", func(t *testing.T) {
		api, stored, posted := setupAPI(t, nil)
		defer api.AssertExpectations(t)

		status, response := submit(t, api, model.StringInterface{
			dialogElementMessage: "standup",
			dialogElementWhen:    "2026-11-05T14:30:00Z",
			dialogElementRepeat:  repeatOnce,
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

		assert.Len(t, *posted, 1, "the confirmation post")
	})

	t.Run("a wall clock is read in the user's zone", func(t *testing.T) {
		api, stored, _ := setupAPI(t, nil)
		defer api.AssertExpectations(t)

		status, _ := submit(t, api, model.StringInterface{
			dialogElementMessage: "standup",
			dialogElementWhen:    "2026-11-05T14:30:00",
			dialogElementRepeat:  repeatOnce,
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

		status, _ := submit(t, api, model.StringInterface{
			dialogElementMessage: "standup",
			dialogElementWhen:    "2026-11-05T14:30:00Z",
			dialogElementRepeat:  repeatWeekly,
		})

		assert.Equal(t, 200, status)
		require.Len(t, *stored, 1)
		assert.Equal(t, "every thursday at 2:30PM", (*stored)[0].When)
		require.Len(t, (*stored)[0].Occurrences, 1)
		assert.Equal(t, "every thursday at 2:30PM", (*stored)[0].Occurrences[0].Repeat)
	})

	t.Run("what cannot be acted on comes back on the field it belongs to", func(t *testing.T) {
		tests := []struct {
			name       string
			submission model.StringInterface
			field      string
		}{
			{
				name:       "no message",
				submission: model.StringInterface{dialogElementWhen: "2026-11-05T14:30:00Z"},
				field:      dialogElementMessage,
			},
			{
				name:       "no time",
				submission: model.StringInterface{dialogElementMessage: "standup"},
				field:      dialogElementWhen,
			},
			{
				name: "a time that cannot be read",
				submission: model.StringInterface{
					dialogElementMessage: "standup",
					dialogElementWhen:    "half past two",
				},
				field: dialogElementWhen,
			},
			{
				name: "a time that has passed",
				submission: model.StringInterface{
					dialogElementMessage: "standup",
					dialogElementWhen:    "2020-01-01T09:00:00Z",
				},
				field: dialogElementWhen,
			},
			{
				name: "a repeat aimed at someone else",
				submission: model.StringInterface{
					dialogElementMessage: "standup",
					dialogElementWhen:    "2026-11-05T14:30:00Z",
					dialogElementRepeat:  repeatDaily,
					dialogElementTarget:  "@someone",
				},
				field: dialogElementTarget,
			},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				api, stored, posted := setupAPI(t, nil)
				defer api.AssertExpectations(t)

				status, response := submit(t, api, test.submission)

				// The errors have to arrive on a successful submission: the
				// client only reads the body to show them when it is one.
				assert.Equal(t, 200, status)
				assert.Contains(t, response.Errors, test.field)
				assert.Empty(t, *stored, "nothing is stored")
				assert.Empty(t, *posted, "and nothing is posted")
			})
		}
	})

	t.Run("a submission that is not JSON is refused", func(t *testing.T) {
		// Nothing is asked of the API at all: the body is refused on its own.
		api := &plugintest.API{}
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.API = api
		p.router = p.InitAPI()

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
		api, stored, posted := setupAPI(t, []Reminder{existing})
		defer api.AssertExpectations(t)

		status, response := submit(t, api, model.StringInterface{
			dialogElementRemove + existing.Id: true,
		})

		assert.Equal(t, 200, status)
		assert.Empty(t, response.Errors)
		assert.Empty(t, *stored, "the reminder is gone")
		// The reply is the list, which is where the remaining buttons live.
		require.Len(t, *posted, 1)
	})

	t.Run("a tick arrives as a bool or as the word true", func(t *testing.T) {
		for _, value := range []any{true, "true", "TRUE"} {
			existing := existingReminder(t, "standup")
			api, stored, _ := setupAPI(t, []Reminder{existing})
			defer api.AssertExpectations(t)

			_, _ = submit(t, api, model.StringInterface{dialogElementRemove + existing.Id: value})

			assert.Empty(t, *stored, "ticked as %#v", value)
		}
	})

	t.Run("a box that was not ticked removes nothing", func(t *testing.T) {
		existing := existingReminder(t, "standup")

		for _, value := range []any{false, "false", ""} {
			api, stored, posted := setupAPI(t, []Reminder{existing})
			defer api.AssertExpectations(t)

			status, response := submit(t, api, model.StringInterface{
				dialogElementRemove + existing.Id: value,
			})

			// Nothing ticked and nothing filled in is a submission that asks
			// for nothing.
			assert.Equal(t, 200, status)
			assert.Contains(t, response.Errors, dialogElementMessage)
			assert.Len(t, *stored, 1, "still there")
			assert.Empty(t, *posted)
		}
	})

	t.Run("a reminder that is gone by the time the dialog is submitted is skipped", func(t *testing.T) {
		api, stored, posted := setupAPI(t, nil)
		defer api.AssertExpectations(t)

		status, response := submit(t, api, model.StringInterface{
			dialogElementRemove + model.NewId(): true,
		})

		assert.Equal(t, 200, status)
		assert.Empty(t, response.Errors)
		assert.Empty(t, *stored)
		require.Len(t, *posted, 1, "the list still answers")
	})

	t.Run("one submit adds one reminder and removes another", func(t *testing.T) {
		existing := existingReminder(t, "standup")
		api, stored, posted := setupAPI(t, []Reminder{existing})
		defer api.AssertExpectations(t)

		status, response := submit(t, api, model.StringInterface{
			dialogElementRemove + existing.Id: true,
			dialogElementMessage:              "propa meeting",
			dialogElementWhen:                 "2026-11-05T14:30:00Z",
			dialogElementRepeat:               repeatOnce,
		})

		assert.Equal(t, 200, status)
		assert.Empty(t, response.Errors)
		require.Len(t, *stored, 1)
		assert.Equal(t, "propa meeting", (*stored)[0].Message)
		assert.NotEqual(t, existing.Id, (*stored)[0].Id)
		require.Len(t, *posted, 1, "one reply: the list")
	})

	t.Run("a submit that asks for nothing says so", func(t *testing.T) {
		api, stored, posted := setupAPI(t, nil)
		defer api.AssertExpectations(t)

		status, response := submit(t, api, model.StringInterface{})

		assert.Equal(t, 200, status)
		assert.Contains(t, response.Errors, dialogElementMessage)
		assert.Contains(t, response.Errors[dialogElementMessage], "schedule.error.nothing")
		assert.Empty(t, *stored)
		assert.Empty(t, *posted)
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
