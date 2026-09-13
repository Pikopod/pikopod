package main

import (
	"runtime/debug"
	"testing"
)

func TestFormatVersion(t *testing.T) {
	cases := []struct {
		version, commit, date, want string
	}{
		{"dev", "", "", "dev"},
		{"v0.1.0", "abc1234", "2026-09-13T10:00:00Z", "v0.1.0 (abc1234 2026-09-13T10:00:00Z)"},
		{"dev", "abc1234-dirty", "", "dev (abc1234-dirty)"},
		{"v0.1.0", "", "2026-09-13T10:00:00Z", "v0.1.0 (2026-09-13T10:00:00Z)"},
	}
	for _, tc := range cases {
		got := formatVersion(tc.version, tc.commit, tc.date)
		if got != tc.want {
			t.Fatalf("formatVersion(%q, %q, %q) = %q, want %q", tc.version, tc.commit, tc.date, got, tc.want)
		}
	}
}

func TestResolveVersionFromBuildInfo(t *testing.T) {
	settings := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "deadbeef"},
		{Key: "vcs.time", Value: "2026-09-13T10:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}

	v, c, d := resolveVersion("dev", "", "", settings, "(devel)")
	if v != "dev" || c != "deadbeef-dirty" || d != "2026-09-13T10:00:00Z" {
		t.Fatalf("unset ldflags: got %q %q %q", v, c, d)
	}

	v, c, d = resolveVersion("v0.1.0", "stamped", "2020-01-01T00:00:00Z", settings, "v0.1.0")
	if v != "v0.1.0" || c != "stamped" || d != "2020-01-01T00:00:00Z" {
		t.Fatalf("ldflags must win: got %q %q %q", v, c, d)
	}

	v, c, d = resolveVersion("dev", "", "", []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abc"},
		{Key: "vcs.modified", Value: "false"},
	}, "(devel)")
	if c != "abc" || d != "" {
		t.Fatalf("clean tree: got commit=%q date=%q", c, d)
	}

	// go install pkg@version: no vcs.* settings, module version is set.
	v, c, d = resolveVersion("dev", "", "", nil, "v0.1.0")
	if v != "v0.1.0" || c != "" || d != "" {
		t.Fatalf("go install: got %q %q %q", v, c, d)
	}

	v, c, d = resolveVersion("dev", "", "", nil, "(devel)")
	if v != "dev" || c != "" {
		t.Fatalf("devel module must stay dev: got %q %q", v, c)
	}

	v, c, d = resolveVersion("dev", "", "", settings, "v9.9.9")
	if v != "dev" || c != "deadbeef-dirty" {
		t.Fatalf("vcs commit wins over module version: got %q %q", v, c)
	}
}
