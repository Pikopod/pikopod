package config

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/llmprovider"
	"github.com/pikopod/pikopod/internal/store"
	"gopkg.in/yaml.v3"
)

type Upstream struct {
	Listen string `yaml:"listen"`

	Target string `yaml:"target"`

	VolatileFields []string `yaml:"volatile_fields,omitempty"`

	Mute []string `yaml:"mute,omitempty"`

	SpecSource string `yaml:"spec_source,omitempty"`

	Incidents Incidents `yaml:"incidents,omitempty"`
}

type Incidents struct {
	ClientErrors    bool    `yaml:"client_errors,omitempty"`
	ClientErrorRate float64 `yaml:"client_error_rate,omitempty"`
}

const clientErrorFloor = 20

func (u Upstream) ClientErrorRateFor() float64 {
	if u.Incidents.ClientErrorRate > 0 {
		return u.Incidents.ClientErrorRate
	}
	return 0.05
}

func ClientErrorFloor() int { return clientErrorFloor }

type SpecWatch struct {
	IntervalMinutes int `yaml:"interval_minutes,omitempty"`
}

type Slack struct {
	WebhookURL string `yaml:"webhook_url,omitempty"`

	MinLevel    string `yaml:"min_level,omitempty"`
	DigestHours int    `yaml:"digest_hours,omitempty"`
}

type LLM struct {
	Provider string `yaml:"provider,omitempty"`
	APIKey   string `yaml:"api_key,omitempty"`
	Model    string `yaml:"model,omitempty"`

	OpenRouterKey string `yaml:"openrouter_key,omitempty"`
}

type Behaviour struct {
	Enabled bool `yaml:"enabled,omitempty"`
}

type Refine struct {
	Enabled bool `yaml:"enabled,omitempty"`

	PreferSpec bool `yaml:"prefer_spec,omitempty"`
}

type Sampling struct {
	Rate *float64 `yaml:"rate,omitempty"`
}

type Retention struct {
	MaxAgeHours int `yaml:"max_age_hours,omitempty"`
}

type Warmup struct {
	MinSamples int  `yaml:"min_samples,omitempty"`
	MinHours   *int `yaml:"min_hours,omitempty"`
}

type TLS struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
}

func (t TLS) Enabled() bool { return t.CertFile != "" && t.KeyFile != "" }

type Config struct {
	Listen      string              `yaml:"listen,omitempty"`
	AgentPort   int                 `yaml:"agent_port,omitempty"`
	SandboxPort int                 `yaml:"sandbox_port,omitempty"`
	DataDir     string              `yaml:"data_dir,omitempty"`
	TokenFile   string              `yaml:"token_file,omitempty"`
	TLS         TLS                 `yaml:"tls,omitempty"`
	Upstreams   map[string]Upstream `yaml:"upstreams"`
	Slack       Slack               `yaml:"slack,omitempty"`
	LLM         LLM                 `yaml:"llm,omitempty"`
	Warmup      Warmup              `yaml:"warmup,omitempty"`
	Refine      Refine              `yaml:"refine,omitempty"`
	Behaviour   Behaviour           `yaml:"behaviour,omitempty"`
	Sampling    Sampling            `yaml:"sampling,omitempty"`
	Retention   Retention           `yaml:"retention,omitempty"`
	SpecWatch   SpecWatch           `yaml:"spec_watch,omitempty"`

	token string

	sourcePath string
}

const (
	DefaultAgentPort   = 4700
	DefaultSandboxPort = 4600
	DefaultMinSamples  = 50
	DefaultMinHours    = 48
)

const defaultLLMProvider = llmprovider.Default

func (c *Config) SampleRate() float64 {
	if c.Sampling.Rate == nil {
		return 1
	}
	return *c.Sampling.Rate
}

func (c *Config) RetentionTTL() time.Duration {
	return time.Duration(c.Retention.MaxAgeHours) * time.Hour
}

