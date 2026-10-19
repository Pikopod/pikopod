package report

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/scenario"
)

const (
	FormatText          = "text"
	FormatJSON          = "json"
	FormatJUnit         = "junit"
	FormatGitHubActions = "githubactions"
)

func ParseFormat(s string) (string, error) {
	switch s {
	case "", FormatText:
		return FormatText, nil
	case FormatJSON, FormatJUnit, FormatGitHubActions:
		return s, nil
	}
	return "", errfmt.New("unknown --format", fmt.Sprintf("%q is not text, json, junit or githubactions", s), "pass --format text, json, junit or githubactions", "scenarios/README.md")
}

type Run struct {
	Name   string              `json:"name"`
	File   string              `json:"file,omitempty"`
	Result *scenario.RunResult `json:"result"`
}

type Document struct {
	Runs        []Run    `json:"runs"`
	Failed      int      `json:"failed"`
	Errored     int      `json:"errored"`
	Exercised   []string `json:"exercised,omitempty"`
	Unexercised []string `json:"unexercised,omitempty"`
}

func (d *Document) Add(name, file string, res *scenario.RunResult) {
	d.Runs = append(d.Runs, Run{Name: name, File: file, Result: res})
	switch res.Status {
	case scenario.RunFailed:
		d.Failed++
	case scenario.RunErrored:
		d.Errored++
	}
}

func (d *Document) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Tests   int          `xml:"tests,attr"`
	Failure int          `xml:"failures,attr"`
	Errors  int          `xml:"errors,attr"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Skipped  int         `xml:"skipped,attr"`
	File     string      `xml:"file,attr,omitempty"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Error     *junitMessage `xml:"error,omitempty"`
	Skipped   *struct{}     `xml:"skipped,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

func (d *Document) WriteJUnit(w io.Writer) error {
	root := junitSuites{}
	for _, run := range d.Runs {
		suite := junitSuite{Name: run.Name, File: run.File}
		for _, step := range run.Result.Steps {
			c := junitCase{Name: step.Key, ClassName: run.Name}
			switch step.Status {
			case scenario.RunFailed:
				c.Failure = &junitMessage{Message: step.Summary, Type: step.Type, Body: stepBody(step)}
				suite.Failures++
			case scenario.RunErrored:
				c.Error = &junitMessage{Message: step.Summary, Type: step.Type, Body: stepBody(step)}
				suite.Errors++
			case scenario.RunConditionGenerated, scenario.StatusNotEvaluated:
				c.Skipped = &struct{}{}
				suite.Skipped++
			}
			suite.Tests++
			suite.Cases = append(suite.Cases, c)
		}
		if len(run.Result.Steps) == 0 {
			c := junitCase{Name: run.Name, ClassName: run.Name}
			if run.Result.Status == scenario.RunFailed {
				c.Failure = &junitMessage{Message: run.Result.Summary}
				suite.Failures++
			} else if run.Result.Status == scenario.RunErrored {
				c.Error = &junitMessage{Message: run.Result.Summary}
				suite.Errors++
			}
			suite.Tests++
			suite.Cases = append(suite.Cases, c)
		}
		root.Tests += suite.Tests
		root.Failure += suite.Failures
		root.Errors += suite.Errors
		root.Suites = append(root.Suites, suite)
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(root); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func stepBody(step scenario.StepResult) string {
	if curl, _ := step.Detail["curl"].(string); curl != "" {
		return step.Summary + "\n" + curl
	}
	return step.Summary
}

func ghEscape(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

func ghProperty(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(s)
}

func (d *Document) WriteGitHubActions(w io.Writer) {
	for _, run := range d.Runs {
		lines := stepLines(run.File)
		for _, step := range run.Result.Steps {
			kind := ""
			switch step.Status {
			case scenario.RunFailed:
				kind = "error"
			case scenario.RunErrored:
				kind = "error"
			default:
				continue
			}
			props := "title=" + ghProperty(run.Name+" / "+step.Key)
			if run.File != "" {
				if line, ok := lines[step.Key]; ok {
					props = "file=" + ghProperty(run.File) + ",line=" + fmt.Sprint(line) + "," + props
				} else {
					props = "file=" + ghProperty(run.File) + "," + props
				}
			}
			fmt.Fprintf(w, "::%s %s::%s\n", kind, props, ghEscape(step.Summary))
		}
		if len(run.Result.Steps) == 0 && run.Result.Status != scenario.RunPassed {
			fmt.Fprintf(w, "::error title=%s::%s\n", ghProperty(run.Name), ghEscape(run.Result.Summary))
		}
	}
	if len(d.Unexercised) > 0 {
		fmt.Fprintf(w, "::notice title=%s::%s\n", ghProperty("unexercised operations"), ghEscape(strings.Join(d.Unexercised, ", ")))
	}
}

func stepLines(file string) map[string]int {
	out := map[string]int{}
	if file == "" {
		return out
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return out
	}
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-"))
		if key, ok := strings.CutPrefix(trimmed, "key:"); ok {
			key = strings.Trim(strings.TrimSpace(key), `"'`)
			if _, seen := out[key]; !seen {
				out[key] = i + 1
			}
		}
	}
	return out
}

func (d *Document) Exercise(def *ir.ApiDefinition) {
	hit := map[string]bool{}
	for _, run := range d.Runs {
		for _, step := range run.Result.Steps {
			if step.Type != "REQUEST" {
				continue
			}
			req, _ := step.Detail["request"].(map[string]any)
			method, _ := req["method"].(string)
			path, _ := req["path"].(string)
			if template, ok := templateFor(def, method, path); ok {
				hit[strings.ToUpper(method)+" "+template] = true
			}
		}
	}
	d.Exercised, d.Unexercised = nil, nil
	for i := range def.Endpoints {
		ep := &def.Endpoints[i]
		op := strings.ToUpper(ep.Method.Value) + " " + ep.PathTemplate.Value
		if hit[op] {
			d.Exercised = append(d.Exercised, op)
		} else {
			d.Unexercised = append(d.Unexercised, op)
		}
	}
	sort.Strings(d.Exercised)
	sort.Strings(d.Unexercised)
}

func (d *Document) ExercisedLine() string {
	total := len(d.Exercised) + len(d.Unexercised)
	line := fmt.Sprintf("exercised %d of %d declared operations", len(d.Exercised), total)
	if len(d.Unexercised) > 0 {
		line += " (unexercised: " + strings.Join(d.Unexercised, ", ") + ")"
	}
	return line
}

func templateFor(def *ir.ApiDefinition, method, path string) (string, bool) {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	want := strings.Split(strings.Trim(path, "/"), "/")
	for i := range def.Endpoints {
		ep := &def.Endpoints[i]
		if !strings.EqualFold(ep.Method.Value, method) {
			continue
		}
		have := strings.Split(strings.Trim(ep.PathTemplate.Value, "/"), "/")
		if len(have) != len(want) {
			continue
		}
		ok := true
		for j := range have {
			if !strings.HasPrefix(have[j], "{") && have[j] != want[j] {
				ok = false
				break
			}
		}
		if ok {
			return ep.PathTemplate.Value, true
		}
	}
	return "", false
}
