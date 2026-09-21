package replay

import (
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/proxy"
	"github.com/pikopod/pikopod/internal/volatile"
)

func TestGateAndLoadShareOneMatcher(t *testing.T) {
	dir := t.TempDir()
	writeRecordings(t, dir, "pay", []proxy.Record{{Method: "GET", Path: "/x", Status: 200, RespKind: "json", RespBody: body(`{"ok":true}`)}})
	set, err := Load(dir, "pay", []string{"status", "meta/ref"})
	if err != nil {
		t.Fatal(err)
	}
	gateM, _, err := volatile.Compile([]string{"status", "meta/ref"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"status", "payment/status", "meta/ref", "data/meta/ref", "ref", "amount", "items[]/status"} {
		_, gate := gateM.Match(path)
		_, load := set.volatile.Match(path)
		if gate != load {
			t.Fatalf("%s: gate=%v load=%v", path, gate, load)
		}
	}
	stripped := set.stripVolatile(body(`{"payment":{"status":"x","amount":1},"meta":{"ref":"r","ok":true},"idempotency_key":"k"}`), "")
	got := canonicalJSON(stripped)
	for _, gone := range []string{"status", `"ref"`, "idempotency_key"} {
		if strings.Contains(got, gone) {
			t.Fatalf("%s must be stripped before hashing: %s", gone, got)
		}
	}
	if !strings.Contains(got, "amount") || !strings.Contains(got, `"ok"`) {
		t.Fatalf("unrelated fields must survive: %s", got)
	}
	drops := set.volatile.Drops()
	if len(drops) != 3 || !strings.HasPrefix(drops[0].Entry, "curated:") {
		t.Fatalf("drops attribute curated and user entries apart: %+v", drops)
	}
	if _, err := Load(dir, "pay", []string{"bad entry"}); err == nil {
		t.Fatal("a malformed entry is a configuration error, not a silent skip")
	}
}
