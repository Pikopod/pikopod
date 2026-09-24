package main

import (
	"fmt"
	"runtime/debug"
	"strings"
)

var (
	version = "dev"
	commit  = ""
	date    = ""
)

func currentVersion() string {
	var settings []debug.BuildSetting
	moduleVersion := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
		moduleVersion = info.Main.Version
	}
	v, c, d := resolveVersion(version, commit, date, settings, moduleVersion)
	return formatVersion(v, c, d)
}

func resolveVersion(version, commit, date string, settings []debug.BuildSetting, moduleVersion string) (string, string, string) {
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
	if strings.TrimSpace(commit) == "" {
		mv := strings.TrimSpace(moduleVersion)
		if mv != "" && mv != "(devel)" && (version == "dev" || strings.TrimSpace(version) == "") {
			version = mv
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
