package pr

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/specdiff"
)

// The spec-diff handoff must round-trip through the REAL producer
// serialization — the hand-written fixtures in pr_test.go would stay green
// across a field rename that breaks the actual pipeline.
func TestHandoffRoundTripsFromRealProducer(t *testing.T) {
	fs := []specdiff.Finding{{
		ID: "endpoint-removed", Level: specdiff.Err, Method: "GET",
		Template: "/legacy", Detail: "endpoint removed from the spec",
	}}
	var buf bytes.Buffer
	if err := specdiff.BuildReport("old.yaml", "new.yaml", fs).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	source, md, err := RenderHandoff(buf.Bytes())
	if err != nil || source != "spec-diff" {
		t.Fatalf("%v %s", err, source)
	}
	for _, want := range []string{"endpoint removed from the spec", "old.yaml", "🔴"} {
		if !strings.Contains(md, want) {
			t.Fatalf("real producer output must render, missing %q:\n%s", want, md)
		}
	}
}

// --------------------------------------------------------- GitLab writes

func fakeGitLabServer(t *testing.T, notes *[]map[string]any, failWrites bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "glt" {
			http.Error(w, "401", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/user":
			json.NewEncoder(w).Encode(map[string]any{"id": 7})
		case strings.HasSuffix(r.URL.Path, "/notes") && r.Method == "GET":
			json.NewEncoder(w).Encode(notes)
		case strings.HasSuffix(r.URL.Path, "/notes") && r.Method == "POST":
			if failWrites {
				http.Error(w, "insufficient scope", http.StatusForbidden)
				return
			}
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			*notes = append(*notes, map[string]any{"id": len(*notes) + 1, "body": in["body"], "author": map[string]any{"id": 7}})
			w.WriteHeader(201)
			w.Write([]byte("{}"))
		case strings.Contains(r.URL.Path, "/notes/") && r.Method == "PUT":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			(*notes)[0]["body"] = in["body"]
			w.Write([]byte("{}"))
		case strings.HasSuffix(r.URL.Path, "/merge_requests") && r.Method == "POST":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["source_branch"] == "" || in["target_branch"] == "" || in["title"] == "" {
				http.Error(w, "missing fields", 400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"web_url": "https://gitlab.example/mr/1"})
		default:
			http.Error(w, "unhandled "+r.Method+" "+r.URL.Path, 500)
		}
	}))
}

func TestGitLabCommentLifecycle(t *testing.T) {
	var notes []map[string]any
	srv := fakeGitLabServer(t, &notes, false)
	defer srv.Close()
	g := &GitLab{BaseURL: srv.URL, Project: "grp/proj", Number: 3, Token: "glt"}

	action, err := UpsertComment(g, "conformance", "sha1", "body v1", nil)
	if err != nil || action != Created || len(notes) != 1 {
		t.Fatalf("create: %v %s n=%d", err, action, len(notes))
	}
	action, err = UpsertComment(g, "conformance", "sha2", "body v2", nil)
	if err != nil || action != Updated || len(notes) != 1 {
		t.Fatalf("update: %v %s n=%d", err, action, len(notes))
	}
	if !strings.Contains(notes[0]["body"].(string), "body v2") {
		t.Fatalf("note not updated: %v", notes[0])
	}
}

func TestGitLabOpenPRFieldMapping(t *testing.T) {
	var notes []map[string]any
	srv := fakeGitLabServer(t, &notes, false)
	defer srv.Close()
	g := &GitLab{BaseURL: srv.URL, Project: "grp/proj", Number: 3, Token: "glt"}
	url, err := g.OpenPR("feature", "main", "title", "body")
	if err != nil || url != "https://gitlab.example/mr/1" {
		t.Fatalf("%v %s", err, url)
	}
}

func TestGitHubOpenPRFieldMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls" || r.Method != "POST" {
			http.Error(w, "unhandled", 500)
			return
		}
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["head"] != "feature" || in["base"] != "main" || in["title"] == "" {
			http.Error(w, "bad fields", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"html_url": "https://github.example/pr/9"})
	}))
	defer srv.Close()
	g := &GitHub{BaseURL: srv.URL, Repo: "o/r", Number: 1, Token: "t"}
	url, err := g.OpenPR("feature", "main", "t", "b")
	if err != nil || url != "https://github.example/pr/9" {
		t.Fatalf("%v %s", err, url)
	}
}

func TestForgeWriteFailureIsReadOnlyClassified(t *testing.T) {
	var notes []map[string]any
	srv := fakeGitLabServer(t, &notes, true)
	defer srv.Close()
	g := &GitLab{BaseURL: srv.URL, Project: "grp/proj", Number: 3, Token: "glt"}
	err := g.CreateComment("x")
	fe, ok := err.(*ForgeError)
	if !ok || !fe.ReadOnly() {
		t.Fatalf("403 must classify as read-only for the degradation ladder: %v", err)
	}
}

// A hostile forge returning a full page forever must be bounded, not an
// infinite loop + OOM.
func TestPaginationBounded(t *testing.T) {
	page := make([]map[string]any, perPage)
	for i := range page {
		page[i] = map[string]any{"id": i, "body": "x", "user": map[string]any{"id": 1}}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(page) // ALWAYS full
	}))
	defer srv.Close()
	g := &GitHub{BaseURL: srv.URL, Repo: "o/r", Number: 1, Token: "t"}
	if _, err := g.ListComments(); err == nil {
		t.Fatal("unbounded pagination must error out")
	}
}

// ------------------------------------------------------------- git guards

func TestGitIsAncestorRefusesOptionLikeArgs(t *testing.T) {
	if GitIsAncestor("-x", "HEAD") || GitIsAncestor("HEAD", "--upload-pack=/tmp/x") {
		t.Fatal("dash-prefixed args must be refused before reaching git argv")
	}
}
