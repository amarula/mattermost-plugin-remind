package main

import (
	"time"

	"github.com/mattermost/go-i18n/i18n"
	"github.com/mattermost/mattermost/server/public/model"
)

func (p *Plugin) translation(user *model.User) (i18n.TranslateFunc, string) {
	locale := "en"
	for l := range p.locales {
		if user.Locale == l {
			locale = user.Locale
			break
		}
	}
	return p.GetUserTranslations(locale), locale
}

// location is the time zone a reminder for this user is expressed in: the one
// they set, or the server's if they have not set one.
//
// It never returns nil. A time zone the host cannot resolve used to be returned
// as a nil *time.Location, and time.In/Date panic on nil, which took down the
// whole plugin process from inside the reminder tick.
func (p *Plugin) location(user *model.User) *time.Location {
	serverLocation := time.Now().Location()

	tz := user.GetPreferredTimezone()
	if tz == "" {
		// Use server's timezone
		return serverLocation
	}

	location, err := time.LoadLocation(tz)
	if err != nil {
		p.API.LogError("Unknown timezone for user " + user.Username + ", using the server's: " + tz)
		return serverLocation
	}

	return location
}
