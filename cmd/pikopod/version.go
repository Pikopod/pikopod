package main

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// version, commit, and date are stamped by goreleaser via -ldflags.
// Source builds and `go install` leave them empty so resolveVersion can
// fill commit/date from the VCS metadata Go embeds in the binary.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func currentVersion() string {
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	v, c, d := resolveVersion(version, commit, date, settings)
	return formatVersion(v, c, d)
}

func resolveVersion(version, commit, date string, settings []debug.BuildSetting) (string, string, string) {
	if strings.TrimSpace(commit) == "" {
		rev := vcsSetting(settings, "vcs.revision")
		if rev != "" {
			if vcsSetting(settings, "vcs.modified") == "true" {
				rev += "-dirty"
			}
			commit = rev
		}
	}
	if strings.TrimSpace(date) == "" {
		if t := vcsSetting(settings, "vcs.time"); t != "" {
			date = t
		}
	}
	return version, commit, date
}

func formatVersion(version, commit, date string) string {
	switch {
	case commit != "" && date != "":
		return fmt.Sprintf("%s (%s %s)", version, commit, date)
	case commit != "":
		return fmt.Sprintf("%s (%s)", version, commit)
	case date != "":
		return fmt.Sprintf("%s (%s)", version, date)
	default:
		return version
	}
}

func vcsSetting(settings []debug.BuildSetting, key string) string {
	for _, s := range settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}
