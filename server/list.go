package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

const RemindersPerPage = 4

func (p *Plugin) ListReminders(user *model.User, channelID string) *model.Post {
	T, _ := p.translation(user)
	offset := 0
	reminders := p.GetReminders(user.Username)
	if len(reminders) == 0 {
		return &model.Post{
			ChannelId:     channelID,
			PendingPostId: model.NewId() + ":" + fmt.Sprint(model.GetMillis()),
			UserId:        p.botUserID,
			Message:       T("no.reminders"),
		}
	}
	completedReminderCount := 0
	for _, reminder := range reminders {
		if reminder.Completed != p.emptyTime {
			completedReminderCount++
		}
	}
	endOffset := offset + RemindersPerPage - 1
	activeReminderCount := len(reminders) - completedReminderCount
	if endOffset >= activeReminderCount {
		endOffset = activeReminderCount - 1
	}

	upcomingOccurrences,
		recurringOccurrences,
		pastOccurrences,
		channelOccurrences := p.categorizeOccurrences(reminders)

	attachments := p.pagedOccurrences(
		user,
		reminders,
		upcomingOccurrences,
		recurringOccurrences,
		pastOccurrences,
		channelOccurrences,
		offset,
		endOffset)

	attachments = p.listControl(
		user,
		activeReminderCount,
		completedReminderCount,
		offset,
		endOffset,
		attachments)

	return &model.Post{
		ChannelId:     channelID,
		PendingPostId: model.NewId() + ":" + fmt.Sprint(model.GetMillis()),
		UserId:        p.botUserID,
		Props: model.StringInterface{
			"attachments": attachments,
		},
	}
}

func (p *Plugin) UpdateListReminders(userID string, postID string, channelID string, offset int) {
	user, uErr := p.API.GetUser(userID)
	if uErr != nil {
		p.API.LogError(uErr.Error())
	}

	reminders := p.GetReminders(user.Username)
	completedReminderCount := 0
	for _, reminder := range reminders {
		if reminder.Completed != p.emptyTime {
			completedReminderCount++
		}
	}
	activeReminderCount := len(reminders) - completedReminderCount
	if offset < 0 {
		offset = 0
	} else if offset >= activeReminderCount {
		offset = 0
	}
	endOffset := offset + RemindersPerPage - 1
	if endOffset >= activeReminderCount {
		endOffset = activeReminderCount - 1
	}

	upcomingOccurrences,
		recurringOccurrences,
		pastOccurrences,
		channelOccurrences := p.categorizeOccurrences(reminders)

	attachments := p.pagedOccurrences(
		user,
		reminders,
		upcomingOccurrences,
		recurringOccurrences,
		pastOccurrences,
		channelOccurrences,
		offset,
		endOffset)

	attachments = p.listControl(
		user,
		activeReminderCount,
		completedReminderCount,
		offset,
		endOffset,
		attachments)

	post := &model.Post{
		Id:        postID,
		ChannelId: channelID,
		UserId:    p.botUserID,
		Props: model.StringInterface{
			"attachments": attachments,
		},
	}
	p.API.UpdateEphemeralPost(userID, post)
}

func (p *Plugin) categorizeOccurrences(reminders []Reminder) (
	upcomingOccurrences []Occurrence,
	recurringOccurrences []Occurrence,
	pastOccurrences []Occurrence,
	channelOccurrences []Occurrence) {
	for _, reminder := range reminders {
		occurrences := reminder.Occurrences

		if len(occurrences) > 0 {
			occurrence := occurrences[0]
			t := occurrence.Occurrence
			s := occurrence.Snoozed

			isChannelReminder := strings.HasPrefix(reminder.Target, "~")
			isCompleted := reminder.Completed != p.emptyTime
			isPast := t.Before(time.Now().UTC())
			isFuture := t.After(time.Now().UTC())

			if !isChannelReminder &&
				!isCompleted &&
				((occurrence.Repeat == "" && isFuture) ||
					(s != p.emptyTime && s.After(time.Now().UTC()))) {
				upcomingOccurrences = append(upcomingOccurrences, occurrence)
			} else if !isChannelReminder &&
				occurrence.Repeat != "" && isFuture {
				recurringOccurrences = append(recurringOccurrences, occurrence)
			} else if !isCompleted &&
				isPast &&
				(s.Equal(p.emptyTime) || s.Before(time.Now().UTC())) {
				pastOccurrences = append(pastOccurrences, occurrence)
			} else if isChannelReminder &&
				!isCompleted &&
				isFuture {
				channelOccurrences = append(channelOccurrences, occurrence)
			} /* else if isCompleted {
				p.API.LogInfo("completed reminder: " + fmt.Sprintf("%v", reminder))
			} else {
				p.API.LogInfo("unknown reminder: " + fmt.Sprintf("%v", reminder))
			} */
		}
	}

	return upcomingOccurrences, recurringOccurrences, pastOccurrences, channelOccurrences
}

