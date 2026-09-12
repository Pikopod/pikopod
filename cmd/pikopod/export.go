// Recording exports (curl, HAR 1.2). Everything exported is POST-SANITIZER —
// tokens and placeholders, never raw payloads — which is what makes it safe.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/proxy"
)

// renderCurl renders one sanitized record as curl against the upstream's real base URL.
func renderCurl(out io.Writer, target string, rec *proxy.Record) {
	var b strings.Builder
	b.WriteString("curl -X " + rec.Method + " " + shellQuote(strings.TrimRight(target, "/")+rec.Path))
	for _, k := range sortedHeaderKeys(rec.ReqHeader) {
		v, _ := rec.ReqHeader[k].(string)
		b.WriteString(" \\\n  -H " + shellQuote(k+": "+v))
	}
	if rec.ReqBody != nil {
		if raw, err := json.Marshal(rec.ReqBody); err == nil {
			b.WriteString(" \\\n  -d " + shellQuote(string(raw)))
		}
	}
	fmt.Fprintf(out, "# %s → %d (%dms)\n%s\n\n", rec.TS.Format(time.RFC3339), rec.Status, rec.DurMS, b.String())
}

// harArchive renders records as a minimal valid HAR 1.2 document.
func harArchive(target string, records []*proxy.Record) map[string]any {
	entries := make([]any, 0, len(records))
	for _, rec := range records {
		reqHeaders := make([]any, 0, len(rec.ReqHeader))
		for _, k := range sortedHeaderKeys(rec.ReqHeader) {
			v, _ := rec.ReqHeader[k].(string)
			reqHeaders = append(reqHeaders, map[string]any{"name": k, "value": v})
		}
		respHeaders := make([]any, 0, len(rec.RespHeader))
		for _, k := range sortedHeaderKeys(rec.RespHeader) {
			v, _ := rec.RespHeader[k].(string)
			respHeaders = append(respHeaders, map[string]any{"name": k, "value": v})
		}
		request := map[string]any{
			"method": rec.Method, "url": strings.TrimRight(target, "/") + rec.Path,
			"httpVersion": "HTTP/1.1", "headers": reqHeaders,
			"queryString": []any{}, "cookies": []any{},
			"headersSize": -1, "bodySize": rec.ReqSize,
		}
		if rec.ReqBody != nil {
			if raw, err := json.Marshal(rec.ReqBody); err == nil {
				request["postData"] = map[string]any{"mimeType": "application/json", "text": string(raw)}
			}
		}
		content := map[string]any{"size": rec.RespSize, "mimeType": "application/json"}
		if rec.RespBody != nil {
			if raw, err := json.Marshal(rec.RespBody); err == nil {
				content["text"] = string(raw)
			}
		}
		entries = append(entries, map[string]any{
			"startedDateTime": rec.TS.Format(time.RFC3339),
			"time":            rec.DurMS,
			"request":         request,
			"response": map[string]any{
				"status": rec.Status, "statusText": "", "httpVersion": "HTTP/1.1",
				"headers": respHeaders, "cookies": []any{}, "content": content,
				"redirectURL": "", "headersSize": -1, "bodySize": rec.RespSize,
			},
			"cache":   map[string]any{},
			"timings": map[string]any{"send": 0, "wait": rec.DurMS, "receive": 0},
		})
	}
	return map[string]any{"log": map[string]any{
		"version": "1.2",
		"creator": map[string]any{"name": "pikopod", "version": version, "comment": "post-sanitizer recordings — tokens, never raw payloads"},
		"entries": entries,
	}}
}

func sortedHeaderKeys(h map[string]any) []string {
	return slices.Sorted(maps.Keys(h))
}

// shellQuote single-quotes for POSIX shells (the only metacharacter inside
// single quotes is the single quote itself).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
