package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

func (p *Plugin) ScheduleReminder(request *ReminderRequest, channelID string) (*model.Post, error) {
	user, uErr := p.API.GetUserByUsername(request.Username)
	if uErr != nil {
		p.API.LogError(uErr.Error())
		return nil, uErr
	}
	T, _ := p.translation(user)
	location := p.location(user)

	if pErr := p.ParseRequest(request); pErr != nil {
		p.API.LogError(pErr.Error())
		return nil, pErr
	}

	useTo := strings.HasPrefix(request.Reminder.Message, T("to"))
	var useToString string
	if useTo {
		useToString = " " + T("to")
	} else {
		useToString = ""
	}

	request.Reminder.Id = model.NewId()
	request.Reminder.TeamId = request.TeamId
	request.Reminder.Username = request.Username
	request.Reminder.Completed = p.emptyTime

	if cErr := p.CreateOccurrences(request); cErr != nil {
		p.API.LogError(cErr.Error())
		return nil, cErr
	}

	if rErr := p.UpsertReminder(request); rErr != nil {
		p.API.LogError(rErr.Error())
		return nil, rErr
	}

	if request.Reminder.Target == T("me") {
		request.Reminder.Target = T("you")
	}

	t := ""
	if len(request.Reminder.Occurrences) > 0 {
		t = request.Reminder.Occurrences[0].Occurrence.In(location).Format(time.RFC3339)
	}
	var responseParameters = map[string]interface{}{
		"Target":  request.Reminder.Target,
		"UseTo":   useToString,
		"Message": request.Reminder.Message,
		"When": p.formatWhen(
			request.Username,
			request.Reminder.When,
			t,
			false,
		),
	}

	return &model.Post{
		ChannelId: channelID,
		UserId:    p.botUserID,
		Props: model.StringInterface{
			"attachments": []*model.MessageAttachment{
				{
					Text: T("schedule.response", responseParameters),
					Actions: []*model.PostAction{
						{
							Id: model.NewId(),
							Integration: &model.PostActionIntegration{
								Context: model.StringInterface{
									"reminder_id":   request.Reminder.Id,
									"occurrence_id": request.Reminder.Occurrences[0].Id,
									"action":        "delete/ephemeral",
								},
								URL: fmt.Sprintf("/plugins/%s/delete/ephemeral", PluginID),
							},
							Type: model.PostActionTypeButton,
							Name: T("button.delete"),
						},
						{
							Id: model.NewId(),
							Integration: &model.PostActionIntegration{
								Context: model.StringInterface{
									"reminder_id":   request.Reminder.Id,
									"occurrence_id": request.Reminder.Occurrences[0].Id,
									"action":        "view/ephemeral",
								},
								URL: fmt.Sprintf("/plugins/%s/view/ephemeral", PluginID),
							},
							Type: model.PostActionTypeButton,
							Name: T("button.view.reminders"),
						},
					},
				},
			},
		},
	}, nil
}

// scheduleDialog is the dialog /remind opens. It is also the one a submission is
// answered with, so the list it shows is the one the submission just changed.
func (p *Plugin) scheduleDialog(user *model.User) model.Dialog {
	T, _ := p.translation(user)
	// The zone the picker shows its times in, and the zone the phrase it
	// composes is read in. Empty when the user has set none.
	timezone := p.dialogTimezone(user)

	// The reminders come first: ticking one marks it for removal, and filling in
	// the fields below adds one. A single submit does both, so every field is
	// optional and the rule between them lives in the handler.
	elements := p.dialogReminderElements(user, p.GetReminders(user.Username))
	elements = append(elements,
		model.DialogElement{
			DisplayName: T("schedule.message"),
			Name:        dialogElementMessage,
			Type:        "text",
			SubType:     "text",
			HelpText:    T("schedule.message.help"),
			Optional:    true,
		},
		model.DialogElement{
			DisplayName: T("schedule.when"),
			Name:        dialogElementWhen,
			Type:        "datetime",
			HelpText:    p.whenHelpText(user, timezone),
			DateTimeConfig: &model.DialogDateTimeConfig{
				// "today" rather than an instant: the field is validated as a date
				// and rendered in the configured zone, and the authoritative check
				// that the time has not passed happens on submission.
				MinDate:          "today",
				TimeInterval:     15,
				LocationTimezone: timezone,
				ManualTimeEntry:  true,
			},
		},
		model.DialogElement{
			DisplayName: T("schedule.repeat"),
			Name:        dialogElementRepeat,
			Type:        "select",
			SubType:     "select",
			Default:     repeatOnce,
			HelpText:    T("schedule.repeat.help"),
			Options: []*model.PostActionOptions{
				{
					Text:  T("schedule.repeat.once"),
					Value: repeatOnce,
				},
				{
					Text:  T("schedule.repeat.daily"),
					Value: repeatDaily,
				},
				{
					Text:  T("schedule.repeat.weekdays"),
					Value: repeatWeekdays,
				},
				{
					Text:  T("schedule.repeat.weekly"),
					Value: repeatWeekly,
				},
				{
					Text:  T("schedule.repeat.monthly"),
					Value: repeatMonthly,
				},
			},
		},
		model.DialogElement{
			DisplayName: T("schedule.target"),
			Name:        dialogElementTarget,
			HelpText:    T("schedule.target.help"),
			Placeholder: "me",
			Type:        "text",
			SubType:     "text",
			Optional:    true,
		},
	)

	return model.Dialog{
		Title:      T("schedule.reminder"),
		CallbackId: model.NewId(),
		// One button does both: what is ticked is removed, what is filled in is
		// added.
		SubmitLabel: T("button.save"),
		Elements:    elements,
	}
}

func (p *Plugin) InteractiveSchedule(triggerID string, user *model.User) {
	dialogRequest := model.OpenDialogRequest{
		TriggerId: triggerID,
		URL:       fmt.Sprintf("/plugins/%s/dialog", PluginID),
		Dialog:    p.scheduleDialog(user),
	}

	if pErr := p.API.OpenInteractiveDialog(dialogRequest); pErr != nil {
		p.API.LogError("Failed opening interactive dialog " + pErr.Error())
	}
}

func (p *Plugin) Run() {
	p.Stop()
	if !p.running.Load() {
		p.running.Store(true)
		p.runner(p.generation.Add(1))
	}
}

func (p *Plugin) Stop() {
	p.running.Store(false)
	// Bumping the generation retires the tick that is already in flight: without
	// it, a tick scheduled before this Stop would see running set to true again by
	// a later Run and start a second chain alongside the new one.
	p.generation.Add(1)
}

func (p *Plugin) runner(generation uint64) {
	go func() {
		<-time.NewTimer(time.Second).C
		if !p.running.Load() || p.generation.Load() != generation {
			return
		}
		p.triggerRemindersSafely()
		p.runner(generation)
	}()
}

// triggerRemindersSafely runs one tick. A panic in a tick used to kill the plugin
// process while the server kept proxying requests to it, so a failed tick is
// contained instead: the next one runs a second later.
func (p *Plugin) triggerRemindersSafely() {
	defer func() {
		if r := recover(); r != nil {
			p.API.LogError(fmt.Sprintf("Recovered from panic in the reminder tick: %v", r))
		}
	}()

	p.TriggerReminders()
}