func (p *Plugin) pagedOccurrences(
	user *model.User,
	reminders []Reminder,
	upcomingOccurrences []Occurrence,
	recurringOccurrences []Occurrence,
	pastOccurrences []Occurrence,
	channelOccurrences []Occurrence,
	offset int,
	endOffset int) (attachments []*model.MessageAttachment) {
	offsetCount := 0

	if len(upcomingOccurrences) > 0 {
		for _, o := range upcomingOccurrences {
			if offsetCount >= offset && offsetCount <= endOffset {
				attachments = append(attachments, p.addAttachment(user, o, reminders, "upcoming", offset))
			}
			offsetCount++
		}
	}

	if len(recurringOccurrences) > 0 {
		for _, o := range recurringOccurrences {
			if offsetCount >= offset && offsetCount <= endOffset {
				attachments = append(attachments, p.addAttachment(user, o, reminders, "recurring", offset))
			}
			offsetCount++
		}
	}

	if len(pastOccurrences) > 0 {
		for _, o := range pastOccurrences {
			if offsetCount >= offset && offsetCount <= endOffset {
				attachments = append(attachments, p.addAttachment(user, o, reminders, "past", offset))
			}
			offsetCount++
		}
	}
	if len(channelOccurrences) > 0 {
		for _, o := range channelOccurrences {
			if offsetCount >= offset && offsetCount <= endOffset {
				attachments = append(attachments, p.addAttachment(user, o, reminders, "channel", offset))
			}
			offsetCount++
		}
	}

	return attachments
}

func (p *Plugin) listControl(
	user *model.User,
	activeReminderCount int,
	completedReminderCount int,
	offset int,
	endOffset int,
	attachments []*model.MessageAttachment) []*model.MessageAttachment {
	T, _ := p.translation(user)

	reminderCount := map[string]interface{}{
		"ReminderCount": RemindersPerPage,
	}

	start := offset + 1
	stop := endOffset + 1
	if activeReminderCount == 0 {
		start = 0
		stop = 0
	}
	remindersPageCount := map[string]interface{}{
		"Start": start,
		"Stop":  stop,
		"Total": activeReminderCount,
	}

	actions := []*model.PostAction{}

	if activeReminderCount > RemindersPerPage {
		if offset == 0 {
			actions = append(actions,
				&model.PostAction{
					Integration: &model.PostActionIntegration{
						Context: model.StringInterface{
							"action": "next/reminders",
							"offset": endOffset + 1,
						},
						URL: fmt.Sprintf("/plugins/%s/next/reminders", PluginID),
					},
					Type: model.PostActionTypeButton,
					Name: T("button.next.reminders", reminderCount),
				})
		} else if offset >= activeReminderCount-RemindersPerPage {
			actions = append(actions,
				&model.PostAction{
					Integration: &model.PostActionIntegration{
						Context: model.StringInterface{
							"action": "previous/reminders",
							"offset": offset - RemindersPerPage,
						},
						URL: fmt.Sprintf("/plugins/%s/next/reminders", PluginID),
					},
					Type: model.PostActionTypeButton,
					Name: T("button.previous.reminders", reminderCount),
				})
		} else {
			actions = append(actions,
				&model.PostAction{
					Integration: &model.PostActionIntegration{
						Context: model.StringInterface{
							"action": "previous/reminders",
							"offset": offset - RemindersPerPage,
						},
						URL: fmt.Sprintf("/plugins/%s/next/reminders", PluginID),
					},
					Type: model.PostActionTypeButton,
					Name: T("button.previous.reminders", reminderCount),
				})
			actions = append(actions,
				&model.PostAction{
					Integration: &model.PostActionIntegration{
						Context: model.StringInterface{
							"action": "next/reminders",
							"offset": endOffset + 1,
						},
						URL: fmt.Sprintf("/plugins/%s/next/reminders", PluginID),
					},
					Type: model.PostActionTypeButton,
					Name: T("button.next.reminders", reminderCount),
				})
		}
	}

	if completedReminderCount > 0 {
		actions = append(actions,
			&model.PostAction{
				Integration: &model.PostActionIntegration{
					Context: model.StringInterface{
						"action": "view/complete/list",
					},
					URL: fmt.Sprintf("/plugins/%s/view/complete/list", PluginID),
				},
				Type: model.PostActionTypeButton,
				Name: T("button.view.complete"),
			})

		actions = append(actions,
			&model.PostAction{
				Integration: &model.PostActionIntegration{
					Context: model.StringInterface{
						"action": "delete/complete/list",
						"offset": offset,
					},
					URL: fmt.Sprintf("/plugins/%s/delete/complete/list", PluginID),
				},
				Type: model.PostActionTypeButton,
				Name: T("button.delete.complete"),
			})
	}

	actions = append(actions,
		&model.PostAction{
			Integration: &model.PostActionIntegration{
				Context: model.StringInterface{
					"action": "close/list",
				},
				URL: fmt.Sprintf("/plugins/%s/close/list", PluginID),
			},
			Name: T("button.close.list"),
			Type: "action",
		})

	return append(attachments, &model.MessageAttachment{
		Text:    T("reminders.page.numbers", remindersPageCount),
		Actions: actions,
	})
}

