package main

import (
	"os"
	"sync/atomic"
	"time"

	// Embed the timezone database so resolving a user's time zone does not depend
	// on the host shipping /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/pkg/errors"
)

const (
	botUserName    = "remindbot"
	botDisplayName = "Remindbot"

	// PluginID is the id the plugin is installed under, as plugin.json declares it.
	// It goes into every action URL the plugin hands to the web app.
	PluginID = "com.github.scottleedavis.mattermost-plugin-remind"
)

// PluginVersion is the version reported to users, injected at build time as
// "vX.Y.Z" by the build. A build that skips those ldflags says "dev" rather than
// reporting a version it does not have.
var PluginVersion = "dev"

type Plugin struct {
	plugin.MattermostPlugin
	client *pluginapi.Client

	router    *mux.Router
	botUserId string
	emptyTime time.Time

	// running and generation are touched by the reminder tick goroutine as well as
	// by the plugin hooks, so they are atomic.
	running    atomic.Bool
	generation atomic.Uint64

	ServerConfig *model.Config

	readFile func(path string) ([]byte, error)
	locales  map[string]string
}

func NewPlugin() *Plugin {
	return &Plugin{
		readFile: os.ReadFile,
		locales:  make(map[string]string),
	}
}

func (p *Plugin) OnActivate() error {
	p.client = pluginapi.NewClient(p.API, p.Driver)

	p.ServerConfig = p.API.GetConfig()
	p.router = p.InitAPI()
	p.emptyTime = time.Time{}.AddDate(1, 1, 1)

	botID, err := p.client.Bot.EnsureBot(&model.Bot{
		Username:    botUserName,
		DisplayName: botDisplayName,
		Description: "Created by the GitHub plugin.",
	}, pluginapi.ProfileImagePath("assets/icon.png"))
	if err != nil {
		return errors.Wrap(err, "failed to ensure remind bot")
	}
	p.botUserId = botID

	err = p.registerCommand()
	if err != nil {
		return errors.Wrap(err, "failed to register command")
	}

	if err := p.TranslationsPreInit(); err != nil {
		return errors.Wrap(err, "failed to initialize translations")
	}
	p.Run()

	return nil
}

func (p *Plugin) OnDeactivate() error {
	p.Stop()
	return nil
}
