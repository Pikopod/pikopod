// GitLab forge over the REST API (merge-request notes). Pagination follows
// pages to the end, not just the first page of notes.
package pr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type GitLab struct {
	BaseURL string // default https://gitlab.com/api/v4
	Project string // numeric id or url-encoded path
	Number  int    // MR iid
	Token   string
	Client  *http.Client
}

func (g *GitLab) Name() string { return "gitlab" }

func (g *GitLab) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (g *GitLab) base() string {
	if g.BaseURL != "" {
		return g.BaseURL
	}
	return "https://gitlab.com/api/v4"
}

func (g *GitLab) project() string { return url.PathEscape(g.Project) }

func (g *GitLab) do(method, path string, in any, out any) error {
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
	if g.Token != "" {
		req.Header.Set("PRIVATE-TOKEN", g.Token)
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
		return &ForgeError{Status: resp.StatusCode, Detail: fmt.Sprintf("gitlab %s %s: %s", method, path, truncate(string(raw), 200))}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (g *GitLab) CurrentUserID() (int64, error) {
	var u struct {
		ID int64 `json:"id"`
	}
	if err := g.do("GET", "/user", nil, &u); err != nil {
		return 0, err
	}
	return u.ID, nil
}

func (g *GitLab) ListComments() ([]Comment, error) {
	var out []Comment
	for page := 1; page <= maxPages; page++ {
		var batch []struct {
			ID     int64  `json:"id"`
			Body   string `json:"body"`
			Author struct {
				ID int64 `json:"id"`
			} `json:"author"`
		}
		path := fmt.Sprintf("/projects/%s/merge_requests/%d/notes?per_page=%d&page=%d", g.project(), g.Number, perPage, page)
		if err := g.do("GET", path, nil, &batch); err != nil {
			return nil, err
		}
		for _, c := range batch {
			out = append(out, Comment{ID: c.ID, Body: c.Body, AuthorID: c.Author.ID})
		}
		if len(batch) < perPage {
			return out, nil
		}
	}
	return nil, &ForgeError{Status: 0, Detail: fmt.Sprintf("gitlab note listing exceeded %d pages — refusing to trust the server", maxPages)}
}

func (g *GitLab) CreateComment(body string) error {
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/notes", g.project(), g.Number)
	return g.do("POST", path, map[string]string{"body": body}, nil)
}

func (g *GitLab) UpdateComment(id int64, body string) error {
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/notes/%s", g.project(), g.Number, strconv.FormatInt(id, 10))
	return g.do("PUT", path, map[string]string{"body": body}, nil)
}

func (g *GitLab) OpenPR(head, base, title, body string) (string, error) {
	var out struct {
		WebURL string `json:"web_url"`
	}
	err := g.do("POST", "/projects/"+g.project()+"/merge_requests",
		map[string]string{"source_branch": head, "target_branch": base, "title": title, "description": body}, &out)
	return out.WebURL, err
}
