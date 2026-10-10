package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

func (p *Plugin) InitAPI() *mux.Router {
	r := mux.NewRouter()
	r.HandleFunc("/dialog", p.handleDialog).Methods("POST")

	r.HandleFunc("/view/ephemeral", p.handleViewEphemeral).Methods("POST")
	r.HandleFunc("/view/complete/list", p.handleViewCompleteList).Methods("POST")

	r.HandleFunc("/complete", p.handleComplete).Methods("POST")
	r.HandleFunc("/complete/list", p.handleCompleteList).Methods("POST")

	r.HandleFunc("/delete", p.handleDelete).Methods("POST")
	r.HandleFunc("/delete/ephemeral", p.handleDeleteEphemeral).Methods("POST")
	r.HandleFunc("/delete/list", p.handleDeleteList).Methods("POST")
	r.HandleFunc("/delete/complete/list", p.handleDeleteCompleteList).Methods("POST")

	r.HandleFunc("/snooze", p.handleSnooze).Methods("POST")
	r.HandleFunc("/snooze/list", p.handleSnoozeList).Methods("POST")

	r.HandleFunc("/close/list", p.handleCloseList).Methods("POST")

	r.HandleFunc("/next/reminders", p.handleNextReminders).Methods("POST")

	return r
}

func (p *Plugin) ServeHTTP(_ *plugin.Context, w http.ResponseWriter, r *http.Request) {
	// These endpoints are reachable by any signed-in user, and the handlers read
	// their input with unchecked type assertions on the request body. A panic
	// there used to kill the plugin process, taking every other user's reminders
	// with it, so a failing request answers with an error instead.
	defer func() {
		if rec := recover(); rec != nil {
			p.API.LogError(fmt.Sprintf("Recovered from panic serving %s: %v", r.URL.Path, rec))
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}()

	p.router.ServeHTTP(w, r)
}

func (p *Plugin) handleDialog(w http.ResponseWriter, req *http.Request) {
	body, readErr := io.ReadAll(req.Body)
	defer func() { _ = req.Body.Close() }()
	if readErr != nil {
		http.Error(w, "could not read the submission", http.StatusBadRequest)
		return
	}

	var request *model.SubmitDialogRequest
	if decodeErr := json.Unmarshal(body, &request); decodeErr != nil || request == nil {
		http.Error(w, "could not read the submission", http.StatusBadRequest)
		return
	}

	user, uErr := p.API.GetUser(request.UserId)
	if uErr != nil {
		p.API.LogError(uErr.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	T, _ := p.translation(user)
	location := p.location(user)

	target := p.dialogTarget(user, submissionString(request.Submission, dialogElementTarget))

	// One submit adds every row that was filled in and removes the reminders
	// that were ticked. A submission that asks for neither is told so.
	var toAdd []dialogRow
	for _, row := range dialogRows(request.Submission) {
		if row.filled() {
			toAdd = append(toAdd, row)
		}
	}
	ticked := tickedReminderIDs(request.Submission)

	// What cannot be acted on is answered on the field it belongs to, which is
	// what keeps the dialog open with what was typed still in it. Failures used
	// to be swallowed into the log, and the dialog simply closed.
	fieldErrors := map[string]string{}
	for _, row := range toAdd {
		if row.message == "" {
			fieldErrors[row.messageElement] = T("schedule.error.message")
			continue
		}

		if row.when == "" {
			fieldErrors[row.whenElement] = T("schedule.error.when")
			continue
		}

		picked, whenErr := parseDialogDateTime(row.when, location)
		switch {
		case whenErr != nil:
			fieldErrors[row.whenElement] = T("schedule.error.when.format")
		case row.repeat == repeatOnce && !picked.After(time.Now()):
			fieldErrors[row.whenElement] = T("schedule.error.when.past")
		}
	}

	// The engine refuses a repeating reminder aimed at another user; it is
	// refused here first, where the user can see why. The target belongs to the
	// submission rather than to a row, so this answers on the target itself.
	if target != T("me") && strings.HasPrefix(target, "@") {
		for _, row := range toAdd {
			if row.repeat != repeatOnce {
				fieldErrors[dialogElementTarget] = T("schedule.error.target")
				break
			}
		}
	}

	if len(fieldErrors) > 0 {
		writeSubmitDialogResponse(w, &model.SubmitDialogResponse{Errors: fieldErrors})
		return
	}

	if len(toAdd) == 0 && len(ticked) == 0 {
		writeSubmitDialogResponse(w, &model.SubmitDialogResponse{Error: T("schedule.error.nothing")})
		return
	}

	for _, row := range toAdd {
		picked, _ := parseDialogDateTime(row.when, location)

		r := &ReminderRequest{
			TeamId:   request.TeamId,
			Username: user.Username,
			Payload:  row.message,
			Reminder: Reminder{
				Id:        model.NewId(),
				TeamId:    request.TeamId,
				Username:  user.Username,
				Message:   row.message,
				Completed: p.emptyTime,
				Target:    target,
				When:      p.composeWhen(user, picked, row.repeat),
			},
		}

		// Adding comes first: if it fails, nothing has been removed and the
		// rows are still in the dialog for another try.
		if cErr := p.CreateOccurrences(r); cErr != nil {
			p.API.LogError(cErr.Error())
			writeSubmitDialogResponse(w, &model.SubmitDialogResponse{
				Error: T("exception.response"),
				Type:  string(model.SubmitDialogResponseTypeForm),
				Form:  p.refreshedDialog(user),
			})
			return
		}

		if rErr := p.UpsertReminder(r); rErr != nil {
			p.API.LogError(rErr.Error())
			writeSubmitDialogResponse(w, &model.SubmitDialogResponse{
				Error: T("exception.response"),
				Type:  string(model.SubmitDialogResponseTypeForm),
				Form:  p.refreshedDialog(user),
			})
			return
		}
	}

	reminders := p.GetReminders(user.Username)
	for _, id := range ticked {
		reminder, found := findReminderByID(reminders, id)
		if !found {
			// Completed or deleted while the dialog was open.
			continue
		}

		p.removeReminder(user, reminder)
	}

	// The answer is the dialog again, rebuilt: the list it shows is the one the
	// submission just changed, and it stays open for the next one. A client that
	// does not know the form response closes the dialog instead, which is what it
	// did before. /remind list is still there for the buttons a dialog cannot
	// carry.
	writeSubmitDialogResponse(w, &model.SubmitDialogResponse{
		Type: string(model.SubmitDialogResponseTypeForm),
		Form: p.refreshedDialog(user),
	})
}

func (p *Plugin) handleViewEphemeral(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)

	user, uErr := p.API.GetUser(request.UserId)
	if uErr != nil {
		p.API.LogError(uErr.Error())
		writePostActionIntegrationResponseError(w, &model.PostActionIntegrationResponse{})
		return
	}
	p.API.SendEphemeralPost(user.Id, p.ListReminders(user, request.ChannelId))

	writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
}

func (p *Plugin) handleComplete(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)

	reminder := p.GetReminder(request.Context["orig_user_id"].(string), request.Context["reminder_id"].(string))
	user, uErr := p.API.GetUser(request.UserId)
	if uErr != nil {
		p.API.LogError(uErr.Error())
		writePostActionIntegrationResponseError(w, &model.PostActionIntegrationResponse{})
		return
	}
	T, _ := p.translation(user)

	for _, occurrence := range reminder.Occurrences {
		p.ClearScheduledOccurrence(reminder, occurrence)
	}

	reminder.Completed = time.Now().UTC()
	urErr := p.UpdateReminder(request.Context["orig_user_id"].(string), reminder)
	if urErr != nil {
		p.API.LogError("failed to update reminder %s", urErr)
	}

	if post, pErr := p.API.GetPost(request.PostId); pErr != nil {
		p.API.LogError("unable to get post " + pErr.Error())
		writePostActionIntegrationResponseError(w, &model.PostActionIntegrationResponse{})
	} else {
		user, uError := p.API.GetUser(request.UserId)
		if uError != nil {
			p.API.LogError(uError.Error())
			return
		}
		finalTarget := reminder.Target
		if finalTarget == T("me") {
			finalTarget = T("you")
		} else {
			finalTarget = "@" + user.Username
		}

		messageParameters := map[string]interface{}{
			"FinalTarget": finalTarget,
			"Message":     reminder.Message,
		}

		var updateParameters = map[string]interface{}{
			"Message": reminder.Message,
		}

		post.Message = "~~" + T("reminder.message", messageParameters) + "~~\n" + T("action.complete", updateParameters)
		post.Props = model.StringInterface{}
		_, upErr := p.API.UpdatePost(post)
		if upErr != nil {
			p.API.LogError("failed to update post %s", upErr)
		}

		if reminder.Username != user.Username {
			if originalUser, uErr := p.API.GetUserByUsername(reminder.Username); uErr != nil {
				p.API.LogError(uErr.Error())
				writePostActionIntegrationResponseError(w, &model.PostActionIntegrationResponse{})
				return
			} else {
				if channel, cErr := p.API.GetDirectChannel(p.botUserID, originalUser.Id); cErr != nil {
					p.API.LogError("failed to create channel " + cErr.Error())
					writePostActionIntegrationResponseError(w, &model.PostActionIntegrationResponse{})
				} else {
					var postbackUpdateParameters = map[string]interface{}{
						"User":    "@" + user.Username,
						"Message": reminder.Message,
					}
					if _, pErr := p.API.CreatePost(&model.Post{
						ChannelId: channel.Id,
						UserId:    p.botUserID,
						Message:   T("action.complete.callback", postbackUpdateParameters),
					}); pErr != nil {
						p.API.LogError(pErr.Error())
						writePostActionIntegrationResponseError(w, &model.PostActionIntegrationResponse{})
					}
				}
			}
		}

		writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
	}
}

func (p *Plugin) handleDelete(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)

	reminder := p.GetReminder(request.Context["orig_user_id"].(string), request.Context["reminder_id"].(string))
	user, uErr := p.API.GetUser(request.UserId)
	if uErr != nil {
		p.API.LogError(uErr.Error())
		writePostActionIntegrationResponseError(w, &model.PostActionIntegrationResponse{})
		return
	}
	T, _ := p.translation(user)

	for _, occurrence := range reminder.Occurrences {
		p.ClearScheduledOccurrence(reminder, occurrence)
	}

	message := reminder.Message
	dErr := p.DeleteReminder(request.Context["orig_user_id"].(string), reminder)
	if dErr != nil {
		p.API.LogError("failed to delete reminder %s", dErr)
	}

	if post, pErr := p.API.GetPost(request.PostId); pErr != nil {
		p.API.LogError(pErr.Error())
		writePostActionIntegrationResponseError(w, &model.PostActionIntegrationResponse{})
	} else {
		var deleteParameters = map[string]interface{}{
			"Message": message,
		}
		post.Message = T("action.delete", deleteParameters)
		post.Props = model.StringInterface{}
		_, upErr := p.API.UpdatePost(post)
		if upErr != nil {
			p.API.LogError("failed to update post %s", upErr)
		}
		writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
	}
}

