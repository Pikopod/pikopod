package sandbox

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
)

type transcriptStep struct {
	Name        string            `json:"name"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	ReqHeaders  map[string]string `json:"reqHeaders"`
	ReqBody     json.RawMessage   `json:"reqBody"`
	Status      int               `json:"status"`
	RespHeaders map[string]string `json:"respHeaders"`
	RespBody    json.RawMessage   `json:"respBody"`
}

type transcriptMeta struct {
	SpecFile        string   `json:"specFile"`
	SandboxSeed     string   `json:"sandboxSeed"`
	VirtualClockMs  int64    `json:"virtualClockMs"`
	Credential      string   `json:"credential"`
	NormalizedPaths []string `json:"normalizedPaths"`
}

type transcript struct {
	Meta  transcriptMeta   `json:"meta"`
	Steps []transcriptStep `json:"steps"`
}

var capturedRespHeaders = []string{"content-type", "etag", "allow", "link", "idempotent-replayed", "www-authenticate"}

func TestParity_SandboxTranscripts(t *testing.T) {
	files, err := filepath.Glob("../../testdata/parity/sandbox/*.transcript.json")
	if err != nil {
		t.Fatalf("glob transcripts: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("no transcripts under testdata/parity/sandbox — restore them from git")
	}
	for _, file := range files {
		file := file
		t.Run(filepath.Base(file), func(t *testing.T) { replayTranscript(t, file) })
	}
}

func replayTranscript(t *testing.T, file string) {
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	var tr transcript
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("parse transcript: %v", err)
	}

	specRaw, err := os.ReadFile(filepath.Join("../../testdata/parity", tr.Meta.SpecFile))
	if err != nil {
		t.Fatalf("read spec %s: %v", tr.Meta.SpecFile, err)
	}
	def, err := importer.NormalizeOpenAPI(specRaw)
	if err != nil {
		t.Fatalf("normalize spec: %v", err)
	}
	store, err := OpenMemoryStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	engine, err := NewEngine(def, Config{
		ID:             "sbx_parity",
		Seed:           tr.Meta.SandboxSeed,
		Mode:           "deterministic",
		VirtualClockMs: tr.Meta.VirtualClockMs,

		Credential: tr.Meta.Credential,
	}, store)
	if err != nil {
		t.Fatalf("build engine: %v", err)
	}

	for _, step := range tr.Steps {
		t.Run(step.Name, func(t *testing.T) {
			var body *strings.Reader
			if len(step.ReqBody) > 0 && string(step.ReqBody) != "null" {
				body = strings.NewReader(string(step.ReqBody))
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(step.Method, step.Path, body)
			hasCT := false
			for k, v := range step.ReqHeaders {
				if v == "$CREDENTIAL" {
					v = engine.Credential()
				}
				req.Header.Set(k, v)
				if strings.EqualFold(k, "content-type") {
					hasCT = true
				}
			}
			if !hasCT && len(step.ReqBody) > 0 && string(step.ReqBody) != "null" {
				req.Header.Set("content-type", "application/json")
			}

			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)

			if rec.Code != step.Status {
				t.Fatalf("status: got %d want %d (body %s)", rec.Code, step.Status, rec.Body.String())
			}

			wantBody := decodeJSON(t, step.RespBody)
			var gotBody any
			if rec.Body.Len() > 0 {
				gotBody = decodeJSON(t, rec.Body.Bytes())
			}
			if !reflect.DeepEqual(gotBody, wantBody) {
				t.Fatalf("body:\n got  %s\n want %s", rec.Body.String(), string(step.RespBody))
			}

			for _, h := range capturedRespHeaders {
				got := rec.Header().Get(h)
				want := step.RespHeaders[h]
				if got != want {
					t.Fatalf("header %s: got %q want %q", h, got, want)
				}
			}
		})
	}
}

func decodeJSON(t *testing.T, raw []byte) any {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("parse JSON %q: %v", string(raw), err)
	}
	return v
}
