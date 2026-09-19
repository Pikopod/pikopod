package archetype

import (
	"encoding/json"
	"testing"
)

// Extensions are not covered by the parity goldens (those iterate Catalogue),
// so the claims each extension archetype makes are pinned here instead.
func extensionByID(t *testing.T, id string) *Archetype {
	t.Helper()
	for i := range Extensions {
		if Extensions[i].ID == id {
			return &Extensions[i]
		}
	}
	t.Fatalf("extension archetype %q not found", id)
	return nil
}

func stepsOf(t *testing.T, a *Archetype) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(a.Expands)
	if err != nil {
		t.Fatal(err)
	}
	var steps []map[string]any
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatal(err)
	}
	return steps
}

func stepByKey(t *testing.T, a *Archetype, key string) map[string]any {
	t.Helper()
	for _, s := range stepsOf(t, a) {
		if s["key"] == key {
			return s
		}
	}
	t.Fatalf("%s has no step %q", a.ID, key)
	return nil
}

func assertsTarget(step map[string]any, target string) bool {
	raw, _ := step["assertions"].([]any)
	for _, item := range raw {
		if a, ok := item.(map[string]any); ok && a["target"] == target {
			return true
		}
	}
	return false
}

// Both failing requests must prove the fault actually applied, the way
// declines and timeouts do. A 503 that arrived for some other reason would
// otherwise satisfy the archetype.
func TestDowntimeRecoveryProvesTheFaultApplied(t *testing.T) {
	a := extensionByID(t, "downtime_recovery")
	for _, key := range []string{"down", "still-down"} {
		if !assertsTarget(stepByKey(t, a, key), "execution.faultApplied") {
			t.Errorf("step %q must assert execution.faultApplied", key)
		}
	}
}

// Recovery must be shown to follow the outage window, not merely to succeed.
func TestDowntimeRecoveryProvesTheOutageWindow(t *testing.T) {
	a := extensionByID(t, "downtime_recovery")
	step := stepByKey(t, a, "outage-shape")
	if step["type"] != "VERIFY_SEQUENCE" {
		t.Fatalf("outage-shape is %v, want VERIFY_SEQUENCE", step["type"])
	}
	cfg, _ := step["config"].(map[string]any)
	reqs, _ := cfg["requests"].([]any)
	if len(reqs) != 3 {
		t.Fatalf("want three ordered requests (down, still-down, recovered), got %d", len(reqs))
	}
	last, _ := reqs[2].(map[string]any)
	gap, ok := last["minGapMs"].(float64)
	if !ok {
		t.Fatal("the recovery matcher must carry minGapMs")
	}
	wait, _ := stepByKey(t, a, "outage-window")["config"].(map[string]any)
	if window, _ := wait["durationMs"].(float64); gap != window {
		t.Fatalf("minGapMs %v must equal the outage window %v", gap, window)
	}
}
