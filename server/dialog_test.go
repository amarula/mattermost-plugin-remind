package main

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func dialogTestUser(zone string) *model.User {
	user := &model.User{
		Id:       model.NewId(),
		Email:    "-@-.-",
		Nickname: "TestUser",
		Username: "testuser",
		Roles:    model.SystemUserRoleId,
		Locale:   "en",
	}
	if zone != "" {
		user.Timezone = map[string]string{"manualTimezone": zone}
	}

	return user
}

func TestComposeWhen(t *testing.T) {
	user := dialogTestUser("UTC")
	// A Thursday, so the weekly phrase has a weekday to name.
	picked := time.Date(2026, time.November, 5, 14, 30, 0, 0, time.UTC)

	p := &Plugin{}

	tests := []struct {
		repeat string
		want   string
	}{
		{repeatOnce, "on 2026-11-05 at 2:30PM"},
		{repeatDaily, "every day at 2:30PM"},
		{repeatWeekdays, "every weekdays at 2:30PM"},
		{repeatWeekly, "every thursday at 2:30PM"},
		{repeatMonthly, "every 5th at 2:30PM"},
		{"", "on 2026-11-05 at 2:30PM"},
	}

	for _, test := range tests {
		t.Run(test.repeat, func(t *testing.T) {
			assert.Equal(t, test.want, p.composeWhen(user, picked, test.repeat))
		})
	}
}

// The composed phrase is only worth anything if the reminder engine resolves it,
// so each choice is fed back through the parser it is meant for.
func TestComposeWhenIsUnderstoodByTheEngine(t *testing.T) {
	user := dialogTestUser("UTC")
	picked := time.Date(2026, time.November, 5, 14, 30, 0, 0, time.UTC)

	setup := func(t *testing.T) *Plugin {
		t.Helper()

		api := &plugintest.API{}
		api.On("GetUserByUsername", mock.Anything).Return(user, nil)
		// Filing an occurrence writes to the store; parsing a phrase back does
		// not, and the same setup serves both.
		api.On("KVGet", mock.Anything).Return(nil, nil).Maybe()
		api.On("KVSet", mock.Anything, mock.Anything).Return(nil).Maybe()
		api.On("LogDebug", mock.Anything).Maybe()
		api.On("LogError", mock.Anything, mock.Anything).Maybe()
		t.Cleanup(func() { api.AssertExpectations(t) })

		p := &Plugin{}
		p.API = api
		p.emptyTime = time.Time{}.AddDate(1, 1, 1)
		p.locales = map[string]string{"en": "/test/en.json"}

		return p
	}

	// Only a one-off starts at the instant that was picked; a repeat starts at
	// the next time the phrase matches, which is why the repeat help text says so.
	create := func(t *testing.T, repeat string) []Occurrence {
		t.Helper()

		p := setup(t)

		request := &ReminderRequest{
			Username: user.Username,
			Payload:  "standup",
			Reminder: Reminder{
				Id:        model.NewId(),
				Username:  user.Username,
				Message:   "standup",
				Target:    "me",
				Completed: p.emptyTime,
				When:      p.composeWhen(user, picked, repeat),
			},
		}

		require.NoError(t, p.CreateOccurrences(request))

		return request.Reminder.Occurrences
	}

	t.Run("once lands on the picked instant", func(t *testing.T) {
		occurrences := create(t, repeatOnce)

		require.Len(t, occurrences, 1)
		assert.True(t, picked.Equal(occurrences[0].Occurrence), "got %v", occurrences[0].Occurrence)
		assert.Empty(t, occurrences[0].Repeat)
	})

	t.Run("daily starts at the picked time of day", func(t *testing.T) {
		occurrences := create(t, repeatDaily)

		require.Len(t, occurrences, 1)
		assert.Equal(t, 14, occurrences[0].Occurrence.In(time.UTC).Hour())
		assert.Equal(t, 30, occurrences[0].Occurrence.In(time.UTC).Minute())
		assert.True(t, occurrences[0].Occurrence.After(time.Now()), "a repeat has to start in the future")
		assert.Equal(t, occurrences[0].Repeat, "every day at 2:30PM")
	})

	t.Run("weekdays covers monday to friday", func(t *testing.T) {
		occurrences := create(t, repeatWeekdays)

		require.Len(t, occurrences, 5)
		for _, occurrence := range occurrences {
			weekday := occurrence.Occurrence.In(time.UTC).Weekday()
			assert.NotEqual(t, time.Saturday, weekday)
			assert.NotEqual(t, time.Sunday, weekday)
			assert.Equal(t, 14, occurrence.Occurrence.In(time.UTC).Hour())
		}
	})

	t.Run("weekly keeps the picked weekday and time, and can be rescheduled", func(t *testing.T) {
		occurrences := create(t, repeatWeekly)

		require.Len(t, occurrences, 1)
		// The picked date is not the first occurrence: a repeat starts at the
		// next time the phrase matches, which is what the help text says.
		assert.Equal(t, time.Thursday, occurrences[0].Occurrence.In(time.UTC).Weekday())
		assert.Equal(t, 14, occurrences[0].Occurrence.In(time.UTC).Hour())
		assert.True(t, occurrences[0].Occurrence.After(time.Now()))

		// The tick re-parses the phrase once the occurrence has fired, so the
		// phrase has to resolve to a later time every time it is read.
		fired := occurrences[0]
		fired.Occurrence = fired.Occurrence.AddDate(0, 0, -7)

		p := setup(t)
		next, err := p.rescheduleOccurrenceEN(&fired)
		require.NoError(t, err)
		require.NotEmpty(t, next)
		assert.True(t, next[0].After(fired.Occurrence), "got %v after %v", next[0], fired.Occurrence)
		assert.True(t, next[0].After(time.Now()))
		assert.Equal(t, time.Thursday, next[0].In(time.UTC).Weekday())
	})

	t.Run("monthly keeps the picked day of the month and rolls forward", func(t *testing.T) {
		occurrences := create(t, repeatMonthly)

		require.Len(t, occurrences, 1)
		assert.Equal(t, 5, occurrences[0].Occurrence.In(time.UTC).Day())
		assert.Equal(t, 14, occurrences[0].Occurrence.In(time.UTC).Hour())

		fired := occurrences[0]
		fired.Occurrence = fired.Occurrence.AddDate(0, -1, 0)

		p := setup(t)
		next, err := p.rescheduleOccurrenceEN(&fired)
		require.NoError(t, err)
		require.NotEmpty(t, next)
		assert.Equal(t, 5, next[0].In(time.UTC).Day())
		assert.True(t, next[0].After(time.Now()))
	})
}

