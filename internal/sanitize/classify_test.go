package sanitize

import "testing"

func TestClassifyShortEnumFastPathPreservesKeyGuards(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value any
		want  Mode
	}{
		{name: "ordinary enum", key: "status", value: "pending", want: ModeAllow},
		{name: "long identifier-shaped value", key: "status", value: "pending_approval", want: ModeTokenize},
		{name: "secret substring", key: "authstatus", value: "pending", want: ModeSubstitute},
		{name: "secret boundary", key: "status_token", value: "pending", want: ModeSubstitute},
		{name: "one time code", key: "onetimecode", value: "pending", want: ModeSubstitute},
		{name: "uppercase value", key: "status", value: "ACTIVE", want: ModeTokenize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.key, tt.value, false); got != tt.want {
				t.Fatalf("Classify(%q, %q) = %s, want %s", tt.key, tt.value, got, tt.want)
			}
		})
	}
}
