package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Reminders and occurrences are stored in the plugin's KV store as JSON, and
// neither struct carries json tags, so the Go field names are the stored keys.
// Renaming a field — Id to ID, say — would silently decode every reminder
// already stored as zero values, and the next write would persist the loss.
//
// This test fails if that happens, so the rename has to come with json tags and
// a migration for what is already stored.
func TestStoredReminderFormat(t *testing.T) {
	stored := `[{
		"TeamId": "team-id",
		"Id": "reminder-id",
		"Username": "testuser",
		"Target": "@someone",
		"Message": "speak up",
		"When": "in 1 hour",
		"Occurrences": [{
			"Hostname": "hostname",
			"Id": "occurrence-id",
			"Username": "testuser",
			"ReminderId": "reminder-id",
			"Repeat": "every monday",
			"Occurrence": "2026-10-12T09:00:00Z",
			"Snoozed": "2026-10-12T09:05:00Z"
		}],
		"Completed": "2026-10-12T09:00:00Z"
	}]`

	var reminders []Reminder
	require.NoError(t, json.Unmarshal([]byte(stored), &reminders))
	require.Len(t, reminders, 1)

	reminder := reminders[0]
	require.Equal(t, "team-id", reminder.TeamId)
	require.Equal(t, "reminder-id", reminder.Id)
	require.Equal(t, "testuser", reminder.Username)
	require.Equal(t, "@someone", reminder.Target)
	require.Equal(t, "speak up", reminder.Message)
	require.Equal(t, "in 1 hour", reminder.When)
	require.Equal(t, time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), reminder.Completed)

	require.Len(t, reminder.Occurrences, 1)
	occurrence := reminder.Occurrences[0]
	require.Equal(t, "hostname", occurrence.Hostname)
	require.Equal(t, "occurrence-id", occurrence.Id)
	require.Equal(t, "reminder-id", occurrence.ReminderId)
	require.Equal(t, "every monday", occurrence.Repeat)
	require.Equal(t, time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), occurrence.Occurrence)
	require.Equal(t, time.Date(2026, 10, 12, 9, 5, 0, 0, time.UTC), occurrence.Snoozed)

	// And what is written back has to keep the same keys.
	written, err := json.Marshal(reminders)
	require.NoError(t, err)
	for _, key := range []string{
		"TeamId", "Id", "Username", "Target", "Message", "When", "Occurrences", "Completed",
		"Hostname", "ReminderId", "Repeat", "Occurrence", "Snoozed",
	} {
		require.Contains(t, string(written), `"`+key+`":`, "stored key %s changed", key)
	}
}