func (p *Plugin) addAttachment(user *model.User, occurrence Occurrence, reminders []Reminder, gType string, offset int) *model.MessageAttachment {
	location := p.location(user)
	T, _ := p.translation(user)

	reminder := p.findReminder(reminders, occurrence)

	t := occurrence.Occurrence
	s := occurrence.Snoozed

	formattedOccurrence := ""
	formattedOccurrence = p.formatWhen(user.Username, reminder.When, t.In(location).Format(time.RFC3339), false)

	formattedSnooze := ""
	if s != p.emptyTime {
		formattedSnooze = p.formatWhen(user.Username, reminder.When, s.In(location).Format(time.RFC3339), true)
	}
	var messageParameters = map[string]interface{}{
		"Message":    reminder.Message,
		"Occurrence": formattedOccurrence,
		"Snoozed":    formattedSnooze,
		"Channel":    reminder.Target,
	}
	if !t.Equal(p.emptyTime) {
		switch gType {
		case "upcoming":
			output := ""
			if formattedSnooze == "" {
				output = T("list.upcoming") + " " + T("list.element.upcoming", messageParameters)
			} else {
				output = T("list.upcoming") + " " + T("list.element.upcoming.snoozed", messageParameters)
			}
			return &model.MessageAttachment{
				Text: output,
				Actions: []*model.PostAction{
					{
						Integration: &model.PostActionIntegration{
							Context: model.StringInterface{
								"reminder_id":   reminder.Id,
								"occurrence_id": occurrence.Id,
								"action":        "complete/list",
								"offset":        offset,
							},
							URL: fmt.Sprintf("/plugins/%s/complete/list", PluginID),
						},
						Type: model.PostActionTypeButton,
						Name: T("button.complete"),
					},
					{
						Integration: &model.PostActionIntegration{
							Context: model.StringInterface{
								"reminder_id":   reminder.Id,
								"occurrence_id": occurrence.Id,
								"action":        "delete/list",
								"offset":        offset,
							},
							URL: fmt.Sprintf("/plugins/%s/delete/list", PluginID),
						},
						Name: T("button.delete"),
						Type: model.PostActionTypeButton,
					},
				},
			}
		case "recurring":
			output := ""
			if formattedSnooze == "" {
				output = T("list.recurring") + " " + T("list.element.recurring", messageParameters)
			} else {
				output = T("list.recurring") + " " + T("list.element.recurring.snoozed", messageParameters)
			}
			return &model.MessageAttachment{
				Text: output,
				Actions: []*model.PostAction{
					{
						Integration: &model.PostActionIntegration{
							Context: model.StringInterface{
								"reminder_id":   reminder.Id,
								"occurrence_id": occurrence.Id,
								"action":        "delete/list",
								"offset":        offset,
							},
							URL: fmt.Sprintf("/plugins/%s/delete/list", PluginID),
						},
						Name: T("button.delete"),
						Type: model.PostActionTypeButton,
					},
				},
			}
		case "past":
			output := ""
			if formattedSnooze == "" {
				output = T("list.past.and.incomplete") + " " + T("list.element.past", messageParameters)
			} else {
				output = T("list.past.and.incomplete") + " " + T("list.element.past.snoozed", messageParameters)
			}
			return &model.MessageAttachment{
				Text: output,
				Actions: []*model.PostAction{
					{
						Integration: &model.PostActionIntegration{
							Context: model.StringInterface{
								"reminder_id":   reminder.Id,
								"occurrence_id": occurrence.Id,
								"action":        "complete/list",
								"offset":        offset,
							},
							URL: fmt.Sprintf("/plugins/%s/complete/list", PluginID),
						},
						Type: model.PostActionTypeButton,
						Name: T("button.complete"),
					},
					{
						Integration: &model.PostActionIntegration{
							Context: model.StringInterface{
								"reminder_id":   reminder.Id,
								"occurrence_id": occurrence.Id,
								"action":        "delete/list",
								"offset":        offset,
							},
							URL: fmt.Sprintf("/plugins/%s/delete/list", PluginID),
						},
						Name: T("button.delete"),
						Type: model.PostActionTypeButton,
					},
					{
						Integration: &model.PostActionIntegration{
							Context: model.StringInterface{
								"reminder_id":   reminder.Id,
								"occurrence_id": occurrence.Id,
								"action":        "snooze/list",
								"offset":        offset,
							},
							URL: fmt.Sprintf("/plugins/%s/snooze/list", PluginID),
						},
						Name: T("button.snooze"),
						Type: "select",
						Options: []*model.PostActionOptions{
							{
								Text:  T("button.snooze.20min"),
								Value: "20min",
							},
							{
								Text:  T("button.snooze.1hr"),
								Value: "1hr",
							},
							{
								Text:  T("button.snooze.3hr"),
								Value: "3hrs",
							},
							{
								Text:  T("button.snooze.tomorrow"),
								Value: "tomorrow",
							},
							{
								Text:  T("button.snooze.nextweek"),
								Value: "nextweek",
							},
						},
					},
				},
			}
		case "channel":
			output := ""
			if formattedSnooze == "" {
				output = T("list.channel") + " " + T("list.element.channel", messageParameters)
			} else {
				output = T("list.channel") + " " + T("list.element.channel.snoozed", messageParameters)
			}
			return &model.MessageAttachment{
				Text: output,
				Actions: []*model.PostAction{
					{
						Integration: &model.PostActionIntegration{
							Context: model.StringInterface{
								"reminder_id":   reminder.Id,
								"occurrence_id": occurrence.Id,
								"action":        "complete/list",
								"offset":        offset,
							},
							URL: fmt.Sprintf("/plugins/%s/complete/list", PluginID),
						},
						Type: model.PostActionTypeButton,
						Name: T("button.complete"),
					},
					{
						Integration: &model.PostActionIntegration{
							Context: model.StringInterface{
								"reminder_id":   reminder.Id,
								"occurrence_id": occurrence.Id,
								"action":        "delete/list",
								"offset":        offset,
							},
							URL: fmt.Sprintf("/plugins/%s/delete/list", PluginID),
						},
						Name: T("button.delete"),
						Type: model.PostActionTypeButton,
					},
				},
			}
		}
	}

	return &model.MessageAttachment{}
}

