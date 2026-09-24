package pr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeGitHub struct {
	mu         []Comment
	nextID     int64
	userID     int64
	creates    int
	updates    int
	failUser   bool
	failUpdate bool
	perPage    int
}

func (f *fakeGitHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			if f.failUser {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": f.userID})
		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == "GET":
			page := 1
			fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
			per := f.perPage
			if per == 0 {
				per = 100
			}
			start := (page - 1) * per
			end := start + per
			if start > len(f.mu) {
				start = len(f.mu)
			}
			if end > len(f.mu) {
				end = len(f.mu)
			}
			out := []map[string]any{}
			for _, c := range f.mu[start:end] {
				out = append(out, map[string]any{"id": c.ID, "body": c.Body, "user": map[string]any{"id": c.AuthorID}})
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == "POST":
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in)
			f.nextID++
			f.mu = append(f.mu, Comment{ID: f.nextID, Body: in.Body, AuthorID: f.userID})
			f.creates++
			w.WriteHeader(201)
			w.Write([]byte("{}"))
		case strings.Contains(r.URL.Path, "/issues/comments/") && r.Method == "PATCH":
			if f.failUpdate {
				http.Error(w, "must have admin rights", http.StatusForbidden)
				return
			}
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in)
			var id int64
			fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "%d", &id)
			for i := range f.mu {
				if f.mu[i].ID == id {
					f.mu[i].Body = in.Body
				}
			}
			f.updates++
			w.Write([]byte("{}"))
		default:
			http.Error(w, "unhandled "+r.Method+" "+r.URL.Path, 500)
		}
	}))
}

func newForge(url string) *GitHub {
	return &GitHub{BaseURL: url, Repo: "o/r", Number: 7, Token: "t"}
}

func TestUpsertCreateThenUpdateThenUnchanged(t *testing.T) {
	fake := &fakeGitHub{userID: 42}
	srv := fake.server(t)
	defer srv.Close()
	f := newForge(srv.URL)

	action, err := UpsertComment(f, "spec-diff", "sha1", "body v1", nil)
	if err != nil || action != Created {
		t.Fatalf("create: %v %s", err, action)
	}
	action, err = UpsertComment(f, "spec-diff", "sha2", "body v2", nil)
	if err != nil || action != Updated {
		t.Fatalf("update: %v %s", err, action)
	}
	if fake.creates != 1 || fake.updates != 1 || len(fake.mu) != 1 {
		t.Fatalf("comment count drifted: creates=%d updates=%d n=%d", fake.creates, fake.updates, len(fake.mu))
	}
	action, err = UpsertComment(f, "spec-diff", "sha2", "body v2", nil)
	if err != nil || action != Unchanged {
		t.Fatalf("unchanged: %v %s", err, action)
	}

	action, _ = UpsertComment(f, "conformance", "sha2", "other", nil)
	if action != Created || len(fake.mu) != 2 {
		t.Fatalf("per-source comments: %s n=%d", action, len(fake.mu))
	}
}

func TestUpsertStaleSkip(t *testing.T) {
	fake := &fakeGitHub{userID: 42}
	srv := fake.server(t)
	defer srv.Close()
	f := newForge(srv.URL)

	if _, err := UpsertComment(f, "spec-diff", "newsha", "newer body", nil); err != nil {
		t.Fatal(err)
	}

	isAncestor := func(a, b string) bool { return a == "oldsha" && b == "newsha" }
	action, err := UpsertComment(f, "spec-diff", "oldsha", "older body", isAncestor)
	if err != nil || action != StaleSkip {
		t.Fatalf("stale: %v %s", err, action)
	}
	if !strings.Contains(fake.mu[0].Body, "newer body") {
		t.Fatal("stale update overwrote the newer comment")
	}
}

func TestUpsertPagination(t *testing.T) {
	fake := &fakeGitHub{userID: 42, perPage: 100}

	for i := 0; i < 250; i++ {
		fake.nextID++
		fake.mu = append(fake.mu, Comment{ID: fake.nextID, Body: fmt.Sprintf("noise %d", i), AuthorID: 7})
	}
	fake.nextID++
	fake.mu = append(fake.mu, Comment{ID: fake.nextID, Body: marker("spec-diff", "s1") + "\nold", AuthorID: 42})
	srv := fake.server(t)
	defer srv.Close()

	action, err := UpsertComment(newForge(srv.URL), "spec-diff", "s2", "new", nil)
	if err != nil || action != Updated {
		t.Fatalf("marker behind 2 full pages must still be found: %v %s", err, action)
	}
}

func TestUpsertAuthorshipFilter(t *testing.T) {
	fake := &fakeGitHub{userID: 42}

	fake.nextID++
	fake.mu = append(fake.mu, Comment{ID: fake.nextID, Body: marker("spec-diff", "x") + "\nimpostor", AuthorID: 99})
	srv := fake.server(t)
	defer srv.Close()

	action, err := UpsertComment(newForge(srv.URL), "spec-diff", "s", "ours", nil)
	if err != nil || action != Created {
		t.Fatalf("must never edit someone else's comment: %v %s", err, action)
	}
	if strings.Contains(fake.mu[0].Body, "ours") {
		t.Fatal("impostor comment was edited")
	}
}

