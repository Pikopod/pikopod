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

func TestClassifyShortEnumFastPathMatchesSlowClassifier(t *testing.T) {
	keys := []string{
		"api_key", "secret", "password", "passwd", "token", "credential", "auth", "signature", "private_key", "access_key", "bearer",
		"cvv", "cvv2", "cvc", "cvc2", "cid", "csc", "pin", "otp", "passcode", "security_code", "security_password", "security_pin", "one_time_code", "one_time_password", "one_time_pin", "transaction-pin",
		"id", "ids", "uuid", "guid", "email", "account", "customer", "user", "order", "ref",
		"name", "surname", "username", "nickname", "city", "street", "address", "dob", "birthdate", "birthday", "birth", "gender", "beneficiary", "sender", "recipient", "payee", "payer", "holder",
		"phone", "mobile", "msisdn", "card", "pan", "iban", "bvn", "nin", "ssn", "expiry", "expiration", "exp_month", "exp_year", "valid_thru", "valid_until",
		"status", "state", "mode", "result", "phase", "kind", "type", "region",
	}
	values := []string{"pending", "on_hold", "abc", "a.b/c", "x-y"}
	for _, key := range keys {
		for _, value := range values {
			t.Run(key+"/"+value, func(t *testing.T) {
				got := Classify(key, value, false)
				want := classifySlow(key, value)
				if got != want {
					t.Fatalf("Classify(%q, %q) = %s, classifySlow = %s", key, value, got, want)
				}
			})
		}
	}
}
