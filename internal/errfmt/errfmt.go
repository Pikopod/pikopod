package errfmt

import "fmt"

const docsBase = "https://github.com/pikopod/pikopod/blob/main"

type E struct {
	What string
	Why  string
	Fix  string
	Docs string
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

func New(what, why, fix, docs string) error {
	return &E{What: what, Why: why, Fix: fix, Docs: docs}
}

func Newf(what, fix, docs, whyFormat string, args ...any) error {
	return &E{What: what, Why: fmt.Sprintf(whyFormat, args...), Fix: fix, Docs: docs}
}