func (p *Plugin) ListCompletedReminders(userID string, postID string, channelID string) {
	user, uErr := p.API.GetUser(userID)
	if uErr != nil {
		p.API.LogError(uErr.Error())
	}
	reminders := p.GetReminders(user.Username)

	var output string
	output = ""
	for _, reminder := range reminders {
		if reminder.Completed != p.emptyTime {
			location := p.location(user)
			output = strings.Join([]string{output, "* \"" + reminder.Message + "\" " + fmt.Sprintf("%v", reminder.Completed.In(location).Format(time.UnixDate))}, "\n")
		}
	}

	post := &model.Post{
		Id:        postID,
		ChannelId: channelID,
		Message:   output,
		Props:     model.StringInterface{},
	}
	p.API.UpdateEphemeralPost(userID, post)
}

func (p *Plugin) DeleteCompletedReminders(userID string) {
	user, uErr := p.API.GetUser(userID)
	if uErr != nil {
		p.API.LogError(uErr.Error())
	}
	reminders := p.GetReminders(user.Username)
	for _, reminder := range reminders {
		if reminder.Completed != p.emptyTime {
			dErr := p.DeleteReminder(userID, reminder)
			if dErr != nil {
				p.API.LogError("failed to update post %s", dErr)
			}
		}
	}
}