func (p *Plugin) handleDeleteEphemeral(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)

	reminder := p.GetReminder(request.UserId, request.Context["reminder_id"].(string))
	user, uErr := p.API.GetUser(request.UserId)
	if uErr != nil {
		p.API.LogError(uErr.Error())
		writePostActionIntegrationResponseError(w, &model.PostActionIntegrationResponse{})
		return
	}
	T, _ := p.translation(user)

	for _, occurrence := range reminder.Occurrences {
		p.ClearScheduledOccurrence(reminder, occurrence)
	}

	message := reminder.Message
	dErr := p.DeleteReminder(request.UserId, reminder)
	if dErr != nil {
		p.API.LogError("failed to delete reminder %s", dErr)
	}
	var deleteParameters = map[string]interface{}{
		"Message": message,
	}
	post := &model.Post{
		Id:        request.PostId,
		UserId:    p.botUserID,
		ChannelId: request.ChannelId,
		Message:   T("action.delete", deleteParameters),
	}
	p.API.UpdateEphemeralPost(request.UserId, post)
	writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
}

func (p *Plugin) handleSnooze(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)

	reminder := p.GetReminder(request.Context["orig_user_id"].(string), request.Context["reminder_id"].(string))
	user, uErr := p.API.GetUser(request.UserId)
	if uErr != nil {
		p.API.LogError(uErr.Error())
		writePostActionIntegrationResponseError(w, &model.PostActionIntegrationResponse{})
		return
	}
	T, _ := p.translation(user)

	for _, occurrence := range reminder.Occurrences {
		if occurrence.Id == request.Context["occurrence_id"].(string) {
			p.ClearScheduledOccurrence(reminder, occurrence)
		}
	}

	if post, pErr := p.API.GetPost(request.PostId); pErr != nil {
		p.API.LogError("unable to get post " + pErr.Error())
		writePostActionIntegrationResponseError(w, &model.PostActionIntegrationResponse{})
	} else {
		var snoozeParameters = map[string]interface{}{
			"Message": reminder.Message,
		}

		switch request.Context["selected_option"].(string) {
		case "20min":
			for i, occurrence := range reminder.Occurrences {
				if occurrence.Id == request.Context["occurrence_id"].(string) {
					occurrence.Snoozed = time.Now().UTC().Round(time.Second).Add(time.Minute * time.Duration(20))
					reminder.Occurrences[i] = occurrence
					sErr := p.snoozeOccurrence(request.Context["orig_user_id"].(string), reminder, occurrence)
					if sErr != nil {
						p.API.LogError("failed to update reminder %s", sErr)
					}
					post.Message = T("action.snooze.20min", snoozeParameters)
					break
				}
			}
		case "1hr":
			for i, occurrence := range reminder.Occurrences {
				if occurrence.Id == request.Context["occurrence_id"].(string) {
					occurrence.Snoozed = time.Now().UTC().Round(time.Second).Add(time.Hour * time.Duration(1))
					reminder.Occurrences[i] = occurrence
					sErr := p.snoozeOccurrence(request.Context["orig_user_id"].(string), reminder, occurrence)
					if sErr != nil {
						p.API.LogError("failed to update reminder %s", sErr)
					}
					post.Message = T("action.snooze.1hr", snoozeParameters)
					break
				}
			}
		case "3hrs":
			for i, occurrence := range reminder.Occurrences {
				if occurrence.Id == request.Context["occurrence_id"].(string) {
					occurrence.Snoozed = time.Now().UTC().Round(time.Second).Add(time.Hour * time.Duration(3))
					reminder.Occurrences[i] = occurrence
					sErr := p.snoozeOccurrence(request.Context["orig_user_id"].(string), reminder, occurrence)
					if sErr != nil {
						p.API.LogError("failed to update reminder %s", sErr)
					}
					post.Message = T("action.snooze.3hr", snoozeParameters)
					break
				}
			}
		case "tomorrow":
			for i, occurrence := range reminder.Occurrences {
				if occurrence.Id == request.Context["occurrence_id"].(string) {
					if user, uErr := p.API.GetUser(request.UserId); uErr != nil {
						p.API.LogError(uErr.Error())
						return
					} else {
						location := p.location(user)
						tt := time.Now().In(location).Add(time.Hour * time.Duration(24))
						occurrence.Snoozed = time.Date(tt.Year(), tt.Month(), tt.Day(), 9, 0, 0, 0, location).UTC()
						reminder.Occurrences[i] = occurrence
						sErr := p.snoozeOccurrence(request.Context["orig_user_id"].(string), reminder, occurrence)
						if sErr != nil {
							p.API.LogError("failed to update reminder %s", sErr)
						}
						post.Message = T("action.snooze.tomorrow", snoozeParameters)
						break
					}
				}
			}
		case "nextweek":
			for i, occurrence := range reminder.Occurrences {
				if occurrence.Id == request.Context["occurrence_id"].(string) {
					if user, uErr := p.API.GetUser(request.UserId); uErr != nil {
						p.API.LogError(uErr.Error())
						return
					} else {
						location := p.location(user)

						todayWeekDayNum := int(time.Now().In(location).Weekday())
						weekDayNum := 1
						day := 0

						if weekDayNum < todayWeekDayNum {
							day = 7 - (todayWeekDayNum - weekDayNum)
						} else if weekDayNum >= todayWeekDayNum {
							day = 7 + (weekDayNum - todayWeekDayNum)
						}

						tt := time.Now().In(location).Add(time.Hour * time.Duration(24))
						occurrence.Snoozed = time.Date(tt.Year(), tt.Month(), tt.Day(), 9, 0, 0, 0, location).AddDate(0, 0, day).UTC()
						reminder.Occurrences[i] = occurrence
						sErr := p.snoozeOccurrence(request.Context["orig_user_id"].(string), reminder, occurrence)
						if sErr != nil {
							p.API.LogError("failed to update reminder %s", sErr)
						}
						post.Message = T("action.snooze.nextweek", snoozeParameters)
						break
					}
				}
			}
		}

		post.Props = model.StringInterface{}
		_, upErr := p.API.UpdatePost(post)
		if upErr != nil {
			p.API.LogError("failed to update post %s", upErr)
		}
		writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
	}
}

