// Package errfmt implements the one-line pikopod error contract, "<what>:
// <why> → <fix> → <docs URL>", required on every user-facing CLI error.
package errfmt

import "fmt"

const docsBase = "https://github.com/pikopod/pikopod/blob/main"

// E is a user-facing error carrying the four contract fields.
type E struct {
	What string // what failed, from the user's point of view
	Why  string // the cause, concretely
	Fix  string // the next action the user should take
	Docs string // absolute URL, or a repo-root-relative page path
}

func (e *E) Error() string {
	docs := e.Docs
	if docs != "" && docs[0] != 'h' {
		docs = docsBase + "/" + docs
	}
	s := fmt.Sprintf("%s: %s → %s", e.What, e.Why, e.Fix)
	if docs != "" {
		s += " → " + docs
	}
	return s
}

// New builds a contract error. docs may be "" (omitted), a full URL, or a
// repo-root-relative page like "docs/config-reference.md#listen".
func New(what, why, fix, docs string) error {
	return &E{What: what, Why: why, Fix: fix, Docs: docs}
}

// Newf is New with printf formatting applied to why.
func Newf(what, fix, docs, whyFormat string, args ...any) error {
	return &E{What: what, Why: fmt.Sprintf(whyFormat, args...), Fix: fix, Docs: docs}
}