func TestParseDialogDateTime(t *testing.T) {
	denver, err := time.LoadLocation("America/Denver")
	require.NoError(t, err)

	// 5 November 2026 is outside daylight saving in Denver, so it is UTC-7.
	tests := []struct {
		name  string
		value string
		zone  *time.Location
		want  time.Time
	}{
		{
			name:  "an instant in UTC",
			value: "2026-11-05T14:30:00Z",
			zone:  time.UTC,
			want:  time.Date(2026, time.November, 5, 14, 30, 0, 0, time.UTC),
		},
		{
			name:  "an instant with an offset",
			value: "2026-11-05T14:30:00-07:00",
			zone:  time.UTC,
			want:  time.Date(2026, time.November, 5, 21, 30, 0, 0, time.UTC),
		},
		{
			name:  "an instant with fractional seconds",
			value: "2026-11-05T14:30:00.500Z",
			zone:  time.UTC,
			want:  time.Date(2026, time.November, 5, 14, 30, 0, 500000000, time.UTC),
		},
		{
			name:  "a wall clock, read in the user's zone",
			value: "2026-11-05T14:30:00",
			zone:  denver,
			want:  time.Date(2026, time.November, 5, 21, 30, 0, 0, time.UTC),
		},
		{
			name:  "a wall clock without seconds, read in the user's zone",
			value: "2026-11-05T14:30",
			zone:  denver,
			want:  time.Date(2026, time.November, 5, 21, 30, 0, 0, time.UTC),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, parseErr := parseDialogDateTime(test.value, test.zone)
			require.NoError(t, parseErr)
			assert.True(t, test.want.Equal(parsed), "want %v, got %v", test.want, parsed)
		})
	}

	t.Run("it refuses what it cannot read", func(t *testing.T) {
		for _, value := range []string{"", "garbage", "2026-11-05", "half past two"} {
			_, parseErr := parseDialogDateTime(value, time.UTC)
			assert.Error(t, parseErr, "value %q", value)
		}
	})
}
