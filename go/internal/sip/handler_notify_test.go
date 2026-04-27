package sip

import "testing"

func TestIsReferEvent(t *testing.T) {
	tests := []struct {
		name  string
		event string
		want  bool
	}{
		{name: "exact", event: "refer", want: true},
		{name: "case-insensitive", event: "ReFeR", want: true},
		{name: "with-params", event: "refer;id=1", want: true},
		{name: "with-spaces", event: "  refer ;id=1 ", want: true},
		{name: "other-event", event: "message-summary", want: false},
		{name: "empty", event: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReferEvent(tc.event); got != tc.want {
				t.Fatalf("isReferEvent(%q) = %v, want %v", tc.event, got, tc.want)
			}
		})
	}
}

func TestShouldHangupOnReferNotify(t *testing.T) {
	tests := []struct {
		name       string
		event      string
		code       int
		hasSIPFrag bool
		want       bool
	}{
		{name: "refer-trying", event: "refer", code: 100, hasSIPFrag: true, want: false},
		{name: "refer-ringing", event: "refer", code: 180, hasSIPFrag: true, want: false},
		{name: "refer-success", event: "refer", code: 200, hasSIPFrag: true, want: true},
		{name: "refer-accepted", event: "refer;id=1", code: 202, hasSIPFrag: true, want: true},
		{name: "refer-failed", event: "refer", code: 486, hasSIPFrag: true, want: false},
		{name: "refer-no-sipfrag", event: "refer", code: 0, hasSIPFrag: false, want: false},
		{name: "non-refer-success", event: "message-summary", code: 200, hasSIPFrag: true, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldHangupOnReferNotify(tc.event, tc.code, tc.hasSIPFrag); got != tc.want {
				t.Fatalf("shouldHangupOnReferNotify(%q, %d, %v) = %v, want %v", tc.event, tc.code, tc.hasSIPFrag, got, tc.want)
			}
		})
	}
}
