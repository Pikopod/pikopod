package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pikopod.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsAndUpstreamListen(t *testing.T) {
	cfg, err := Load(writeCfg(t, "upstreams:\n  examplepay:\n    target: https://api.examplepay.co\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1" || cfg.AgentPort != 4700 || cfg.SandboxPort != 4600 {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if got := cfg.Upstreams["examplepay"].Listen; got != "/examplepay" {
		t.Fatalf("listen should default to /<name>, got %q", got)
	}
	if cfg.Warmup.MinSamples != 50 || *cfg.Warmup.MinHours != 48 {
		t.Fatalf("warmup defaults wrong: %+v", cfg.Warmup)
	}
}

func TestListenSafety(t *testing.T) {
	body := "listen: 0.0.0.0\nupstreams:\n  p:\n    target: https://example.com\n"

	t.Setenv("PIKOPOD_TOKEN", "")
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatal("0.0.0.0 without token must refuse")
	} else if !strings.Contains(err.Error(), "refusing to bind") || !strings.Contains(err.Error(), "→") {
		t.Fatalf("refusal must use the error contract: %v", err)
	}

	t.Setenv("PIKOPOD_TOKEN", "s3cret")
	if _, err := Load(writeCfg(t, body)); err != nil {
		t.Fatalf("token via env must allow: %v", err)
	}

	t.Setenv("PIKOPOD_TOKEN", "")
	tokFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokFile, []byte("filetoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeCfg(t, body+"token_file: "+tokFile+"\n"))
	if err != nil {
		t.Fatalf("token via file must allow: %v", err)
	}
	if cfg.Token() != "filetoken" {
		t.Fatalf("token file must be trimmed, got %q", cfg.Token())
	}
}

func TestMissingConfigUsesContract(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !strings.Contains(err.Error(), "pikopod init") {
		t.Fatalf("missing config must point at pikopod init: %v", err)
	}
}

func TestUpstreamValidation(t *testing.T) {
	if _, err := Load(writeCfg(t, "upstreams:\n  p:\n    listen: nope\n    target: https://x\n")); err == nil {
		t.Fatal("listen without leading / must fail")
	}
	if _, err := Load(writeCfg(t, "upstreams:\n  p:\n    listen: /p\n")); err == nil {
		t.Fatal("empty target must fail")
	}
}

func TestWarmupMinHoursZeroIsExpressible(t *testing.T) {
	cfg, err := Load(writeCfg(t, "upstreams:\n  p:\n    target: http://x\nwarmup:\n  min_samples: 5\n  min_hours: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if *cfg.Warmup.MinHours != 0 {
		t.Fatalf("explicit min_hours: 0 must survive, got %d", *cfg.Warmup.MinHours)
	}
}

func TestUnknownConfigKeysRefuse(t *testing.T) {
	for _, body := range []string{
		"upstreams:\n  p:\n    target: http://x\nenum_fieldz: [a]\n",
		"upstreams:\n  p:\n    target: http://x\n    enum_fields: [status]\n",
		"upstreams:\n  p:\n    target: http://x\nslack:\n  bot_token: xoxb-1\n",
	} {
		if _, err := Load(writeCfg(t, body)); err == nil {
			t.Fatalf("unknown key must refuse at startup, accepted:\n%s", body)
		}
	}

	if _, err := Load(writeCfg(t, "upstreams:\n  p:\n    target: http://x\n    volatile_fields: [ref]\n    mute: [\"/a/{id}\"]\nslack:\n  webhook_url: https://hooks.example\nllm:\n  model: m\nwarmup:\n  min_samples: 5\n  min_hours: 0\n")); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
}

func TestStorageKnobValidation(t *testing.T) {
	if _, err := Load(writeCfg(t, "upstreams: {}\nsampling:\n  rate: 1.5\n")); err == nil {
		t.Fatal("rate > 1 must be a startup error")
	}
	if _, err := Load(writeCfg(t, "upstreams: {}\nretention:\n  max_age_hours: -1\n")); err == nil {
		t.Fatal("negative retention must be a startup error")
	}
	cfg, err := Load(writeCfg(t, "upstreams: {}\nsampling:\n  rate: 0.1\nretention:\n  max_age_hours: 168\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SampleRate() != 0.1 || cfg.RetentionTTL() != 168*time.Hour {
		t.Fatalf("resolved knobs wrong: %v %v", cfg.SampleRate(), cfg.RetentionTTL())
	}

	plain, err := Load(writeCfg(t, "upstreams: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if plain.SampleRate() != 1 || plain.RetentionTTL() != 0 {
		t.Fatalf("defaults must be rate 1 / TTL off: %v %v", plain.SampleRate(), plain.RetentionTTL())
	}

	zero, err := Load(writeCfg(t, "upstreams: {}\nsampling:\n  rate: 0\n"))
	if err != nil || zero.SampleRate() != 0 {
		t.Fatalf("explicit rate 0 must resolve to 0: %v %v", zero.SampleRate(), err)
	}
}

func TestListenRouteOverlapRefused(t *testing.T) {
	base := func() *Config {
		return &Config{Upstreams: map[string]Upstream{
			"pay":  {Listen: "/pay", Target: "https://api.example.com"},
			"docs": {Listen: "/pay/sub", Target: "https://docs.example.com"},
		}}
	}
	if err := base().finish(); err == nil {
		t.Fatal("nested routes must be refused")
	}

	dup := &Config{Upstreams: map[string]Upstream{
		"a": {Listen: "/same", Target: "https://a.example.com"},
		"b": {Listen: "/same", Target: "https://b.example.com"},
	}}
	if err := dup.finish(); err == nil {
		t.Fatal("duplicate routes must be refused")
	}

	root := &Config{Upstreams: map[string]Upstream{
		"all": {Listen: "/", Target: "https://a.example.com"},
	}}
	if err := root.finish(); err == nil {
		t.Fatal("a / route swallows everything and must be refused")
	}

	ok := &Config{Upstreams: map[string]Upstream{
		"pay":  {Listen: "/pay", Target: "https://a.example.com"},
		"docs": {Listen: "/payments", Target: "https://b.example.com"},
	}}
	if err := ok.finish(); err != nil {
		t.Fatalf("distinct segments must pass: %v", err)
	}
}

func TestWorldReadableYAMLWithLLMKeyRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pikopod.yaml")
	yaml := "upstreams:\n  pay:\n    target: https://api.example.com\nllm:\n  openrouter_key: sk-or-test\n"
	os.WriteFile(path, []byte(yaml), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("world-readable yaml carrying a key must refuse: %v", err)
	}
	os.Chmod(path, 0o600)
	if _, err := Load(path); err != nil {
		t.Fatalf("0600 must pass: %v", err)
	}
}
