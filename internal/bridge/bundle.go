package bridge

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/proxy"
)

const (
	BundleSchemaVersion = 1
	maxBundleBytes      = 8 << 20
	bundleDocs          = "docs/config-reference.md#retention"
)

type BundleSource struct {
	Host    string `json:"host,omitempty"`
	DataDir string `json:"data_dir,omitempty"`
}

type Bundle struct {
	SchemaVersion   int              `json:"schema_version"`
	ExportedAt      time.Time        `json:"exported_at"`
	Source          BundleSource     `json:"source"`
	ExpiresAt       *time.Time       `json:"expires_at,omitempty"`
	Event           alert.DriftEvent `json:"event"`
	Recording       proxy.Record     `json:"recording"`
	ContractVersion int              `json:"contract_version"`
}

func ExpiresAt(ev *alert.DriftEvent, retention time.Duration) *time.Time {
	if retention <= 0 {
		return nil
	}
	t := ev.LastSeen.Add(retention)
	return &t
}

func Export(dataDir string, ev *alert.DriftEvent, retention time.Duration, host string, now time.Time) (*Bundle, error) {
	rec, err := FindRecording(dataDir, ev)
	if err != nil {
		return nil, err
	}
	version := 0
	if ov, _ := contract.LoadOverlay(dataDir, ev.Upstream); ov != nil {
		version = ov.Version
	}
	return &Bundle{
		SchemaVersion:   BundleSchemaVersion,
		ExportedAt:      now.UTC(),
		Source:          BundleSource{Host: host, DataDir: dataDir},
		ExpiresAt:       ExpiresAt(ev, retention),
		Event:           *ev,
		Recording:       *rec,
		ContractVersion: version,
	}, nil
}

func IsBundleArg(arg string) bool {
	if strings.HasPrefix(arg, "fp_") {
		return false
	}
	if strings.HasSuffix(arg, ".json") || strings.ContainsAny(arg, `/\`) {
		return true
	}
	_, err := os.Stat(arg)
	return err == nil
}

func LoadBundle(path string) (*Bundle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errfmt.Newf("cannot read the incident bundle", "check the path", bundleDocs, "%v", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxBundleBytes+1))
	if err != nil {
		return nil, errfmt.Newf("cannot read the incident bundle", "retry", bundleDocs, "%v", err)
	}
	if len(raw) > maxBundleBytes {
		return nil, errfmt.New("incident bundle too large", path+" exceeds 8 MiB", "a bundle holds one event and one redacted recording; this is not one", bundleDocs)
	}
	return DecodeBundle(raw)
}

func DecodeBundle(raw []byte) (*Bundle, error) {
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, errfmt.Newf("incident bundle is not valid JSON", "export it again with `pikopod incidents export <fp>`", bundleDocs, "%v", err)
	}
	if probe.SchemaVersion != BundleSchemaVersion {
		return nil, errfmt.Newf("unsupported incident bundle", "export it again with this version of pikopod", bundleDocs, "schema_version %d, this pikopod reads %d", probe.SchemaVersion, BundleSchemaVersion)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b Bundle
	if err := dec.Decode(&b); err != nil {
		return nil, errfmt.Newf("incident bundle has fields this pikopod does not know", "export it again with this version of pikopod, or remove the extra fields", bundleDocs, "%v", err)
	}
	if b.Event.Fingerprint == "" || b.Recording.Method == "" {
		return nil, errfmt.New("incident bundle is incomplete", "it carries no event or no recording", "export it again with `pikopod incidents export <fp>`", bundleDocs)
	}
	return &b, nil
}
