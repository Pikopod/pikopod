package docimport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/scenario/nl"
)

type Fetcher func(url string) ([]byte, error)

type Result struct {
	Spec   []byte
	Method string
	Source string

	Skipped []string
}

const (
	maxHopPages       = 8
	maxCorpusBytes    = 240 << 10
	maxPageBytes      = 2 << 20
	llmMaxTokens      = 64000
	maxBatchBytes     = 36 << 10
	llmExtractTimeout = 8 * time.Minute
	llmInstruction    = "You convert REST API documentation text into ONE OpenAPI 3.0 JSON document. The pages may cover only PART of the API — extract exactly what these pages describe; other pages are handled separately. Include ONLY endpoints, parameters, request/response fields, auth schemes, and status codes explicitly described in the text — NEVER invent endpoints or fields. Use best-effort JSON schemas from described fields and examples. servers: use the base URL if stated. WEBHOOKS: when the documentation describes webhook events, add a top-level \"webhooks\" object — one key per documented EVENT NAME (e.g. \"payment_intent.completed\"), each {\"post\": {\"requestBody\": {\"content\": {\"application/json\": {\"schema\": <the documented payload schema>}}}, \"responses\": {\"200\": {\"description\": \"ack\"}}, \"x-pikopod-trigger\": {\"method\": \"<http method>\", \"path\": \"<endpoint path>\"}}} where x-pikopod-trigger names the API operation the docs say fires that event (omit it when the docs do not say). Output ONLY the JSON document."
)

var (
	documenterLinkRe = regexp.MustCompile(`href="(https?://[^"]+/api/collections/[^"]+)"`)
	specLinkRe       = regexp.MustCompile(`(?:href|src)="([^"]+(?:openapi|swagger)[^"]*\.(?:json|ya?ml)[^"]*)"`)
	apiDocsLinkRe    = regexp.MustCompile(`(?:href|src)="([^"]+/api-docs[^"]*)"`)
	referenceLinkRe  = regexp.MustCompile(`href="(/reference(?:/[A-Za-z0-9_-]+)?)"`)
	docPageLinkRe    = regexp.MustCompile(`href="(/(?:docs|reference|api|guides)/[A-Za-z0-9_/-]+)"`)
	tagRe            = regexp.MustCompile(`(?s)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>|<nav[^>]*>.*?</nav>|<svg[^>]*>.*?</svg>`)
	anyTagRe         = regexp.MustCompile(`<[^>]{1,512}>`)
	wsRe             = regexp.MustCompile(`[ \t]{2,}|\r`)
	manyNLRe         = regexp.MustCompile(`\n{3,}`)
)

func FromDocsURL(pageURL string, html []byte, fetch Fetcher, llm *nl.Client) (*Result, error) {

	for _, probe := range []struct {
		re     *regexp.Regexp
		method string
	}{
		{documenterLinkRe, "postman-documenter"},
		{specLinkRe, "spec-link"},
		{apiDocsLinkRe, "spec-link"},
	} {
		if link := firstLink(probe.re, html, pageURL); link != "" {

			raw, err := fetch(link)
			if err == nil && len(raw) > 0 && !looksLikeHTML(raw) {
				return &Result{Spec: raw, Method: probe.method, Source: link}, nil
			}
		}
	}

	if spec := extractEmbeddedOpenAPI(html); spec != nil {
		return &Result{Spec: spec, Method: "readme-embedded", Source: pageURL}, nil
	}
	for _, ref := range hopLinks(referenceLinkRe, html, pageURL, 3) {
		raw, err := fetch(ref)
		if err != nil {
			continue
		}
		if spec := extractEmbeddedOpenAPI(raw); spec != nil {
			return &Result{Spec: spec, Method: "readme-embedded", Source: ref}, nil
		}

		for _, deep := range hopLinks(referenceLinkRe, raw, ref, 3) {
			if deep == ref {
				continue
			}
			deepRaw, err := fetch(deep)
			if err != nil {
				continue
			}
			if spec := extractEmbeddedOpenAPI(deepRaw); spec != nil {
				return &Result{Spec: spec, Method: "readme-embedded", Source: deep}, nil
			}
		}
		break
	}

	if res := wellKnownSpec(pageURL, fetch); res != nil {
		return res, nil
	}

	if llm == nil {
		return nil, errfmt.New(
			"this documentation page needs model-assisted extraction",
			pageURL+" embeds no machine-readable spec pikopod recognizes",
			"set llm.api_key (or OPENROUTER_API_KEY / OPENAI_API_KEY) to enable Tier-C extraction, or point --spec at a spec/collection directly",
			"docs/config-reference.md#llm")
	}
	return llmExtract(pageURL, html, fetch, llm)
}

type page struct {
	URL  string `json:"url"`
	Text string `json:"text"`
}

