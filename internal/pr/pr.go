package pr

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

type Comment struct {
	ID       int64
	Body     string
	AuthorID int64
}

type Forge interface {
	Name() string

	CurrentUserID() (int64, error)
	ListComments() ([]Comment, error)
	CreateComment(body string) error
	UpdateComment(id int64, body string) error

	OpenPR(head, base, title, body string) (string, error)
}

func marker(source, sha string) string {
	return fmt.Sprintf("<!-- pikopod:comment source=%s sha=%s -->", source, sha)
}

var markerRE = regexp.MustCompile(`<!-- pikopod:comment source=([^ ]+) sha=([^ ]*) -->`)

func parseMarker(body string) (source, sha string, ok bool) {
	m := markerRE.FindStringSubmatch(body)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

type UpsertAction string

const (
	Created   UpsertAction = "created"
	Updated   UpsertAction = "updated"
	Unchanged UpsertAction = "unchanged"
	StaleSkip UpsertAction = "stale-skip"
)

func UpsertComment(f Forge, source, sha, markdown string, isAncestor func(a, b string) bool) (UpsertAction, error) {
	body := marker(source, sha) + "\n" + markdown

	uid, uidErr := f.CurrentUserID()
	comments, err := f.ListComments()
	if err != nil {
		return "", err
	}
	for _, c := range comments {
		src, existingSha, ok := parseMarker(c.Body)
		if !ok || src != source {
			continue
		}

		verified := uidErr == nil && c.AuthorID == uid
		if !verified && uidErr == nil {
			continue
		}
		if c.Body == body {
			return Unchanged, nil
		}

		if isAncestor != nil && sha != "" && existingSha != "" && sha != existingSha && isAncestor(sha, existingSha) {
			return StaleSkip, nil
		}
		if err := f.UpdateComment(c.ID, body); err != nil {
			if fe, isForge := err.(*ForgeError); isForge && !verified && fe.ReadOnly() {
				continue
			}
			return "", err
		}
		return Updated, nil
	}
	if err := f.CreateComment(body); err != nil {
		return "", err
	}
	return Created, nil
}

func GitIsAncestor(a, b string) bool {
	if strings.HasPrefix(a, "-") || strings.HasPrefix(b, "-") {
		return false
	}
	return exec.Command("git", "merge-base", "--is-ancestor", a, b).Run() == nil
}
