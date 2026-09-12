// GitHub forge over the REST API. Auth: token in the Authorization header,
// sourced from env or `gh auth token` — never argv.
package pr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

type GitHub struct {
	BaseURL string // default https://api.github.com; overridable for tests/GHE
	Repo    string // owner/name
	Number  int    // PR number
	Token   string
	Client  *http.Client
}

func (g *GitHub) Name() string { return "github" }

func (g *GitHub) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (g *GitHub) base() string {
	if g.BaseURL != "" {
		return g.BaseURL
	}
	return "https://api.github.com"
}

func (g *GitHub) do(method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, g.base()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return &ForgeError{Status: resp.StatusCode, Detail: fmt.Sprintf("github %s %s: %s", method, path, truncate(string(raw), 200))}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (g *GitHub) CurrentUserID() (int64, error) {
	var u struct {
		ID int64 `json:"id"`
	}
	if err := g.do("GET", "/user", nil, &u); err != nil {
		return 0, err
	}
	return u.ID, nil
}

// maxPages bounds pagination so a server that always returns a full page
// cannot hang the CI job (20k comments, far past any real PR).
const (
	perPage  = 100
	maxPages = 200
)

func (g *GitHub) ListComments() ([]Comment, error) {
	var out []Comment
	for page := 1; page <= maxPages; page++ {
		var batch []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User struct {
				ID int64 `json:"id"`
			} `json:"user"`
		}
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d&page=%d", g.Repo, g.Number, perPage, page)
		if err := g.do("GET", path, nil, &batch); err != nil {
			return nil, err
		}
		for _, c := range batch {
			out = append(out, Comment{ID: c.ID, Body: c.Body, AuthorID: c.User.ID})
		}
		if len(batch) < perPage {
			return out, nil
		}
	}
	return nil, &ForgeError{Status: 0, Detail: fmt.Sprintf("github comment listing exceeded %d pages — refusing to trust the server", maxPages)}
}

func (g *GitHub) CreateComment(body string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", g.Repo, g.Number)
	return g.do("POST", path, map[string]string{"body": body}, nil)
}

func (g *GitHub) UpdateComment(id int64, body string) error {
	path := "/repos/" + g.Repo + "/issues/comments/" + strconv.FormatInt(id, 10)
	return g.do("PATCH", path, map[string]string{"body": body}, nil)
}

func (g *GitHub) OpenPR(head, base, title, body string) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	err := g.do("POST", "/repos/"+g.Repo+"/pulls",
		map[string]string{"head": head, "base": base, "title": title, "body": body}, &out)
	return out.HTMLURL, err
}

// ForgeError carries the HTTP status so the caller can pick the degradation
// rung (403/404 → read-only token → job summary → stderr).
type ForgeError struct {
	Status int
	Detail string
}

func (e *ForgeError) Error() string { return e.Detail }

// ReadOnly reports whether the failure smells like a token without write
// access (the degradation-ladder trigger).
func (e *ForgeError) ReadOnly() bool { return e.Status == 401 || e.Status == 403 || e.Status == 404 }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