func (p page) size() int    { return len(p.Text) }
func (p page) name() string { return p.URL }

func llmExtract(pageURL string, html []byte, fetch Fetcher, llm *nl.Client) (*Result, error) {
	var corpus []page
	size := 0
	add := func(url, text string) {
		if len(strings.TrimSpace(text)) == 0 || size >= maxCorpusBytes {
			return
		}
		if len(text) < 120 && len(corpus) > 0 {
			return
		}
		if size+len(text) > maxCorpusBytes {
			text = text[:maxCorpusBytes-size]
		}
		corpus = append(corpus, page{URL: url, Text: text})
		size += len(text)
	}

	var skipped []string
	if groups := llmsTxtGroups(pageURL, fetch); len(groups) > 0 {
		var fetched [][]page
		for _, group := range groups {
			var pages []page
			for _, link := range group {
				raw, err := fetch(link)
				if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
					continue
				}
				pages = append(pages, page{URL: link, Text: string(raw)})
			}
			fetched = append(fetched, pages)
		}
		chosen, left := planCorpus(fetched, maxCorpusBytes, corpusShares)
		for _, p := range chosen {
			add(p.URL, p.Text)
		}
		skipped = left
	}
	if len(corpus) == 0 {
		add(pageURL, htmlToText(html))
		for _, link := range hopLinks(docPageLinkRe, html, pageURL, maxHopPages) {
			if size >= maxCorpusBytes {
				break
			}
			raw, err := fetch(link)
			if err != nil {
				continue
			}
			add(link, htmlToText(raw))
		}
	}

	llm.MaxTokens = llmMaxTokens

	llm.Stream = true
	llm.HTTPClient = &http.Client{
		Timeout:   llmExtractTimeout,
		Transport: &http.Transport{TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}},
	}

	var merged map[string]any
	extracted, failed := 0, 0
	for _, batch := range batchPages(corpus, maxBatchBytes) {
		payload, err := json.Marshal(map[string]any{"documentation_pages": batch})
		if err != nil {
			return nil, err
		}
		candidate, err := llm.CompleteJSON(context.Background(), llmInstruction, string(payload))
		if err != nil {
			failed++
			continue
		}
		doc, ok := candidate.(map[string]any)
		if !ok {
			failed++
			continue
		}
		extracted++
		if merged == nil {
			merged = doc
			continue
		}
		mergeMapField(merged, doc, "paths")
		mergeMapField(merged, doc, "webhooks")
		mergeComponents(merged, doc)
		for _, k := range []string{"servers", "security"} {
			if _, has := merged[k]; !has {
				if v, ok := doc[k]; ok {
					merged[k] = v
				}
			}
		}
	}
	if merged == nil {
		return nil, errfmt.New("model-assisted extraction produced nothing", fmt.Sprintf("%d batch(es) all failed", failed), "retry, or try another llm.model", "docs/config-reference.md#llm")
	}
	cleanPathKeys(merged)

	inlineRequestBodyRefs(merged)
	spec, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return &Result{Spec: spec, Method: "llm-extracted", Source: fmt.Sprintf("%s (+%d pages, %d/%d batches)", pageURL, len(corpus)-1, extracted, extracted+failed), Skipped: skipped}, nil
}