func (c *Config) SpecWatchInterval() time.Duration {
	if c.SpecWatch.IntervalMinutes <= 0 {
		return time.Hour
	}
	return time.Duration(c.SpecWatch.IntervalMinutes) * time.Minute
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = "pikopod.yaml"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errfmt.New(
				"no configuration found",
				fmt.Sprintf("%s does not exist", path),
				"run `pikopod init` to scaffold one, or pass --config",
				"docs/config-reference.md")
		}
		return nil, errfmt.Newf("cannot read configuration", "check file permissions on "+path, "docs/config-reference.md", "%v", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))

	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && err != io.EOF {
		return nil, errfmt.Newf("configuration is not valid", "fix the key or syntax named below (unknown keys are rejected — see the reference for every valid key)", "docs/config-reference.md", "%s: %v", path, err)
	}
	cfg.sourcePath = path
	if err := cfg.finish(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) finish() error {
	if v := os.Getenv("PIKOPOD_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("PIKOPOD_DATA_DIR"); v != "" {
		c.DataDir = v
	}

	c.LLM.Provider = llmprovider.Normalize(c.LLM.Provider)
	if !llmprovider.Known(c.LLM.Provider) {
		valid := strings.Join(llmprovider.Names(), ", ")
		return errfmt.New(
			"unknown llm provider",
			fmt.Sprintf("%q is not registered; valid providers: %s", c.LLM.Provider, valid),
			"set llm.provider to one of: "+valid,
			"docs/config-reference.md#llm")
	}

	fileAPIKey := c.LLM.APIKey
	fileLegacyOpenRouterKey := c.LLM.OpenRouterKey
	resolvedKey := fileAPIKey
	keyFromFile := resolvedKey != ""
	if resolvedKey == "" {
		if v := os.Getenv("PIKOPOD_LLM_KEY"); v != "" {
			resolvedKey = v
		}
	}
	for _, env := range llmprovider.KeyEnvs(c.LLM.Provider) {
		if resolvedKey != "" {
			break
		}
		if v := os.Getenv(env); v != "" {
			resolvedKey = v
		}
	}

	if resolvedKey == "" && c.LLM.Provider == defaultLLMProvider && fileLegacyOpenRouterKey != "" {
		resolvedKey = fileLegacyOpenRouterKey
		keyFromFile = true
	}
	c.LLM.APIKey = resolvedKey

	c.LLM.OpenRouterKey = resolvedKey

	if c.LLM.Provider == defaultLLMProvider {
		if v := os.Getenv("PIKOPOD_OPENROUTER_MODEL"); v != "" {
			c.LLM.Model = v
		} else if v := os.Getenv("OPENROUTER_MODEL"); v != "" && c.LLM.Model == "" {
			c.LLM.Model = v
		}
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1"
	}
	if c.AgentPort == 0 {
		c.AgentPort = DefaultAgentPort
	}
	if c.SandboxPort == 0 {
		c.SandboxPort = DefaultSandboxPort
	}
	if c.DataDir == "" {
		c.DataDir = "pikopod-data"
	}
	if c.Warmup.MinSamples == 0 {
		c.Warmup.MinSamples = DefaultMinSamples
	}
	if c.Warmup.MinHours == nil {
		def := DefaultMinHours
		c.Warmup.MinHours = &def
	}
	if r := c.Sampling.Rate; r != nil && (*r < 0 || *r > 1) {
		return errfmt.New(
			"sampling.rate must be between 0 and 1",
			fmt.Sprintf("%v is not a fraction of routine records to persist", *r),
			"use e.g. 0.1 to keep ~10%; omit the key (or 1) to keep everything",
			"docs/config-reference.md#storage")
	}
	switch strings.ToUpper(c.Slack.MinLevel) {
	case "", "INFO", "WARN", "ERR":
		c.Slack.MinLevel = strings.ToUpper(c.Slack.MinLevel)
	default:
		return errfmt.New(
			"slack.min_level must be INFO, WARN or ERR",
			fmt.Sprintf("%q is not a severity floor", c.Slack.MinLevel),
			"use WARN to mute informational alerts; omit the key to deliver everything",
			"docs/config-reference.md#slack")
	}
	if c.Slack.DigestHours < 0 {
		return errfmt.New(
			"slack.digest_hours cannot be negative",
			fmt.Sprintf("%d is not an interval", c.Slack.DigestHours),
			"use e.g. 24 for a daily digest; omit the key (or 0) to disable",
			"docs/config-reference.md#slack")
	}
	if c.Retention.MaxAgeHours < 0 {
		return errfmt.New(
			"retention.max_age_hours cannot be negative",
			fmt.Sprintf("%d is not an age", c.Retention.MaxAgeHours),
			"use e.g. 168 for a week; omit the key (or 0) for size-only rotation",
			"docs/config-reference.md#storage")
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return errfmt.New(
			"tls needs both cert_file and key_file",
			"half a TLS config would silently serve plaintext",
			"set both keys, or neither (plain HTTP on loopback is the default)",
			"docs/config-reference.md#tls")
	}
	if c.TLS.Enabled() {
		for _, f := range []string{c.TLS.CertFile, c.TLS.KeyFile} {
			if _, err := os.Stat(f); err != nil {
				return errfmt.Newf(
					"tls file is not readable",
					"fix the path, or generate a pair: openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -keyout key.pem -out cert.pem -days 365 -nodes -subj /CN=pikopod",
					"docs/config-reference.md#tls", "%v", err)
			}
		}
	}

	c.token = os.Getenv("PIKOPOD_TOKEN")
	if c.token == "" && c.TokenFile != "" {

		if info, err := os.Stat(c.TokenFile); err == nil && store.PermTooOpen(info.Mode()) {
			return errfmt.New(
				"token file is readable by other users",
				fmt.Sprintf("%s has mode %o; group/other access defeats the token", c.TokenFile, info.Mode().Perm()),
				"chmod 600 "+c.TokenFile,
				"docs/config-reference.md#listen")
		}
		raw, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return errfmt.Newf("cannot read token file", "check token_file in pikopod.yaml points at a readable file", "docs/config-reference.md#listen", "%v", err)
		}
		c.token = strings.TrimSpace(string(raw))
	}

	if !isLoopback(c.Listen) && c.token == "" {
		return errfmt.New(
			"refusing to bind "+c.Listen+" without a token",
			"a non-loopback listener without auth would expose recorded traffic and controls to the network",
			"set PIKOPOD_TOKEN (env) or token_file in pikopod.yaml, or keep the default 127.0.0.1",
			"docs/config-reference.md#listen")
	}

	seenListen := map[string]string{}
	for name, u := range c.Upstreams {

		if !upstreamNameRE.MatchString(name) {
			return errfmt.New("invalid upstream name", fmt.Sprintf("%q may only contain letters, digits, _ and -", name), "rename the upstream in pikopod.yaml (it is a slug, not a URL)", "docs/config-reference.md#upstreams")
		}
		if u.Listen == "" {
			u.Listen = "/" + name
			c.Upstreams[name] = u
		}
		if !strings.HasPrefix(u.Listen, "/") {
			return errfmt.New("invalid upstream listen route", fmt.Sprintf("upstreams.%s.listen %q must start with /", name, u.Listen), "use a route like /"+name, "docs/config-reference.md#upstreams")
		}
		if u.Listen == "/" {
			return errfmt.New("upstream listen route cannot be /", fmt.Sprintf("upstreams.%s would swallow EVERY path, shadowing all other upstreams and the control endpoints", name), "use a named route like /"+name, "docs/config-reference.md#upstreams")
		}
		if u.Target == "" {
			return errfmt.New("upstream has no target", fmt.Sprintf("upstreams.%s.target is empty", name), "set the provider base URL, e.g. https://api.examplepay.com", "docs/config-reference.md#upstreams")
		}
		route := strings.TrimSuffix(u.Listen, "/")
		if other, taken := seenListen[route]; taken {
			return errfmt.New("upstream listen routes overlap",
				fmt.Sprintf("upstreams.%s and upstreams.%s both listen on %s — recordings and baselines would be mis-attributed", name, other, route),
				"give each upstream a distinct route", "docs/config-reference.md#upstreams")
		}
		seenListen[route] = name
	}

	for route, name := range seenListen {
		for other, otherName := range seenListen {
			if name == otherName {
				continue
			}
			if route == other || strings.HasPrefix(other, route+"/") {
				return errfmt.New("upstream listen routes overlap",
					fmt.Sprintf("upstreams.%s (%s) and upstreams.%s (%s) shadow each other under prefix routing — recordings and baselines would be mis-attributed", name, route, otherName, other),
					"give each upstream a distinct, non-nested route", "docs/config-reference.md#upstreams")
			}
		}
	}

	if keyFromFile && c.sourcePath != "" {
		if info, err := os.Stat(c.sourcePath); err == nil && store.PermTooOpen(info.Mode()) {
			return errfmt.New(
				"pikopod.yaml contains an llm API key but is readable by other users",
				fmt.Sprintf("%s has mode %o; group/other access leaks the key", c.sourcePath, info.Mode().Perm()),
				"chmod 600 "+c.sourcePath+" (or move the key to PIKOPOD_LLM_KEY or the provider's standard env var)",
				"docs/config-reference.md#llm")
		}
	}
	return nil
}

var upstreamNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (c *Config) Token() string { return c.token }

func (c *Config) UpstreamNames() []string {
	return slices.Sorted(maps.Keys(c.Upstreams))
}

func (c *Config) SaltPath() string { return filepath.Join(c.DataDir, ".salt") }

func (c *Config) Scheme() string {
	if c.TLS.Enabled() {
		return "https"
	}
	return "http"
}

func (c *Config) LocalClient(timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	if !c.TLS.Enabled() {
		return client
	}
	pool := x509.NewCertPool()
	if pem, err := os.ReadFile(c.TLS.CertFile); err == nil {
		pool.AppendCertsFromPEM(pem)
	}
	client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	return client
}

func isLoopback(addr string) bool {
	if addr == "localhost" {
		return true
	}
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}