func (p *Plugin) handleNextReminders(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)
	p.UpdateListReminders(request.UserId, request.PostId, request.ChannelId, int(request.Context["offset"].(float64)))
	writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
}

func (p *Plugin) handleCompleteList(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)
	reminder := p.GetReminder(request.UserId, request.Context["reminder_id"].(string))

	for _, occurrence := range reminder.Occurrences {
		p.ClearScheduledOccurrence(reminder, occurrence)
	}

	reminder.Completed = time.Now().UTC()
	upErr := p.UpdateReminder(request.UserId, reminder)
	if upErr != nil {
		p.API.LogError("failed to update reminder %s", upErr)
	}
	p.UpdateListReminders(request.UserId, request.PostId, request.ChannelId, int(request.Context["offset"].(float64)))
	writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
}

func (p *Plugin) handleViewCompleteList(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)
	p.ListCompletedReminders(request.UserId, request.PostId, request.ChannelId)
	writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
}

func (p *Plugin) handleDeleteList(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)
	reminder := p.GetReminder(request.UserId, request.Context["reminder_id"].(string))

	for _, occurrence := range reminder.Occurrences {
		p.ClearScheduledOccurrence(reminder, occurrence)
	}

	dErr := p.DeleteReminder(request.UserId, reminder)
	if dErr != nil {
		p.API.LogError("failed to update post %s", dErr)
	}
	p.UpdateListReminders(request.UserId, request.PostId, request.ChannelId, int(request.Context["offset"].(float64)))
	writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
}