func batchPages[T any](pages []T, limit int) [][]T {
	sizeOf := func(p T) int {
		raw, _ := json.Marshal(p)
		return len(raw)
	}
	var out [][]T
	var cur []T
	size := 0
	for _, p := range pages {
		n := sizeOf(p)
		if len(cur) > 0 && size+n > limit {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, p)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func mergeComponents(dst, src map[string]any) {
	sc, ok := src["components"].(map[string]any)
	if !ok {
		return
	}
	dc, ok := dst["components"].(map[string]any)
	if !ok {
		dst["components"] = sc
		return
	}
	for k, v := range sc {
		sub, ok := v.(map[string]any)
		if !ok {
			continue
		}
		dsub, ok := dc[k].(map[string]any)
		if !ok {
			dc[k] = sub
			continue
		}
		for kk, vv := range sub {
			if _, exists := dsub[kk]; !exists {
				dsub[kk] = vv
			}
		}
	}
}

var corpusShares = []float64{0.3, 0.5, 0.2}

func llmsTxtGroups(pageURL string, fetch Fetcher) [][]string {
	u, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	raw, err := fetch(u.Scheme + "://" + u.Host + "/llms.txt")
	if err != nil || looksLikeHTML(raw) {
		return nil
	}
	var apiPages, hookPages, rest []string
	total := 0
	for _, m := range llmsLinkRe.FindAllStringSubmatch(string(raw), -1) {
		link := m[1]
		lu, err := url.Parse(link)
		if err != nil || lu.Host != u.Host {
			continue
		}
		if total >= 3*maxHopPages {
			break
		}
		total++
		switch {
		case strings.Contains(link, "webhook") || strings.Contains(link, "event-types") || strings.Contains(link, "/events"):
			hookPages = append(hookPages, link)
		case strings.Contains(link, "api-reference") || strings.Contains(link, "/reference"):
			apiPages = append(apiPages, link)
		default:
			rest = append(rest, link)
		}
	}
	if total == 0 {
		return nil
	}
	return [][]string{hookPages, apiPages, rest}
}

func planCorpus[P interface {
	size() int
	name() string
}](groups [][]P, budget int, shares []float64) (chosen []P, skipped []string) {
	type slot struct {
		p     P
		group int
	}
	var order []slot
	taken := map[int]map[int]bool{}
	used := 0
	for g := range groups {
		taken[g] = map[int]bool{}
		reserve := budget
		if g < len(shares) {
			reserve = int(float64(budget) * shares[g])
		}
		spent := 0
		for i, p := range groups[g] {
			if spent+p.size() > reserve || used+p.size() > budget {
				break
			}
			taken[g][i] = true
			order = append(order, slot{p, g})
			spent += p.size()
			used += p.size()
		}
	}
	for g := range groups {
		for i, p := range groups[g] {
			if taken[g][i] {
				continue
			}
			if used+p.size() <= budget {
				taken[g][i] = true
				order = append(order, slot{p, g})
				used += p.size()
			}
		}
	}
	for _, s := range order {
		chosen = append(chosen, s.p)
	}
	for g := range groups {
		for i, p := range groups[g] {
			if !taken[g][i] {
				skipped = append(skipped, p.name())
			}
		}
	}
	return chosen, skipped
}

var wellKnownPaths = []string{
	"/openapi.json", "/openapi.yaml", "/openapi.yml", "/swagger.json", "/swagger.yaml",
	"/api-reference/openapi.json", "/api/openapi.json", "/v3/api-docs", "/api-docs", "/.well-known/openapi.json",
}

var llmsSpecLinkRe = regexp.MustCompile(`\((https?://[^)\s]*(?:openapi|swagger)[^)\s]*\.(?:json|ya?ml))\)`)

func wellKnownSpec(pageURL string, fetch Fetcher) *Result {
	u, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	origin := u.Scheme + "://" + u.Host
	var candidates []string
	if raw, err := fetch(origin + "/llms.txt"); err == nil && !looksLikeHTML(raw) {
		for _, m := range llmsSpecLinkRe.FindAllStringSubmatch(string(raw), -1) {
			candidates = append(candidates, m[1])
		}
	}
	for _, p := range wellKnownPaths {
		candidates = append(candidates, origin+p)
	}
	for i, link := range candidates {
		if i >= 12 {
			break
		}
		raw, err := fetch(link)
		if err != nil || len(raw) == 0 || looksLikeHTML(raw) || !looksLikeSpec(raw) {
			continue
		}
		return &Result{Spec: raw, Method: "well-known-spec", Source: link}
	}
	return nil
}

var specMarkerRe = regexp.MustCompile(`(?m)(^\s*(openapi|swagger)\s*:|"(openapi|swagger)"\s*:)`)

func looksLikeSpec(raw []byte) bool {
	return specMarkerRe.Match(raw[:min(len(raw), 64<<10)])
}

var llmsLinkRe = regexp.MustCompile(`\]\((https?://[^)\s]+)\)`)

func extractEmbeddedOpenAPI(html []byte) []byte {
	s := string(html)
	var docs []map[string]any
	for _, marker := range []string{`"schema":{"openapi"`, `"schema":{"swagger"`, `"oasDefinition":{"openapi"`, `"oasDefinition":{"swagger"`} {
		from := 0
		for len(docs) < 4 {
			i := strings.Index(s[from:], marker)
			if i < 0 {
				break
			}
			start := from + i + strings.Index(marker, ":{") + 1
			blob, end := balancedJSON(s, start)
			if blob == "" {
				break
			}
			from = end
			var doc map[string]any
			if json.Unmarshal([]byte(blob), &doc) == nil && doc["paths"] != nil {
				docs = append(docs, doc)
			}
		}
	}
	if len(docs) == 0 {
		return nil
	}
	for _, d := range docs {
		cleanPathKeys(d)
	}
	merged := docs[0]
	for _, d := range docs[1:] {
		mergeMapField(merged, d, "paths")
		if c, ok := d["components"].(map[string]any); ok {
			mc, _ := merged["components"].(map[string]any)
			if mc == nil {
				merged["components"] = c
			} else {
				for k, v := range c {
					if sub, ok := v.(map[string]any); ok {
						msub, _ := mc[k].(map[string]any)
						if msub == nil {
							mc[k] = sub
						} else {
							for kk, vv := range sub {
								if _, exists := msub[kk]; !exists {
									msub[kk] = vv
								}
							}
						}
					}
				}
			}
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil
	}
	return out
}

func inlineRequestBodyRefs(doc map[string]any) {
	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	if schemas == nil {
		return
	}
	paths, _ := doc["paths"].(map[string]any)
	for _, item := range paths {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, op := range itemMap {
			opMap, ok := op.(map[string]any)
			if !ok {
				continue
			}
			rb, _ := opMap["requestBody"].(map[string]any)
			content, _ := rb["content"].(map[string]any)
			for _, media := range content {
				mediaMap, ok := media.(map[string]any)
				if !ok {
					continue
				}
				schema, _ := mediaMap["schema"].(map[string]any)
				ref, _ := schema["$ref"].(string)
				const prefix = "#/components/schemas/"
				if strings.HasPrefix(ref, prefix) {
					if target, ok := schemas[strings.TrimPrefix(ref, prefix)].(map[string]any); ok {
						mediaMap["schema"] = target
					}
				}
			}
		}
	}
}

func cleanPathKeys(doc map[string]any) {
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		return
	}
	cleaned := map[string]any{}
	for key, item := range paths {
		path, query := key, ""
		if i := strings.IndexByte(key, '?'); i >= 0 {
			path, query = key[:i], key[i+1:]
		}
		for len(path) > 1 && strings.HasSuffix(path, "/") {
			path = path[:len(path)-1]
		}
		if path == "" {
			path = "/"
		}
		itemMap, isMap := item.(map[string]any)
		if isMap && query != "" {
			addQueryParams(itemMap, query)
		}
		if existing, collide := cleaned[path].(map[string]any); collide && isMap {
			for m, op := range itemMap {
				if _, has := existing[m]; !has {
					existing[m] = op
				}
			}
			continue
		}
		cleaned[path] = item
	}
	doc["paths"] = cleaned
}

func addQueryParams(item map[string]any, query string) {
	var params []any
	for _, pair := range strings.Split(query, "&") {
		name, _, _ := strings.Cut(pair, "=")
		if name == "" {
			continue
		}
		params = append(params, map[string]any{
			"name": name, "in": "query",
			"schema": map[string]any{"type": "string"},
		})
	}
	if len(params) == 0 {
		return
	}
	for method, op := range item {
		opMap, ok := op.(map[string]any)
		if !ok {
			continue
		}
		switch method {
		case "get", "post", "put", "patch", "delete", "head", "options":
			existing, _ := opMap["parameters"].([]any)
			opMap["parameters"] = append(existing, params...)
		}
	}
}

func mergeMapField(dst, src map[string]any, field string) {
	sm, ok := src[field].(map[string]any)
	if !ok {
		return
	}
	dm, ok := dst[field].(map[string]any)
	if !ok {
		dst[field] = sm
		return
	}
	for k, v := range sm {
		if _, exists := dm[k]; !exists {
			dm[k] = v
		}
	}
}

func balancedJSON(s string, start int) (string, int) {
	if start >= len(s) || s[start] != '{' {
		return "", start
	}
	depth, instr, esc := 0, false, false
	for j := start; j < len(s) && j-start < maxPageBytes; j++ {
		c := s[j]
		if instr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				instr = false
			}
			continue
		}
		switch c {
		case '"':
			instr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : j+1], j + 1
			}
		}
	}
	return "", start
}

