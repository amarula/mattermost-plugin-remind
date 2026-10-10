package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestTranslation(t *testing.T) {
	t.Run("if can translate with locale", func(t *testing.T) {
		p := &Plugin{}

		user := &model.User{
			Email:    "-@-.-",
			Nickname: "TestUser",
			Password: model.NewId(),
			Username: "testuser",
			Roles:    model.SystemUserRoleId,
			Locale:   "en",
		}

		p.locales = make(map[string]string)
		p.locales["en"] = "/test/en.json"
		T, locale := p.translation(user)
		assert.Equal(t, T("me"), "me")
		assert.Equal(t, locale, "en")
	})
}

func TestLocation(t *testing.T) {
	t.Run("if can locate", func(t *testing.T) {
		p := &Plugin{}
		timezone := make(map[string]string)
		timezone["manualTimezone"] = "America/Los_Angeles"
		user := &model.User{
			Email:    "-@-.-",
			Nickname: "TestUser",
			Password: model.NewId(),
			Username: "testuser",
			Roles:    model.SystemUserRoleId,
			Locale:   "en",
			Timezone: timezone,
		}

		location := p.location(user)
		assert.NotNil(t, location)
	})

	t.Run("if a timezone cannot be resolved, it still answers", func(t *testing.T) {
		api := &plugintest.API{}
		api.On("LogError", mock.Anything).Maybe()
		defer api.AssertExpectations(t)

		p := &Plugin{}
		p.API = api

		timezone := make(map[string]string)
		timezone["manualTimezone"] = "Mars/Olympus"
		user := &model.User{
			Email:    "-@-.-",
			Nickname: "TestUser",
			Password: model.NewId(),
			Username: "testuser",
			Roles:    model.SystemUserRoleId,
			Locale:   "en",
			Timezone: timezone,
		}

		// time.In and time.Date panic on a nil location, and this is on the path
		// the reminder tick takes, so the fallback has to be a location.
		location := p.location(user)
		assert.NotNil(t, location)
	})
}