func TestUpsertUserLookupFailureUpdatesOwnEditableComment(t *testing.T) {

	fake := &fakeGitHub{userID: 42, failUser: true}
	fake.nextID++
	fake.mu = append(fake.mu, Comment{ID: fake.nextID, Body: marker("spec-diff", "x") + "\nexisting", AuthorID: 42})
	srv := fake.server(t)
	defer srv.Close()

	action, err := UpsertComment(newForge(srv.URL), "spec-diff", "s", "b", nil)
	if err != nil || action != Updated {
		t.Fatalf("editable marker comment must update, not spam: %v %s", err, action)
	}
	if len(fake.mu) != 1 {
		t.Fatalf("no new comment expected: %d", len(fake.mu))
	}
}

func TestUpsertUserLookupFailureForbiddenUpdateFallsToCreate(t *testing.T) {

	fake := &fakeGitHub{userID: 42, failUser: true, failUpdate: true}
	fake.nextID++
	fake.mu = append(fake.mu, Comment{ID: fake.nextID, Body: marker("spec-diff", "x") + "\nimpostor", AuthorID: 99})
	srv := fake.server(t)
	defer srv.Close()

	action, err := UpsertComment(newForge(srv.URL), "spec-diff", "s", "ours", nil)
	if err != nil || action != Created {
		t.Fatalf("forbidden edit must fall to create: %v %s", err, action)
	}
	if strings.Contains(fake.mu[0].Body, "ours") {
		t.Fatal("impostor comment was edited")
	}
}

func TestGitLabPagination(t *testing.T) {
	notes := make([]map[string]any, 0, 150)
	for i := 0; i < 150; i++ {
		notes = append(notes, map[string]any{"id": i + 1, "body": fmt.Sprintf("n%d", i), "author": map[string]any{"id": 5}})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/notes") {
			http.Error(w, "unhandled", 500)
			return
		}
		page := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		start, end := (page-1)*100, page*100
		if start > len(notes) {
			start = len(notes)
		}
		if end > len(notes) {
			end = len(notes)
		}
		json.NewEncoder(w).Encode(notes[start:end])
	}))
	defer srv.Close()

	g := &GitLab{BaseURL: srv.URL, Project: "g/p", Number: 3, Token: "t"}
	got, err := g.ListComments()
	if err != nil || len(got) != 150 {
		t.Fatalf("gitlab pagination: %v n=%d", err, len(got))
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	src, sha, ok := parseMarker(marker("spec-update", "abc123") + "\nbody")
	if !ok || src != "spec-update" || sha != "abc123" {
		t.Fatalf("%s %s %v", src, sha, ok)
	}
	if _, _, ok := parseMarker("no marker here"); ok {
		t.Fatal("false positive")
	}
}

func TestRenderHandoffSources(t *testing.T) {
	cases := []struct {
		raw     string
		source  string
		expects []string
	}{
		{`{"source":"spec-diff","old":"a.yaml","new":"b.yaml","summary":{"ERR":1},"findings":[{"id":"endpoint-removed","level":"ERR","method":"GET","template":"/x","detail":"endpoint removed","fingerprint":"fp_1"}]}`,
			"spec-diff", []string{"spec-diff", "endpoint removed", "🔴"}},
		{`{"source":"spec-update","upstream":"pay","applied":[{"kind":"enum-union","method":"GET","template":"/tx","reason":"r","evidence":{"occurrences":3}}],"suggestions":[{"kind":"retype","method":"GET","template":"/tx","reason":"needs review","evidence":{}}]}`,
			"spec-update", []string{"additive patch", "enum-union", "needs review", "never auto-applied"}},
		{`{"source":"conformance","upstream":"pay","records":9,"violations":[{"method":"GET","template":"/tx","status":200,"pointer":"/a","severity":"error","message":"m","occurrences":2}]}`,
			"conformance", []string{"conformance", "violation", "2×"}},
		{`{"source":"replay-ci","records":5,"findings":[{"upstream":"pay","method":"GET","template":"/tx","kind":"field_removed","field":"fee","detail":"gone"}]}`,
			"replay-ci", []string{"replay --ci", "field_removed", "gone"}},
	}
	for _, c := range cases {
		source, md, err := RenderHandoff([]byte(c.raw))
		if err != nil || source != c.source {
			t.Fatalf("%s: %v", c.source, err)
		}
		for _, want := range c.expects {
			if !strings.Contains(md, want) {
				t.Fatalf("%s render missing %q:\n%s", c.source, want, md)
			}
		}
	}
	if _, _, err := RenderHandoff([]byte(`{"source":"mystery"}`)); err == nil {
		t.Fatal("unknown source must fail loudly")
	}
}