func (p *Plugin) handleDeleteCompleteList(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)
	p.DeleteCompletedReminders(request.UserId)
	p.UpdateListReminders(request.UserId, request.PostId, request.ChannelId, int(request.Context["offset"].(float64)))
	writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
}

func (p *Plugin) handleSnoozeList(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)
	reminder := p.GetReminder(request.UserId, request.Context["reminder_id"].(string))

	for _, occurrence := range reminder.Occurrences {
		if occurrence.Id == request.Context["occurrence_id"].(string) {
			p.ClearScheduledOccurrence(reminder, occurrence)
		}
	}

	switch request.Context["selected_option"].(string) {
	case "20min":
		for i, occurrence := range reminder.Occurrences {
			if occurrence.Id == request.Context["occurrence_id"].(string) {
				occurrence.Snoozed = time.Now().UTC().Round(time.Second).Add(time.Minute * time.Duration(20))
				reminder.Occurrences[i] = occurrence
				sErr := p.snoozeOccurrence(request.UserId, reminder, occurrence)
				if sErr != nil {
					p.API.LogError("failed to update reminder %s", sErr)
				}
				break
			}
		}
	case "1hr":
		for i, occurrence := range reminder.Occurrences {
			if occurrence.Id == request.Context["occurrence_id"].(string) {
				occurrence.Snoozed = time.Now().UTC().Round(time.Second).Add(time.Hour * time.Duration(1))
				reminder.Occurrences[i] = occurrence
				sErr := p.snoozeOccurrence(request.UserId, reminder, occurrence)
				if sErr != nil {
					p.API.LogError("failed to update reminder %s", sErr)
				}
				break
			}
		}
	case "3hrs":
		for i, occurrence := range reminder.Occurrences {
			if occurrence.Id == request.Context["occurrence_id"].(string) {
				occurrence.Snoozed = time.Now().UTC().Round(time.Second).Add(time.Hour * time.Duration(3))
				reminder.Occurrences[i] = occurrence
				sErr := p.snoozeOccurrence(request.UserId, reminder, occurrence)
				if sErr != nil {
					p.API.LogError("failed to update reminder %s", sErr)
				}
				break
			}
		}
	case "tomorrow":
		for i, occurrence := range reminder.Occurrences {
			if occurrence.Id == request.Context["occurrence_id"].(string) {
				if user, uErr := p.API.GetUser(request.UserId); uErr != nil {
					p.API.LogError(uErr.Error())
					return
				} else {
					location := p.location(user)
					tt := time.Now().In(location).Add(time.Hour * time.Duration(24))
					occurrence.Snoozed = time.Date(tt.Year(), tt.Month(), tt.Day(), 9, 0, 0, 0, location).UTC()
					reminder.Occurrences[i] = occurrence
					sErr := p.snoozeOccurrence(request.UserId, reminder, occurrence)
					if sErr != nil {
						p.API.LogError("failed to update reminder %s", sErr)
					}
					break
				}
			}
		}
	case "nextweek":
		for i, occurrence := range reminder.Occurrences {
			if occurrence.Id == request.Context["occurrence_id"].(string) {
				if user, uErr := p.API.GetUser(request.UserId); uErr != nil {
					p.API.LogError(uErr.Error())
					return
				} else {
					location := p.location(user)

					todayWeekDayNum := int(time.Now().In(location).Weekday())
					weekDayNum := 1
					day := 0

					if weekDayNum < todayWeekDayNum {
						day = 7 - (todayWeekDayNum - weekDayNum)
					} else if weekDayNum >= todayWeekDayNum {
						day = 7 + (weekDayNum - todayWeekDayNum)
					}

					tt := time.Now().In(location).Add(time.Hour * time.Duration(24))
					occurrence.Snoozed = time.Date(tt.Year(), tt.Month(), tt.Day(), 9, 0, 0, 0, location).AddDate(0, 0, day).UTC()
					reminder.Occurrences[i] = occurrence
					sErr := p.snoozeOccurrence(request.UserId, reminder, occurrence)
					if sErr != nil {
						p.API.LogError("failed to update reminder %s", sErr)
					}
					break
				}
			}
		}
	}

	p.UpdateListReminders(request.UserId, request.PostId, request.ChannelId, int(request.Context["offset"].(float64)))
	writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
}

func (p *Plugin) handleCloseList(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()

	var request *model.PostActionIntegrationRequest
	_ = json.Unmarshal(body, &request)
	p.API.DeleteEphemeralPost(request.UserId, request.PostId)
	writePostActionIntegrationResponseOk(w, &model.PostActionIntegrationResponse{})
}

// writeSubmitDialogResponse answers a dialog submission. It is always a 200,
// including when it carries errors: the client reads the body for field errors
// only on a successful submission, so answering with a failure status would
// throw them away and close the dialog.
func writeSubmitDialogResponse(w http.ResponseWriter, response *model.SubmitDialogResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	responseJSON, _ := json.Marshal(response)
	_, _ = w.Write(responseJSON)
}

func writePostActionIntegrationResponseOk(w http.ResponseWriter, response *model.PostActionIntegrationResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	responseJSON, _ := json.Marshal(response)
	_, _ = w.Write(responseJSON)
}

func writePostActionIntegrationResponseError(w http.ResponseWriter, response *model.PostActionIntegrationResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	responseJSON, _ := json.Marshal(response)
	_, _ = w.Write(responseJSON)
}
