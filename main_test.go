package main

import (
	"testing"
	"time"
)

func TestSanitizeSubject(t *testing.T) {
	tests := map[string]string{
		"":                             "None",
		"   ":                          "None",
		"hello":                        "hello",
		"from abc123@example.com":      "from 123@example.com",
		"from ABCdef@example.com":      "from ABC@example.com",
		"multi a1@example.com b2@x.jp": "multi 1@example.com 2@x.jp",
	}

	for input, want := range tests {
		if got := sanitizeSubject(input); got != want {
			t.Fatalf("sanitizeSubject(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWithinWindow(t *testing.T) {
	loc := time.FixedZone("JST", 9*60*60)

	tests := []struct {
		name string
		t    time.Time
		want bool
	}{
		{name: "weekday at start", t: time.Date(2026, 9, 15, 6, 0, 0, 0, loc), want: true},
		{name: "weekday before start", t: time.Date(2026, 9, 15, 5, 59, 0, 0, loc), want: false},
		{name: "weekday before end", t: time.Date(2026, 9, 15, 21, 59, 0, 0, loc), want: true},
		{name: "weekday at end", t: time.Date(2026, 9, 15, 22, 0, 0, 0, loc), want: false},
		{name: "saturday", t: time.Date(2026, 9, 19, 12, 0, 0, 0, loc), want: false},
		{name: "sunday", t: time.Date(2026, 9, 20, 12, 0, 0, 0, loc), want: false},
	}

	for _, tt := range tests {
		if got := withinWindow(tt.t, 6, 22); got != tt.want {
			t.Fatalf("%s: withinWindow() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestRecipientMatches(t *testing.T) {
	tests := []struct {
		name    string
		headers string
		want    bool
	}{
		{name: "list post address", headers: "<mailto:staff@example.test>", want: true},
		{name: "list id", headers: "<staff.example.test>", want: true},
		{name: "x loop", headers: "staff@example.test", want: true},
		{name: "request address", headers: "staff-request@example.test", want: true},
		{name: "different list", headers: "<mailto:dmarc@example.test>\n<dmarc.example.test>", want: false},
	}

	for _, tt := range tests {
		if got := recipientMatches(tt.headers, "staff@example.test"); got != tt.want {
			t.Fatalf("%s: recipientMatches() = %v, want %v", tt.name, got, tt.want)
		}
	}
}
