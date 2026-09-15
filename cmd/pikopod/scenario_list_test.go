package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/config"
)

func setupTestSandbox(t *testing.T) *config.Config {
	t.Helper()
	absSpec, err := filepath.Abs(widgetsSpecPath)
	if err != nil {
		t.Fatalf("absSpec: %v", err)
	}
	dir := cliDir(t)
	cfg, err := config.Load(filepath.Join(dir, "pikopod.yaml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if err := sandboxAdd(cfg, "widgets", absSpec, "test-seed-1", "", "", false, io.Discard); err != nil {
		t.Fatalf("sandboxAdd: %v", err)
	}
	return cfg
}

func TestScenarioListDefaultOutput(t *testing.T) {
	cfg := setupTestSandbox(t)

	var out bytes.Buffer
	if err := scenarioList(cfg, "widgets", false, &out); err != nil {
		t.Fatalf("scenarioList: %v", err)
	}

	got := out.String()

	// Must include header and applicable / non-applicable archetypes.
	if !strings.Contains(got, "archetypes vs widgets") {
		t.Fatalf("expected header 'archetypes vs widgets', got:\n%s", got)
	}
	if !strings.Contains(got, "✓ happy_path") {
		t.Fatalf("expected '✓ happy_path' in output, got:\n%s", got)
	}
	// Non-verbose output must NOT contain candidate bindings like '      op='
	if strings.Contains(got, "      op=") {
		t.Fatalf("non-verbose output must not contain candidate bindings, got:\n%s", got)
	}
}

func TestScenarioListVerboseOutput(t *testing.T) {
	cfg := setupTestSandbox(t)

	var out bytes.Buffer
	if err := scenarioList(cfg, "widgets", true, &out); err != nil {
		t.Fatalf("scenarioList verbose: %v", err)
	}

	got := out.String()

	// Must contain archetype line AND indented candidate bindings following archetype's canonical role order.
	if !strings.Contains(got, "  ✓ happy_path                 Happy path  (1 candidate binding(s))\n      op=ep_1a284091a72f\n") {
		t.Fatalf("expected exact happy_path binding in verbose output, got:\n%s", got)
	}
	if !strings.Contains(got, "  ✓ rate_limit_backoff         Rate limit and backoff  (6 candidate binding(s))\n      op=ep_12d71306d3d7\n") {
		t.Fatalf("expected exact rate_limit_backoff binding in verbose output, got:\n%s", got)
	}
}

func TestScenarioListCLIFlag(t *testing.T) {
	setupTestSandbox(t)

	// Non-verbose via CLI
	cmdNonVerbose := newScenarioCmd()
	outNonVerbose, err := runCLI(t, cmdNonVerbose, "list", "widgets")
	if err != nil {
		t.Fatalf("CLI scenario list failed: %v", err)
	}
	if strings.Contains(outNonVerbose, "      op=") {
		t.Fatalf("CLI non-verbose should not have '      op=', got:\n%s", outNonVerbose)
	}

	// Verbose via --verbose
	cmdVerbose := newScenarioCmd()
	outVerbose, err := runCLI(t, cmdVerbose, "list", "widgets", "--verbose")
	if err != nil {
		t.Fatalf("CLI scenario list --verbose failed: %v", err)
	}
	if !strings.Contains(outVerbose, "      op=") {
		t.Fatalf("CLI --verbose should have '      op=', got:\n%s", outVerbose)
	}

	// Verbose via -v
	cmdShortVerbose := newScenarioCmd()
	outShortVerbose, err := runCLI(t, cmdShortVerbose, "list", "widgets", "-v")
	if err != nil {
		t.Fatalf("CLI scenario list -v failed: %v", err)
	}
	if !strings.Contains(outShortVerbose, "      op=") {
		t.Fatalf("CLI -v should have '      op=', got:\n%s", outShortVerbose)
	}

	if outVerbose != outShortVerbose {
		t.Fatalf("--verbose and -v output must match exactly.\n--verbose:\n%s\n-v:\n%s", outVerbose, outShortVerbose)
	}
}
