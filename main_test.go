package main

import (
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

func TestParseWebhookURLs(t *testing.T) {
	got, err := parseWebhookURLs("")
	if err != nil || got != nil {
		t.Fatalf("parseWebhookURLs(\"\") = %v, %v, want nil, nil", got, err)
	}

	got, err = parseWebhookURLs("urgent=https://discord.com/api/webhooks/1, newsletter = https://discord.com/api/webhooks/2 ")
	if err != nil {
		t.Fatalf("parseWebhookURLs() error = %v", err)
	}
	want := map[string]string{
		"urgent":     "https://discord.com/api/webhooks/1",
		"newsletter": "https://discord.com/api/webhooks/2",
	}
	if len(got) != len(want) {
		t.Fatalf("parseWebhookURLs() = %v, want %v", got, want)
	}
	for name, url := range want {
		if got[name] != url {
			t.Fatalf("parseWebhookURLs()[%q] = %q, want %q", name, got[name], url)
		}
	}

	if _, err := parseWebhookURLs("urgent"); err == nil {
		t.Fatal("parseWebhookURLs(\"urgent\") expected error, got nil")
	}
	if _, err := parseWebhookURLs("=https://discord.com/api/webhooks/1"); err == nil {
		t.Fatal("parseWebhookURLs() with empty name expected error, got nil")
	}
	if _, err := parseWebhookURLs("skip=https://discord.com/api/webhooks/1"); err == nil {
		t.Fatal("parseWebhookURLs() with reserved name \"skip\" expected error, got nil")
	}
}

func TestResolveTarget(t *testing.T) {
	requestKeyword := regexp.MustCompile(`(お願い|トラブル|していただ)`)

	routes := []discordRoute{
		{
			// スキップ条件: ドメインホワイトリスト外，またはブラックリスト
			Conditions: []routeCondition{
				{Field: "from", Regex: regexp.MustCompile(`@blocked\.example$`)},
			},
			Target: skipTarget,
		},
		{
			Conditions: []routeCondition{
				{Field: "from", Regex: regexp.MustCompile(`@(?:[^@]*\.)?nagoya-u\.ac\.jp$`), Negate: true},
				{Field: "from", Regex: regexp.MustCompile(`@coop\.`), Negate: true},
			},
			Target: skipTarget,
		},
		{
			Conditions: []routeCondition{
				{Field: "from", Regex: regexp.MustCompile(`^managers@nagoya-u\.ac\.jp$`)},
			},
			Target: "security-info",
		},
		{
			Conditions: []routeCondition{
				{Field: "from", Regex: regexp.MustCompile(`^nagios@.*coop\.nagoya-u\.ac\.jp$`)},
			},
			Target: "mail-alert",
		},
		{
			Conditions: []routeCondition{
				{Field: "from", Regex: regexp.MustCompile(`^staff@`)},
			},
			Target: "mail-alert",
		},
		{
			Conditions: []routeCondition{
				{Field: "text", Regex: requestKeyword},
			},
			Target: "work-log",
		},
	}

	tests := []struct {
		name string
		msg  messageHeader
		want string
	}{
		{
			name: "blacklisted sender is skipped",
			msg:  messageHeader{From: "spam@blocked.example", Subject: "お願い", Body: ""},
			want: skipTarget,
		},
		{
			name: "domain outside whitelist is skipped",
			msg:  messageHeader{From: "someone@outside.example", Subject: "こんにちは", Body: ""},
			want: skipTarget,
		},
		{
			name: "coop subdomain is not skipped",
			msg:  messageHeader{From: "nagios@monitor.coop.nagoya-u.ac.jp", Subject: "HOST DOWN", Body: ""},
			want: "mail-alert",
		},
		{
			name: "manager security info",
			msg:  messageHeader{From: "managers@nagoya-u.ac.jp", Subject: "セキュリティアップデート", Body: ""},
			want: "security-info",
		},
		{
			name: "staff mail alert",
			msg:  messageHeader{From: "staff@nagoya-u.ac.jp", Subject: "管理システム更新", Body: ""},
			want: "mail-alert",
		},
		{
			name: "keyword based work log",
			msg:  messageHeader{From: "kumagai@nagoya-u.ac.jp", Subject: "資料作成のお願い", Body: ""},
			want: "work-log",
		},
		{
			name: "no rule matches",
			msg:  messageHeader{From: "kumagai@nagoya-u.ac.jp", Subject: "共有です", Body: "資料を共有します"},
			want: "",
		},
	}

	for _, tt := range tests {
		if got := resolveTarget(routes, tt.msg); got != tt.want {
			t.Fatalf("%s: resolveTarget() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestLoadDiscordRoutes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "discord_routes.txt")
	lines := []string{
		"# comment line",
		"",
		"from:@blocked\\.example\tskip",
		"!from:\\.nagoya-u\\.ac\\.jp$\t!from:@coop\\.\tskip",
		"from:^managers@nagoya-u\\.ac\\.jp$\tsecurity-info",
		"text:(お願い|トラブル|していただ)\twork-log",
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write route file: %v", err)
	}

	routes, err := loadDiscordRoutes(path)
	if err != nil {
		t.Fatalf("loadDiscordRoutes() error = %v", err)
	}
	if len(routes) != 4 {
		t.Fatalf("loadDiscordRoutes() returned %d routes, want 4", len(routes))
	}
	if len(routes[1].Conditions) != 2 || !routes[1].Conditions[0].Negate || routes[1].Target != skipTarget {
		t.Fatalf("routes[1] = %+v, want 2 negated conditions and target %q", routes[1], skipTarget)
	}

	missing, err := loadDiscordRoutes(filepath.Join(dir, "does-not-exist.txt"))
	if err != nil || missing != nil {
		t.Fatalf("loadDiscordRoutes(missing file) = %v, %v, want nil, nil", missing, err)
	}
}

func TestRouteFieldValueHeader(t *testing.T) {
	msg := messageHeader{
		From: "staff-sender@coop.nagoya-u.ac.jp",
		Header: textproto.MIMEHeader{
			"X-Original-From": []string{"managers@nagoya-u.ac.jp"},
		},
	}
	value, ok := routeFieldValue("header:X-Original-From", msg)
	if !ok || value != "managers@nagoya-u.ac.jp" {
		t.Fatalf("routeFieldValue() = %q, %v, want %q, true", value, ok, "managers@nagoya-u.ac.jp")
	}

	if _, ok := routeFieldValue("header:", msg); ok {
		t.Fatal("routeFieldValue(\"header:\") expected ok=false")
	}

	value, ok = routeFieldValue("header:X-Nonexistent", msg)
	if !ok || value != "" {
		t.Fatalf("routeFieldValue() for missing header = %q, %v, want empty string, true", value, ok)
	}
}

func TestResolveTargetWithHeaderFallback(t *testing.T) {
	routes := []discordRoute{
		{
			Conditions: []routeCondition{
				{Field: "header:X-Original-From", Regex: regexp.MustCompile(`^managers@nagoya-u\.ac\.jp$`)},
			},
			Target: "B",
		},
		{
			Conditions: []routeCondition{
				{Field: "from", Regex: regexp.MustCompile(`^managers@nagoya-u\.ac\.jp$`)},
			},
			Target: "B",
		},
	}

	// メーリングリスト経由でFromが書き換わっているケース（X-Original-Fromで判定）
	viaML := messageHeader{
		From: "staff-sender@coop.nagoya-u.ac.jp",
		Header: textproto.MIMEHeader{
			"X-Original-From": []string{"managers@nagoya-u.ac.jp"},
		},
	}
	if got := resolveTarget(routes, viaML); got != "B" {
		t.Fatalf("resolveTarget() via mailing list = %q, want %q", got, "B")
	}

	// 直接送信されており，X-Original-Fromが存在しないケース（fromで判定）
	direct := messageHeader{From: "managers@nagoya-u.ac.jp", Header: textproto.MIMEHeader{}}
	if got := resolveTarget(routes, direct); got != "B" {
		t.Fatalf("resolveTarget() direct = %q, want %q", got, "B")
	}
}

func TestLoadDiscordRoutesHeaderField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.txt")
	content := "header:X-Original-From:^managers@nagoya-u\\.ac\\.jp$\tB\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write route file: %v", err)
	}
	routes, err := loadDiscordRoutes(path)
	if err != nil {
		t.Fatalf("loadDiscordRoutes() error = %v", err)
	}
	if len(routes) != 1 || routes[0].Conditions[0].Field != "header:X-Original-From" {
		t.Fatalf("routes = %+v, want header:X-Original-From condition", routes)
	}

	emptyHeaderPath := filepath.Join(dir, "empty_header.txt")
	if err := os.WriteFile(emptyHeaderPath, []byte("header:\tB\n"), 0600); err != nil {
		t.Fatalf("write route file: %v", err)
	}
	if _, err := loadDiscordRoutes(emptyHeaderPath); err == nil {
		t.Fatal("loadDiscordRoutes() with empty header name expected error, got nil")
	}
}
