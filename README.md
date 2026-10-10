# Mattermost Plugin Remind

 [![Build Status](https://img.shields.io/circleci/project/github/scottleedavis/mattermost-plugin-remind/master.svg)](https://circleci.com/gh/scottleedavis/mattermost-plugin-remind)
 [![codecov](https://codecov.io/gh/scottleedavis/mattermost-plugin-remind/branch/master/graph/badge.svg)](https://codecov.io/gh/scottleedavis/mattermost-plugin-remind)  
 [![Go Report Card](https://goreportcard.com/badge/github.com/scottleedavis/mattermost-plugin-remind)](https://goreportcard.com/report/github.com/scottleedavis/mattermost-plugin-remind)  
 [![CII Best Practices](https://bestpractices.coreinfrastructure.org/projects/3119/badge)](https://bestpractices.coreinfrastructure.org/projects/3119)  
 [![Releases](https://img.shields.io/github/release/scottleedavis/mattermost-plugin-remind.svg)](https://github.com/scottleedavis/mattermost-plugin-remind/releases/latest)

_**A bot that schedules reminders for [Mattermost](https://mattermost.com/)**_

<img src="remind.png">

### Installation

_requires Mattermost v11.0.0 or greater._

1) Go to the [releases page](https://github.com/scottleedavis/mattermost-plugin-remind/releases) of this GitHub repository and download the latest release for your Mattermost server.
2) Upload this file in the Mattermost System Console > Plugins > Management page to install the plugin. To learn more about how to upload a plugin, see the documentation.
3) For a better cross timezone experience, enable Experimental timezone support.  `System Console -> Experimental Features -> Timezone  = true`
4) (Opt.) If your server is not configured for cross-team DMs (i.e. `Enable users to open Direct Message channels with:` is set to `Users on same Team`) then you will need to add `remindbot` to any team which wishes to use the plugin. This is done through the "Manage members" interface.


### Usage

* `/remind` - opens up an [interactive dialog](https://docs.mattermost.com/developer/interactive-dialogs.html) to schedule a reminder
* `/remind help` - displays help examples
* `/remind list` - displays a list of reminders
* `/remind [who] [what] [when]`
  * `/remind [who] [what] in [# (seconds|minutes|hours|days|weeks|months|years)]`
  * `/remind [who] [what] at [(noon|midnight|one..twelve|00:00am/pm|0000)] (every) [day|date]`
  * `/remind [who] [what] (on) [(monday-sunday|month&day|m/d/y|d.m.y)] (at) [time]`
  * `/remind [who] [what] every (other) [monday,...,sunday|weekdays|month&day|m/d|d.m] (at) [time]`
* `/remind [who] [when] [what]`

Here is the full list of [Examples](https://github.com/scottleedavis/mattermost-plugin-remind/wiki/Usage)

### Build

```
make dist
```

This produces one plugin file per platform in the `dist` directory. The platforms
come from the `server.executables` map in `plugin.json`, so add an entry there to
build for another one.

| Flavor              | Distribution |
| ------------------- | ------------ |
| Linux (x86-64)      | `mattermost-plugin-com.github.scottleedavis.mattermost-plugin-remind-vx.y.z-linux-amd64.tar.gz` |
| Linux (arm64)       | `mattermost-plugin-com.github.scottleedavis.mattermost-plugin-remind-vx.y.z-linux-arm64.tar.gz` |
| macOS (Intel)       | `mattermost-plugin-com.github.scottleedavis.mattermost-plugin-remind-vx.y.z-darwin-amd64.tar.gz` |
| macOS (Apple silicon) | `mattermost-plugin-com.github.scottleedavis.mattermost-plugin-remind-vx.y.z-darwin-arm64.tar.gz` |
| Windows (x86-64)    | `mattermost-plugin-com.github.scottleedavis.mattermost-plugin-remind-vx.y.z-windows-amd64.tar.gz` |

Upload the file for your server's platform in the Mattermost System Console.
Tagged releases are built and published by the `Release` workflow in
`.github/workflows/release.yml`, so pushing a `vx.y.z` tag is all a release
needs.

`make` on its own runs the style checks, the tests and the build.
