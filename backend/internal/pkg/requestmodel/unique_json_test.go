package requestmodel

import "testing"

func TestHasUniqueJSONMembers(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"messages":[{"content":[{"type":"text","text":"x"}]}]}`, true},
		{`{"a":{"x":1},"b":{"x":2}}`, true},
		{`{"a":{"x":1,"X":2}}`, true}, // Arbitrary JSON keys remain case-sensitive.
		{`{"input":{"n":1e1000,"integer":9007199254740993}}`, true},
		{`{"a":[{"x":1,"x":2}]}`, false},
		{`{"a":{"text":"safe","te\u0078t":"hidden"}}`, false},
		{`{"a":1} {"a":2}`, false},
		{`{"a":[}`, false},
		{``, false},
	} {
		if got := HasUniqueJSONMembers([]byte(tc.body)); got != tc.valid {
			t.Errorf("HasUniqueJSONMembers(%s)=%v, want %v", tc.body, got, tc.valid)
		}
	}
}