func firstLink(re *regexp.Regexp, html []byte, base string) string {
	m := re.FindSubmatch(html)
	if m == nil {
		return ""
	}
	return resolveLink(string(m[1]), base)
}

func hopLinks(re *regexp.Regexp, html []byte, base string, n int) []string {
	baseU, err := url.Parse(base)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllSubmatch(html, -1) {
		link := resolveLink(string(m[1]), base)
		if link == "" || seen[link] || link == base {
			continue
		}
		u, err := url.Parse(link)
		if err != nil || u.Host != baseU.Host {
			continue
		}
		seen[link] = true
		out = append(out, link)
		if len(out) >= n {
			break
		}
	}
	sort.Strings(out)
	return out
}

func resolveLink(link, base string) string {
	link = strings.ReplaceAll(link, "&amp;", "&")
	if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") {
		return link
	}
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	r, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return b.ResolveReference(r).String()
}

func looksLikeHTML(raw []byte) bool {
	head := strings.ToLower(string(raw[:min(len(raw), 512)]))
	return strings.Contains(head, "<!doctype html") || strings.Contains(head, "<html")
}

func htmlToText(html []byte) string {
	s := tagRe.ReplaceAllString(string(html), " ")
	s = anyTagRe.ReplaceAllString(s, "\n")
	s = wsRe.ReplaceAllString(s, " ")
	s = manyNLRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
