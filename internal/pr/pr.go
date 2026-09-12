// Package pr keeps ONE marker-tagged comment per finding source, updated in
// place, and opens branch+commit+PRs carrying a patch with its evidence.
package pr

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Comment is one existing PR/MR comment.
type Comment struct {
	ID       int64
	Body     string
	AuthorID int64
}

// Forge abstracts the two supported platforms (HTTP APIs, httptest-able).
type Forge interface {
	Name() string
	// CurrentUserID identifies the token's own user (authorship filter).
	CurrentUserID() (int64, error)
	ListComments() ([]Comment, error)
	CreateComment(body string) error
	UpdateComment(id int64, body string) error
	// OpenPR creates a pull/merge request and returns its URL.
	OpenPR(head, base, title, body string) (string, error)
}

// marker is the hidden HTML tag that identifies OUR comment per source.
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

// UpsertAction reports what UpsertComment did.
type UpsertAction string

const (
	Created   UpsertAction = "created"
	Updated   UpsertAction = "updated"
	Unchanged UpsertAction = "unchanged"
	StaleSkip UpsertAction = "stale-skip"
)

// UpsertComment posts or updates the ONE marker-tagged comment for a source.
// isAncestor(a, b) reports git ancestry; nil disables the staleness check.
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
		// Unverifiable authorship still attempts the update — skipping would
		// spam a comment per run, and a foreign marker fails as read-only.
		verified := uidErr == nil && c.AuthorID == uid
		if !verified && uidErr == nil {
			continue // authorship KNOWN foreign: never touch it
		}
		if c.Body == body {
			return Unchanged, nil
		}
		// Never let an old commit's re-run overwrite a newer comment: if OUR
		// sha is an ancestor of the existing one, the existing one is newer.
		if isAncestor != nil && sha != "" && existingSha != "" && sha != existingSha && isAncestor(sha, existingSha) {
			return StaleSkip, nil
		}
		if err := f.UpdateComment(c.ID, body); err != nil {
			if fe, isForge := err.(*ForgeError); isForge && !verified && fe.ReadOnly() {
				continue // unverifiable + not editable = someone else's; try the next marker or create
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

// GitIsAncestor reports git ancestry of a in b. Any git failure → false, so
// the staleness gate stands aside rather than block a legitimate update.
func GitIsAncestor(a, b string) bool {
	if strings.HasPrefix(a, "-") || strings.HasPrefix(b, "-") {
		return false
	}
	return exec.Command("git", "merge-base", "--is-ancestor", a, b).Run() == nil
}
