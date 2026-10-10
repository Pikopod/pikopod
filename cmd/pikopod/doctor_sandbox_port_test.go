package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDoctorReportsSandboxPort(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	yaml := "listen: 127.0.0.1\ndata_dir: " + filepath.Join(dir, "data") + "\nupstreams:\n  widgets:\n    target: https://api.example.invalid\n"
	if err := os.WriteFile("pikopod.yaml", []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, newDoctorCmd())
	if err == nil {
		t.Fatalf("doctor should fail on unreachable upstream, got success:\n%s", out)
	}
	if !strings.Contains(out, "sandbox port free or sandbox already running") {
		t.Fatalf("doctor must check sandbox port:\n%s", out)
	}
	if !strings.Contains(out, "agent port free or agent already running") {
		t.Fatalf("doctor must still check agent port:\n%s", out)
	}
}

func TestDoctorSandboxPortTakenByNonPikopod(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	t.Chdir(dir)
	yaml := fmt.Sprintf("listen: 127.0.0.1\nsandbox_port: %d\ndata_dir: %s\nupstreams:\n  widgets:\n    target: https://api.example.invalid\n", port, filepath.Join(dir, "data"))
	if err := os.WriteFile("pikopod.yaml", []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, newDoctorCmd())
	if err == nil {
		t.Fatalf("doctor must fail when sandbox port is foreign:\n%s", out)
	}
	if !strings.Contains(out, "sandbox port free or sandbox already running") {
		t.Fatalf("missing sandbox check line:\n%s", out)
	}
	if !strings.Contains(out, "not pikopod") {
		t.Fatalf("expected foreign-port error:\n%s", out)
	}
}

func TestDoctorSandboxPortAcceptsHealthz(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	u := srv.Listener.Addr().String()
	_, portStr, err := net.SplitHostPort(u)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	t.Chdir(dir)
	yaml := fmt.Sprintf("listen: 127.0.0.1\nsandbox_port: %d\ndata_dir: %s\nupstreams:\n  widgets:\n    target: https://api.example.invalid\n", port, filepath.Join(dir, "data"))
	if err := os.WriteFile("pikopod.yaml", []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, newDoctorCmd())
	if !strings.Contains(out, "✓ sandbox port free or sandbox already running") {
		t.Fatalf("healthz on sandbox port should pass check (err=%v):\n%s", err, out)
	}
}
