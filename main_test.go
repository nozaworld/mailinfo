package main

import (
	"io"
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

func TestParseNamedValues(t *testing.T) {
	got, err := parseNamedValues("")
	if err != nil || got != nil {
		t.Fatalf("parseNamedValues(\"\") = %v, %v, want nil, nil", got, err)
	}

	got, err = parseNamedValues("urgent=https://discord.com/api/webhooks/1, newsletter = https://discord.com/api/webhooks/2 ")
	if err != nil {
		t.Fatalf("parseNamedValues() error = %v", err)
	}
	want := map[string]string{
		"urgent":     "https://discord.com/api/webhooks/1",
		"newsletter": "https://discord.com/api/webhooks/2",
	}
	if len(got) != len(want) {
		t.Fatalf("parseNamedValues() = %v, want %v", got, want)
	}
	for name, url := range want {
		if got[name] != url {
			t.Fatalf("parseNamedValues()[%q] = %q, want %q", name, got[name], url)
		}
	}

	if _, err := parseNamedValues("urgent"); err == nil {
		t.Fatal("parseNamedValues(\"urgent\") expected error, got nil")
	}
	if _, err := parseNamedValues("=https://discord.com/api/webhooks/1"); err == nil {
		t.Fatal("parseNamedValues() with empty name expected error, got nil")
	}
	if _, err := parseNamedValues("skip=https://discord.com/api/webhooks/1"); err == nil {
		t.Fatal("parseNamedValues() with reserved name \"skip\" expected error, got nil")
	}
}

func TestParseNames(t *testing.T) {
	got, err := parseNames("")
	if err != nil || got != nil {
		t.Fatalf("parseNames(\"\") = %v, %v, want nil, nil", got, err)
	}

	got, err = parseNames(" desk1, desk2 ,desk1")
	if err != nil {
		t.Fatalf("parseNames() error = %v", err)
	}
	want := []string{"desk1", "desk2", "desk1"}
	if len(got) != len(want) {
		t.Fatalf("parseNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if _, err := parseNames("skip"); err == nil {
		t.Fatal("parseNames() with reserved name \"skip\" expected error, got nil")
	}
}

func TestResolveTarget(t *testing.T) {
	requestKeyword := regexp.MustCompile(`(お願い|トラブル|していただ)`)

	routes := []route{
		{
			// スキップ条件: ドメインホワイトリスト外，またはブラックリスト
			Conditions: []routeCondition{
				{Field: "from", Regex: regexp.MustCompile(`@blocked\.example$`)},
			},
			Target: skipTarget,
		},
		{
			Conditions: []routeCondition{
				{Field: "from", Regex: regexp.MustCompile(`@(?:[^@]*\.)?example\.com$`), Negate: true},
				{Field: "from", Regex: regexp.MustCompile(`@list\.`), Negate: true},
			},
			Target: skipTarget,
		},
		{
			Conditions: []routeCondition{
				{Field: "from", Regex: regexp.MustCompile(`^managers@example\.com$`)},
			},
			Target: "security-info",
		},
		{
			Conditions: []routeCondition{
				{Field: "from", Regex: regexp.MustCompile(`^nagios@.*list\.example\.com$`)},
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
			msg:  messageHeader{From: "nagios@monitor.list.example.com", Subject: "HOST DOWN", Body: ""},
			want: "mail-alert",
		},
		{
			name: "manager security info",
			msg:  messageHeader{From: "managers@example.com", Subject: "セキュリティアップデート", Body: ""},
			want: "security-info",
		},
		{
			name: "staff mail alert",
			msg:  messageHeader{From: "staff@example.com", Subject: "管理システム更新", Body: ""},
			want: "mail-alert",
		},
		{
			name: "keyword based work log",
			msg:  messageHeader{From: "tanaka@example.com", Subject: "資料作成のお願い", Body: ""},
			want: "work-log",
		},
		{
			name: "no rule matches",
			msg:  messageHeader{From: "tanaka@example.com", Subject: "共有です", Body: "資料を共有します"},
			want: "",
		},
	}

	for _, tt := range tests {
		if got := resolveTarget(routes, tt.msg); got != tt.want {
			t.Fatalf("%s: resolveTarget() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestLoadRoutes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.txt")
	lines := []string{
		"# comment line",
		"",
		"from:@blocked\\.example\tskip",
		"!from:\\.example\\.com$\t!from:@list\\.\tskip",
		"from:^managers@example\\.com$\tsecurity-info",
		"text:(お願い|トラブル|していただ)\twork-log",
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write route file: %v", err)
	}

	routes, err := loadRoutes(path)
	if err != nil {
		t.Fatalf("loadRoutes() error = %v", err)
	}
	if len(routes) != 4 {
		t.Fatalf("loadRoutes() returned %d routes, want 4", len(routes))
	}
	if len(routes[1].Conditions) != 2 || !routes[1].Conditions[0].Negate || routes[1].Target != skipTarget {
		t.Fatalf("routes[1] = %+v, want 2 negated conditions and target %q", routes[1], skipTarget)
	}

	missing, err := loadRoutes(filepath.Join(dir, "does-not-exist.txt"))
	if err != nil || missing != nil {
		t.Fatalf("loadRoutes(missing file) = %v, %v, want nil, nil", missing, err)
	}
}

func TestRouteFieldValueHeader(t *testing.T) {
	msg := messageHeader{
		From: "staff-sender@list.example.com",
		Header: textproto.MIMEHeader{
			"X-Original-From": []string{"managers@example.com"},
		},
	}
	value, ok := routeFieldValue("header:X-Original-From", msg)
	if !ok || value != "managers@example.com" {
		t.Fatalf("routeFieldValue() = %q, %v, want %q, true", value, ok, "managers@example.com")
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
	routes := []route{
		{
			Conditions: []routeCondition{
				{Field: "header:X-Original-From", Regex: regexp.MustCompile(`^managers@example\.com$`)},
			},
			Target: "B",
		},
		{
			Conditions: []routeCondition{
				{Field: "from", Regex: regexp.MustCompile(`^managers@example\.com$`)},
			},
			Target: "B",
		},
	}

	// メーリングリスト経由でFromが書き換わっているケース（X-Original-Fromで判定）
	viaML := messageHeader{
		From: "staff-sender@list.example.com",
		Header: textproto.MIMEHeader{
			"X-Original-From": []string{"managers@example.com"},
		},
	}
	if got := resolveTarget(routes, viaML); got != "B" {
		t.Fatalf("resolveTarget() via mailing list = %q, want %q", got, "B")
	}

	// 直接送信されており，X-Original-Fromが存在しないケース（fromで判定）
	direct := messageHeader{From: "managers@example.com", Header: textproto.MIMEHeader{}}
	if got := resolveTarget(routes, direct); got != "B" {
		t.Fatalf("resolveTarget() direct = %q, want %q", got, "B")
	}
}

func TestLoadRoutesHeaderField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.txt")
	content := "header:X-Original-From:^managers@example\\.com$\tB\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write route file: %v", err)
	}
	routes, err := loadRoutes(path)
	if err != nil {
		t.Fatalf("loadRoutes() error = %v", err)
	}
	if len(routes) != 1 || routes[0].Conditions[0].Field != "header:X-Original-From" {
		t.Fatalf("routes = %+v, want header:X-Original-From condition", routes)
	}

	emptyHeaderPath := filepath.Join(dir, "empty_header.txt")
	if err := os.WriteFile(emptyHeaderPath, []byte("header:\tB\n"), 0600); err != nil {
		t.Fatalf("write route file: %v", err)
	}
	if _, err := loadRoutes(emptyHeaderPath); err == nil {
		t.Fatal("loadRoutes() with empty header name expected error, got nil")
	}
}

func TestCharsetReaderPassesThroughUTF8(t *testing.T) {
	r, err := charsetReader("utf-8", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("charsetReader() error = %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestLogLine(t *testing.T) {
	event := MailEvent{
		Key:     "1700000000.M1.host",
		Subject: "テスト件名",
		From:    "someone@example.com",
		Target:  "mail-alert",
		Time:    time.Date(2026, 9, 15, 9, 30, 0, 0, time.UTC),
	}
	want := "2026-09-15T09:30:00Z\tmail-alert\tsomeone@example.com\tテスト件名\n"
	if got := logLine(event); got != want {
		t.Fatalf("logLine() = %q, want %q", got, want)
	}
}

func TestLogNotifierAppendsLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "notify.log")
	n := &logNotifier{path: path}

	event := MailEvent{Subject: "1件目", From: "a@example.com", Target: "t", Time: time.Unix(0, 0).UTC()}
	if err := n.Notify(event); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	event.Subject = "2件目"
	if err := n.Notify(event); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("log file has %d lines, want 2: %q", len(lines), string(data))
	}
}

func TestNotifySendArgs(t *testing.T) {
	event := MailEvent{Subject: "テスト件名"}

	got := notifySendArgs("", event)
	want := []string{"mailinfo", "テスト件名"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("notifySendArgs(\"\", event) = %v, want %v", got, want)
	}

	got = notifySendArgs("カスタムタイトル", event)
	if got[0] != "カスタムタイトル" || got[1] != "テスト件名" {
		t.Fatalf("notifySendArgs() = %v, want title %q and subject %q", got, "カスタムタイトル", "テスト件名")
	}
}

func TestBuildEmailMessage(t *testing.T) {
	event := MailEvent{
		Subject: "サーバー障害",
		From:    "monitor@example.com",
		Target:  "mail-alert",
		Time:    time.Date(2026, 9, 15, 9, 30, 0, 0, time.UTC),
	}

	msg := string(buildEmailMessage("mailinfo@example.com", "staff@example.com", event))

	if !strings.Contains(msg, "From: mailinfo@example.com\r\n") {
		t.Fatalf("message missing From header: %q", msg)
	}
	if !strings.Contains(msg, "To: staff@example.com\r\n") {
		t.Fatalf("message missing To header: %q", msg)
	}
	// 件名はASCII以外を含むため，RFC 2047のencoded-wordでエンコードされる．
	if !strings.Contains(msg, "Subject: =?UTF-8?") {
		t.Fatalf("message subject is not RFC 2047 encoded: %q", msg)
	}
	if !strings.Contains(msg, "差出人: monitor@example.com") || !strings.Contains(msg, "通知先: mail-alert") {
		t.Fatalf("message body missing event details: %q", msg)
	}
}

func TestBuildNotifiersMergesTypesAndDetectsConflicts(t *testing.T) {
	cfg := config{
		DiscordWebhookURLs: map[string]string{"a": "https://discord.example/a"},
		SlackWebhookURLs:   map[string]string{"b": "https://slack.example/b"},
		WebhookURLs:        map[string]string{"c": "https://webhook.example/c"},
		LogTargets:         map[string]string{"d": t.TempDir() + "/d.log"},
		DesktopTargets:     []string{"e"},
		EmailTargets:       map[string]string{"f": "staff@example.com"},
		SMTPHost:           "smtp.example.com",
		SMTPPort:           "587",
		SMTPFrom:           "mailinfo@example.com",
	}

	notifiers, err := buildNotifiers(cfg)
	if err != nil {
		t.Fatalf("buildNotifiers() error = %v", err)
	}
	for _, target := range []string{"a", "b", "c", "d", "e", "f"} {
		if _, ok := notifiers[target]; !ok {
			t.Fatalf("buildNotifiers() missing notifier for target %q", target)
		}
	}

	conflict := config{
		DiscordWebhookURLs: map[string]string{"dup": "https://discord.example/dup"},
		SlackWebhookURLs:   map[string]string{"dup": "https://slack.example/dup"},
	}
	if _, err := buildNotifiers(conflict); err == nil {
		t.Fatal("buildNotifiers() with target defined by two notifier types expected error, got nil")
	}
}

func TestParseMessageHeaderDecodesISO2022JPSubject(t *testing.T) {
	// staffメーリングリストで実際に文字化けが発生した件名（ISO-2022-JPの
	// encoded-wordが2つ連続する形式）を，iconv経由のcharsetReaderで
	// 正しくデコードできることを確認する．golang.org/x/textには依存しない．
	dir := t.TempDir()
	path := filepath.Join(dir, "iso2022jp.eml")
	content := "Subject: [Staff:43423] =?ISO-2022-JP?B?GyRCOTk/N0RMQ04bKEI=?= - =?ISO-2022-JP?B?GyRCJTclOSVGJWA0SU19PDwbKEI=?=\r\n\r\nbody\r\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write eml: %v", err)
	}

	msg, err := parseMessageHeader(path)
	if err != nil {
		t.Fatalf("parseMessageHeader() error = %v", err)
	}

	want := "[Staff:43423] 更新通知 - システム管理室"
	if msg.Subject != want {
		t.Fatalf("Subject = %q, want %q", msg.Subject, want)
	}
}
