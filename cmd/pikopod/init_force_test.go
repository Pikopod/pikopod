package main

import (
	"os"
	"strings"
	"testing"
)

func TestInitRefusesExistingWithoutForce(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("pikopod.yaml", []byte("listen: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, newInitCmd())
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("init without --force must refuse: %v", err)
	}
	got, err := os.ReadFile("pikopod.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "listen: 127.0.0.1\n" {
		t.Fatalf("refused init must not overwrite: %q", got)
	}
}

func TestInitForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("pikopod.yaml", []byte("listen: stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, newInitCmd(), "--force")
	if err != nil {
		t.Fatalf("init --force: %v\n%s", err, out)
	}
	if !strings.Contains(out, "wrote pikopod.yaml") {
		t.Fatalf("expected write confirmation, got %q", out)
	}
	got, err := os.ReadFile("pikopod.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "listen: stale\n" {
		t.Fatal("init --force must overwrite existing file")
	}
	if !strings.Contains(string(got), "upstreams:") {
		snippet := string(got)
		if len(snippet) > 80 {
			snippet = snippet[:80]
		}
		t.Fatalf("scaffold missing upstreams block: %q", snippet)
	}
}

func TestInitCreatesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	out, err := runCLI(t, newInitCmd())
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if _, err := os.Stat("pikopod.yaml"); err != nil {
		t.Fatalf("expected pikopod.yaml: %v", err)
	}
}
