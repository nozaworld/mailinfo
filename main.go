package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type config struct {
	// DiscordWebhookURLs は，通知先の名前（target）とDiscord Webhook URLの対応表．
	// "default"は，RouteFileのどのルールにもマッチしなかった場合に使う
	// 既定の通知先名（DefaultTargetで変更可能）．
	DiscordWebhookURLs map[string]string
	// SlackWebhookURLs は，target名とSlack Incoming Webhook URLの対応表．
	SlackWebhookURLs map[string]string
	// WebhookURLs は，target名と汎用Webhook URLの対応表．MailEventをJSONへ
	// エンコードしたものをそのままPOSTするため，Discord/Slack以外の任意の
	// システムと連携できる．
	WebhookURLs map[string]string
	// LogTargets は，target名とログファイルの出力先パスの対応表．
	LogTargets map[string]string
	// DesktopTargets は，デスクトップ通知（notify-send）を使うtarget名の一覧．
	// URLやパスなど付随する値を必要としないため，他のtargetと異なり文字列の
	// スライスで保持する．
	DesktopTargets []string
	// EmailTargets は，target名と通知先メールアドレスの対応表．送信に使う
	// SMTPサーバーの設定はSMTPHost以下の項目で共通のものを使う．
	EmailTargets map[string]string
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPass     string
	SMTPFrom     string

	RouteFile string
	// DefaultTarget は，RouteFileのどのルールにもマッチしなかった場合に使う
	// target名．
	DefaultTarget string

	MaildirPath  string
	StaffAddress string
	StateFile    string
	ExcludeFile  string
	PollInterval time.Duration
	Location     *time.Location
	StartHour    int
	EndHour      int
	RequireStaff bool
}

// skipTarget は，RouteFile内のルールで使う予約されたtarget名．
// このtargetにマッチしたメールは，どのDiscord Webhookへも通知せずスキップする．
const skipTarget = "skip"

type state struct {
	Seen map[string]bool `json:"seen"`
}

type messageHeader struct {
	Key       string
	Path      string
	Subject   string
	From      string
	Recipient string
	Body      string
	// Header は，メールの全ヘッダーを保持する．メーリングリストソフト（fml等）を
	// 経由するとFromが書き換わり，元の送信元アドレスがX-Original-Fromなど別の
	// ヘッダーに残るケースがあるため，ルーティング条件で任意のヘッダーを
	// 参照できるようにしている．
	Header textproto.MIMEHeader
}

var emailTokenRE = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
var lowercaseRE = regexp.MustCompile(`[a-z]`)

// routeCondition は，ルーティングルールの1つの条件を表す．
// fieldは"from"，"subject"，"body"，"text"（件名と本文を結合したもの）のいずれか．
// Negateがtrueの場合，正規表現に一致しないことが条件となる．
type routeCondition struct {
	Field  string
	Regex  *regexp.Regexp
	Negate bool
}

// route は，複数のroute condition（すべて満たした場合にマッチ，AND条件）と，
// マッチした場合の通知先target名（buildNotifiersに登録したキー，または
// skipTarget）を表す．通知先の種類（Discord，Slack等）によらず共通で使う．
type route struct {
	Conditions []routeCondition
	Target     string
}

// MailEvent は，通知対象となった1件のメールに関する情報をまとめたもの．
// 「Maildir監視 → メールイベント → 通知先プラグイン」という構造の，
// 監視処理と通知先プラグインの間を受け渡すデータであり，Maildir走査や
// メールヘッダー解析の詳細（生ヘッダーや本文全体など）はここには含めず，
// 通知先プラグインが実際に必要とする項目だけを渡す．
type MailEvent struct {
	// Key は，メールを一意に識別するキー（Maildirファイル名由来）．
	Key string `json:"key"`
	// Subject は，sanitizeSubjectを通した後の件名．
	Subject string `json:"subject"`
	// From は，差出人アドレス．
	From string `json:"from"`
	// Target は，ルーティングでマッチした通知先target名．
	Target string `json:"target"`
	// Time は，通知処理を行った時刻．
	Time time.Time `json:"time"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	filters, err := loadExcludeFilters(cfg.ExcludeFile)
	if err != nil {
		log.Fatal(err)
	}

	routes, err := loadRoutes(cfg.RouteFile)
	if err != nil {
		log.Fatal(err)
	}

	st, stateExists, err := loadState(cfg.StateFile)
	if err != nil {
		log.Fatal(err)
	}
	if st.Seen == nil {
		st.Seen = map[string]bool{}
	}

	if !stateExists {
		messages, err := scanMaildir(cfg.MaildirPath)
		if err != nil {
			log.Fatalf("initialize state: %v", err)
		}
		for _, msg := range messages {
			st.Seen[msg.Key] = true
		}
		if err := saveState(cfg.StateFile, st); err != nil {
			log.Fatal(err)
		}
		log.Printf("initialized state with %d existing messages", len(st.Seen))
	}

	notifiers, err := buildNotifiers(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		if err := pollOnce(cfg, filters, routes, notifiers, &st); err != nil {
			log.Printf("poll failed: %v", err)
		}
		<-ticker.C
	}
}

// Notifier は，マッチしたメールイベントを外部へ知らせる手段を表す．
// 「Maildir監視 → メールイベント → 通知先プラグイン」という構造にしておくことで，
// Discord以外の通知先（Slack，汎用Webhook，メール，ログ，デスクトップ通知など）を
// 追加する場合も，この interface を満たす実装をtarget名に登録するだけでよく，
// pollOnceやルーティングのロジックには手を入れる必要がない．
type Notifier interface {
	// Notify は，1件のメールイベントを，この通知先へ送る．
	Notify(event MailEvent) error
}

// postJSON は，payloadをJSONへエンコードしてurlへPOSTする共通処理．
// Discord・Slack・汎用Webhookの3つのNotifier実装がこれを利用する．
func postJSON(url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("status %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

// discordNotifier は，Discord WebhookへPOSTするNotifierの実装．
type discordNotifier struct {
	webhookURL string
}

func (n *discordNotifier) Notify(event MailEvent) error {
	if err := postJSON(n.webhookURL, map[string]string{"content": event.Subject}); err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	return nil
}

// slackNotifier は，Slack Incoming WebhookへPOSTするNotifierの実装．
type slackNotifier struct {
	webhookURL string
}

func (n *slackNotifier) Notify(event MailEvent) error {
	if err := postJSON(n.webhookURL, map[string]string{"text": event.Subject}); err != nil {
		return fmt.Errorf("slack: %w", err)
	}
	return nil
}

// webhookNotifier は，任意のURLへMailEventをJSONのままPOSTするNotifierの実装．
// Discord・Slack向けの固定フォーマットに合わせられない通知先（自作の受信側や
// 他のチャットツールなど）向けに，メールイベントの内容をそのまま渡す．
type webhookNotifier struct {
	url string
}

func (n *webhookNotifier) Notify(event MailEvent) error {
	if err := postJSON(n.url, event); err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	return nil
}

// logLine は，logNotifierがログファイルへ追記する1行分の文字列を作る．
// タブ区切りで時刻・target・差出人・件名を並べる，単純なテキスト形式．
func logLine(event MailEvent) string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\n", event.Time.Format(time.RFC3339), event.Target, event.From, event.Subject)
}

// logNotifier は，メールイベントをログファイルへ追記するNotifierの実装．
// 常駐プログラム自体の標準ログ（log.Printf）とは別に，通知内容だけを機械
// 可読な形で残しておきたい場合（他のログ収集基盤で監視する場合など）に使う．
type logNotifier struct {
	path string
}

func (n *logNotifier) Notify(event MailEvent) error {
	if err := os.MkdirAll(parentDir(n.path), 0700); err != nil {
		return fmt.Errorf("log: create dir: %w", err)
	}
	file, err := os.OpenFile(n.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("log: open file: %w", err)
	}
	defer file.Close()
	if _, err := file.WriteString(logLine(event)); err != nil {
		return fmt.Errorf("log: write file: %w", err)
	}
	return nil
}

// notifySendArgs は，desktopNotifierがnotify-sendコマンドへ渡す引数を組み立てる．
// titleが空の場合は既定のタイトルを使う．
func notifySendArgs(title string, event MailEvent) []string {
	if title == "" {
		title = "mailinfo"
	}
	return []string{title, event.Subject}
}

// desktopNotifier は，freedesktop仕様のnotify-sendコマンド経由でデスクトップ
// 通知を表示するNotifierの実装．iconvと同様に外部コマンドをexec.Commandで
// 呼び出すだけなので，golang.org/x以下の追加ライブラリには依存しない．
// GUIを持たないサーバー上で常駐させる運用では，notify-sendが存在しない，
// またはDBUSに接続できないため失敗する点に注意する．
type desktopNotifier struct {
	title string
}

func (n *desktopNotifier) Notify(event MailEvent) error {
	args := notifySendArgs(n.title, event)
	cmd := exec.Command("notify-send", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("desktop: notify-send: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// buildEmailMessage は，emailNotifierがnet/smtpへ渡す，ヘッダー付きのメール
// 本文（RFC 5322形式）を組み立てる．件名はASCII以外の文字を含み得るため，
// mime.QEncoding（RFC 2047）でエンコードする．
func buildEmailMessage(from, to string, event MailEvent) []byte {
	subject := mime.QEncoding.Encode("UTF-8", "[mailinfo] "+event.Subject)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", from)
	fmt.Fprintf(&buf, "To: %s\r\n", to)
	fmt.Fprintf(&buf, "Subject: %s\r\n", subject)
	fmt.Fprintf(&buf, "Date: %s\r\n", event.Time.Format(time.RFC1123Z))
	buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	buf.WriteString("\r\n")
	fmt.Fprintf(&buf, "差出人: %s\r\n通知先: %s\r\n件名: %s\r\n", event.From, event.Target, event.Subject)
	return buf.Bytes()
}

// emailNotifier は，SMTP経由でメールを送るNotifierの実装．SMTPサーバーの
// 接続情報はtarget間で共通（config.SMTPHost以下）とし，宛先アドレスだけを
// target毎に変える．
type emailNotifier struct {
	host, port, user, pass, from, to string
}

func (n *emailNotifier) Notify(event MailEvent) error {
	addr := net.JoinHostPort(n.host, n.port)
	var auth smtp.Auth
	if n.user != "" {
		auth = smtp.PlainAuth("", n.user, n.pass, n.host)
	}
	msg := buildEmailMessage(n.from, n.to, event)
	if err := smtp.SendMail(addr, auth, n.from, []string{n.to}, msg); err != nil {
		return fmt.Errorf("email: %w", err)
	}
	return nil
}

// buildNotifiers は，config内の各種通知先設定（Discord，Slack，汎用Webhook，
// ログ，デスクトップ通知，メール）を，target名とNotifierの対応表へまとめる．
// 「Maildir監視 → メールイベント → 通知先プラグイン」という構造にしているため，
// 新しい通知先を追加する場合も，ここに同様のfor文を1つ足して戻り値のmapへ
// 登録するだけでよく，pollOnceやルーティングのロジックには手を入れる必要が
// ない．同じtarget名が複数の種類の通知先に定義された場合はエラーとする．
func buildNotifiers(cfg config) (map[string]Notifier, error) {
	notifiers := make(map[string]Notifier)
	kinds := make(map[string]string)

	add := func(kind, target string, notifier Notifier) error {
		if prevKind, exists := kinds[target]; exists {
			return fmt.Errorf("target %q is defined by both %s and %s notifiers; each target must map to exactly one notifier", target, prevKind, kind)
		}
		kinds[target] = kind
		notifiers[target] = notifier
		return nil
	}

	for target, url := range cfg.DiscordWebhookURLs {
		if err := add("discord", target, &discordNotifier{webhookURL: url}); err != nil {
			return nil, err
		}
	}
	for target, url := range cfg.SlackWebhookURLs {
		if err := add("slack", target, &slackNotifier{webhookURL: url}); err != nil {
			return nil, err
		}
	}
	for target, url := range cfg.WebhookURLs {
		if err := add("webhook", target, &webhookNotifier{url: url}); err != nil {
			return nil, err
		}
	}
	for target, path := range cfg.LogTargets {
		if err := add("log", target, &logNotifier{path: path}); err != nil {
			return nil, err
		}
	}
	for _, target := range cfg.DesktopTargets {
		if err := add("desktop", target, &desktopNotifier{title: "mailinfo"}); err != nil {
			return nil, err
		}
	}
	for target, to := range cfg.EmailTargets {
		notifier := &emailNotifier{
			host: cfg.SMTPHost,
			port: cfg.SMTPPort,
			user: cfg.SMTPUser,
			pass: cfg.SMTPPass,
			from: cfg.SMTPFrom,
			to:   to,
		}
		if err := add("email", target, notifier); err != nil {
			return nil, err
		}
	}

	return notifiers, nil
}

func loadConfig() (config, error) {
	locName := getenvDefault("TIMEZONE", "Asia/Tokyo")
	loc, err := time.LoadLocation(locName)
	if err != nil {
		return config{}, fmt.Errorf("load timezone %q: %w", locName, err)
	}

	pollInterval, err := time.ParseDuration(getenvDefault("POLL_INTERVAL", "1m"))
	if err != nil {
		return config{}, fmt.Errorf("parse POLL_INTERVAL: %w", err)
	}

	startHour, err := getenvInt("WORK_START_HOUR", 6)
	if err != nil {
		return config{}, err
	}
	endHour, err := getenvInt("WORK_END_HOUR", 22)
	if err != nil {
		return config{}, err
	}
	if startHour < 0 || startHour > 23 || endHour < 1 || endHour > 24 || startHour >= endHour {
		return config{}, errors.New("WORK_START_HOUR and WORK_END_HOUR must satisfy 0 <= start < end <= 24")
	}

	webhookURLs, err := parseNamedValues(os.Getenv("DISCORD_WEBHOOK_URLS"))
	if err != nil {
		return config{}, fmt.Errorf("parse DISCORD_WEBHOOK_URLS: %w", err)
	}
	// DISCORD_WEBHOOK_URLは後方互換のため残しており，指定された場合は
	// "default"という名前でwebhookURLsへ登録する（DISCORD_WEBHOOK_URLSで
	// 明示的に"default"が定義されていれば，そちらを優先する）．
	if legacyURL := os.Getenv("DISCORD_WEBHOOK_URL"); legacyURL != "" {
		if webhookURLs == nil {
			webhookURLs = map[string]string{}
		}
		if _, ok := webhookURLs["default"]; !ok {
			webhookURLs["default"] = legacyURL
		}
	}

	slackWebhookURLs, err := parseNamedValues(os.Getenv("SLACK_WEBHOOK_URLS"))
	if err != nil {
		return config{}, fmt.Errorf("parse SLACK_WEBHOOK_URLS: %w", err)
	}
	genericWebhookURLs, err := parseNamedValues(os.Getenv("WEBHOOK_URLS"))
	if err != nil {
		return config{}, fmt.Errorf("parse WEBHOOK_URLS: %w", err)
	}
	logTargets, err := parseNamedValues(os.Getenv("LOG_TARGETS"))
	if err != nil {
		return config{}, fmt.Errorf("parse LOG_TARGETS: %w", err)
	}
	desktopTargets, err := parseNames(os.Getenv("DESKTOP_TARGETS"))
	if err != nil {
		return config{}, fmt.Errorf("parse DESKTOP_TARGETS: %w", err)
	}
	emailTargets, err := parseNamedValues(os.Getenv("EMAIL_TARGETS"))
	if err != nil {
		return config{}, fmt.Errorf("parse EMAIL_TARGETS: %w", err)
	}

	cfg := config{
		DiscordWebhookURLs: webhookURLs,
		SlackWebhookURLs:   slackWebhookURLs,
		WebhookURLs:        genericWebhookURLs,
		LogTargets:         logTargets,
		DesktopTargets:     desktopTargets,
		EmailTargets:       emailTargets,
		SMTPHost:           os.Getenv("SMTP_HOST"),
		SMTPPort:           getenvDefault("SMTP_PORT", "587"),
		SMTPUser:           os.Getenv("SMTP_USER"),
		SMTPPass:           os.Getenv("SMTP_PASS"),
		SMTPFrom:           os.Getenv("SMTP_FROM"),

		RouteFile:     getenvDefault("DISCORD_ROUTE_FILE", "private/discord_routes.txt"),
		DefaultTarget: getenvDefault("DISCORD_DEFAULT_TARGET", "default"),

		MaildirPath:  os.Getenv("MAILDIR_PATH"),
		StaffAddress: strings.ToLower(os.Getenv("STAFF_ADDRESS")),
		StateFile:    getenvDefault("STATE_FILE", "private/state.json"),
		ExcludeFile:  getenvDefault("EXCLUDE_FILE", "private/exclude_senders.txt"),
		PollInterval: pollInterval,
		Location:     loc,
		StartHour:    startHour,
		EndHour:      endHour,
		RequireStaff: getenvBool("REQUIRE_STAFF_ADDRESS", true),
	}

	var missing []string
	hasNotifierConfig := len(cfg.DiscordWebhookURLs) > 0 ||
		len(cfg.SlackWebhookURLs) > 0 ||
		len(cfg.WebhookURLs) > 0 ||
		len(cfg.LogTargets) > 0 ||
		len(cfg.DesktopTargets) > 0 ||
		len(cfg.EmailTargets) > 0
	if !hasNotifierConfig {
		missing = append(missing, "at least one of DISCORD_WEBHOOK_URL(S), SLACK_WEBHOOK_URLS, WEBHOOK_URLS, LOG_TARGETS, DESKTOP_TARGETS, EMAIL_TARGETS")
	}
	if cfg.MaildirPath == "" {
		missing = append(missing, "MAILDIR_PATH")
	}
	if cfg.RequireStaff && cfg.StaffAddress == "" {
		missing = append(missing, "STAFF_ADDRESS")
	}
	if len(cfg.EmailTargets) > 0 {
		if cfg.SMTPHost == "" {
			missing = append(missing, "SMTP_HOST")
		}
		if cfg.SMTPFrom == "" {
			missing = append(missing, "SMTP_FROM")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return config{}, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

// parseNamedValues は，"name1=value1,name2=value2"形式の文字列を，target名と
// 値（Webhook URL，ログファイルパス，メールアドレスなど）の対応表へ変換する．
// Discord・Slack・汎用Webhook・ログ・メールなど，target名に1つの値が対応する
// 通知先設定を読み込むための共通処理．空文字列を渡した場合はnilを返す．
func parseNamedValues(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	values := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, ok := strings.Cut(pair, "=")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("invalid entry %q: want name=value", pair)
		}
		if name == skipTarget {
			return nil, fmt.Errorf("invalid entry %q: %q is a reserved target name", pair, skipTarget)
		}
		values[name] = value
	}
	return values, nil
}

// parseNames は，カンマ区切りのtarget名リスト（例: "desk1,desk2"）を読み取る．
// デスクトップ通知のように，URLやパスなど付随する値を持たない通知先の設定に使う．
// 空文字列を渡した場合はnilを返す．
func parseNames(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var names []string
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if name == skipTarget {
			return nil, fmt.Errorf("invalid target name %q: %q is a reserved target name", name, skipTarget)
		}
		names = append(names, name)
	}
	return names, nil
}

func pollOnce(cfg config, filters []*regexp.Regexp, routes []route, notifiers map[string]Notifier, st *state) error {
	now := time.Now().In(cfg.Location)
	if !withinWindow(now, cfg.StartHour, cfg.EndHour) {
		return nil
	}

	messages, err := scanMaildir(cfg.MaildirPath)
	if err != nil {
		return err
	}

	changed := false
	for _, msg := range messages {
		if st.Seen[msg.Key] {
			continue
		}
		st.Seen[msg.Key] = true
		changed = true

		if cfg.RequireStaff && !recipientMatches(msg.Recipient, cfg.StaffAddress) {
			log.Printf("skip %s: recipient %q does not match %q", msg.Key, msg.Recipient, cfg.StaffAddress)
			continue
		}
		if excluded(msg.From, filters) {
			log.Printf("skip %s from %q", msg.Key, msg.From)
			continue
		}

		target := resolveTarget(routes, msg)
		if target == "" {
			target = cfg.DefaultTarget
		}
		if target == skipTarget {
			log.Printf("skip %s: routed to %q", msg.Key, skipTarget)
			continue
		}

		notifier, ok := notifiers[target]
		if !ok {
			log.Printf("skip %s: no notifier registered for target %q", msg.Key, target)
			continue
		}

		event := MailEvent{
			Key:     msg.Key,
			Subject: sanitizeSubject(msg.Subject),
			From:    msg.From,
			Target:  target,
			Time:    now,
		}
		if err := notifier.Notify(event); err != nil {
			return fmt.Errorf("notify %s: %w", msg.Key, err)
		}
		log.Printf("notified %s (target=%s)", msg.Key, target)
	}

	if changed {
		return saveState(cfg.StateFile, *st)
	}
	return nil
}

// routeFieldValue は，msgからroute conditionのfieldに対応する文字列を取り出す．
// "text"は件名と本文を改行で結合したものを表す．
func routeFieldValue(field string, msg messageHeader) (string, bool) {
	if headerName, ok := strings.CutPrefix(field, "header:"); ok {
		if headerName == "" {
			return "", false
		}
		return msg.Header.Get(headerName), true
	}

	switch field {
	case "from":
		return msg.From, true
	case "subject":
		return msg.Subject, true
	case "body":
		return msg.Body, true
	case "text":
		return msg.Subject + "\n" + msg.Body, true
	default:
		return "", false
	}
}

// resolveTarget は，メールの内容をルールに照らし合わせ，最初にマッチしたルールの
// target名を返す．各ルールは，すべての条件（Conditions）を満たした場合にのみ
// マッチする（AND）．Negateが指定された条件は，正規表現に一致しない場合にマッチする．
// どのルールにもマッチしない場合は空文字列を返す．
func resolveTarget(routes []route, msg messageHeader) string {
	for _, r := range routes {
		matched := true
		for _, cond := range r.Conditions {
			value, ok := routeFieldValue(cond.Field, msg)
			if !ok {
				matched = false
				break
			}
			isMatch := cond.Regex.MatchString(value)
			if cond.Negate {
				isMatch = !isMatch
			}
			if !isMatch {
				matched = false
				break
			}
		}
		if matched {
			return r.Target
		}
	}
	return ""
}

func withinWindow(t time.Time, startHour, endHour int) bool {
	weekday := t.Weekday()
	if weekday == time.Saturday || weekday == time.Sunday {
		return false
	}
	hour := t.Hour()
	return hour >= startHour && hour < endHour
}

func scanMaildir(maildirPath string) ([]messageHeader, error) {
	var messages []messageHeader
	for _, subdir := range []string{"new", "cur"} {
		dir := filepath.Join(maildirPath, subdir)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dir, err)
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			header, err := parseMessageHeader(path)
			if err != nil {
				log.Printf("skip unreadable message %s: %v", path, err)
				continue
			}
			header.Key = maildirKey(entry.Name())
			header.Path = path
			messages = append(messages, header)
		}
	}

	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Key < messages[j].Key
	})
	return messages, nil
}

// subjectDecoder は，件名（Subject）のRFC 2047エンコード（encoded-word）を
// デコードするために使う．ISO-2022-JPやShift_JISなど，Goの標準ライブラリが
// 扱わない文字エンコーディングは，charsetReaderがiconvコマンドへ委譲して
// UTF-8へ変換する（golang.org/x/textに依存しない）．
var subjectDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

// charsetReader は，mime.WordDecoderから，encoded-word内の文字エンコーディング名
// （例: "iso-2022-jp"，"shift_jis"）とその生バイト列を受け取り，UTF-8へ変換した
// io.Readerを返す．UTF-8・US-ASCIIはそのまま返し，それ以外はiconvコマンドへ
// パイプしてUTF-8へ変換する．iconvはLinuxに標準で入っているため，追加の
// 依存ライブラリを必要としない．
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "us-ascii", "ascii", "utf-8", "utf8":
		return input, nil
	}

	data, err := io.ReadAll(input)
	if err != nil {
		return nil, fmt.Errorf("read charset %s input: %w", charset, err)
	}

	cmd := exec.Command("iconv", "-f", charset, "-t", "UTF-8//TRANSLIT")
	cmd.Stdin = bytes.NewReader(data)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("iconv -f %s: %w: %s", charset, err, strings.TrimSpace(stderr.String()))
	}
	return &out, nil
}

func parseMessageHeader(path string) (messageHeader, error) {
	file, err := os.Open(path)
	if err != nil {
		return messageHeader{}, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	header, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil && !errors.Is(err, io.EOF) {
		return messageHeader{}, err
	}

	from := header.Get("From")
	if address, err := mail.ParseAddress(from); err == nil {
		from = address.Address
	}

	subject, err := subjectDecoder.DecodeHeader(header.Get("Subject"))
	if err != nil {
		subject = header.Get("Subject")
	}

	// 本文は生の状態（文字エンコードやマルチパートの分解を行わない）で保持する．
	// ルーティング用の単純な文字列照合にのみ使用する．
	bodyBytes, err := io.ReadAll(reader)
	if err != nil {
		return messageHeader{}, err
	}

	return messageHeader{
		Subject:   subject,
		From:      from,
		Recipient: recipientHeaders(header),
		Body:      string(bodyBytes),
		Header:    header,
	}, nil
}

func recipientHeaders(header textproto.MIMEHeader) string {
	keys := []string{
		"To",
		"Cc",
		"Delivered-To",
		"X-Original-To",
		"Envelope-To",
		"List-Post",
		"List-Id",
		"X-Loop",
		"Sender",
		"Errors-To",
	}

	var fields []string
	for _, key := range keys {
		fields = append(fields, header.Values(key)...)
	}
	return strings.ToLower(strings.Join(fields, "\n"))
}

func recipientMatches(headers string, staffAddress string) bool {
	headers = strings.ToLower(headers)
	staffAddress = strings.ToLower(staffAddress)
	if strings.Contains(headers, staffAddress) {
		return true
	}

	local, domain, ok := strings.Cut(staffAddress, "@")
	if !ok || local == "" || domain == "" {
		return false
	}

	return strings.Contains(headers, local+"."+domain) ||
		strings.Contains(headers, local+"-request@"+domain) ||
		strings.Contains(headers, local+"-owner@"+domain)
}

func maildirKey(filename string) string {
	if idx := strings.Index(filename, ":"); idx >= 0 {
		return filename[:idx]
	}
	return filename
}

func sanitizeSubject(subject string) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "None"
	}
	return emailTokenRE.ReplaceAllStringFunc(subject, func(token string) string {
		parts := strings.SplitN(token, "@", 2)
		return lowercaseRE.ReplaceAllString(parts[0], "") + "@" + parts[1]
	})
}

func excluded(from string, filters []*regexp.Regexp) bool {
	for _, filter := range filters {
		if filter.MatchString(from) {
			return true
		}
	}
	return false
}

func loadExcludeFilters(path string) ([]*regexp.Regexp, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open exclude file: %w", err)
	}
	defer file.Close()

	var filters []*regexp.Regexp
	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		filter, err := regexp.Compile(line)
		if err != nil {
			return nil, fmt.Errorf("compile exclude regex %s:%d: %w", path, lineNo, err)
		}
		filters = append(filters, filter)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read exclude file: %w", err)
	}
	return filters, nil
}

// loadRoutes は，RouteFileからルーティングルールを読み込む．通知先の種類
// （Discord，Slack，Webhookなど）によらず，target名を振り分けるための共通の
// ルールファイルを扱う．各行はタブ区切りで，最後の要素をtarget，それ以外の
// 要素を条件として扱う．条件は"field:regex"（一致することが条件）または
// "!field:regex"（一致しないことが条件）の形式で指定し，1行内のすべての条件を
// 満たした場合にのみマッチする（AND）．fieldは"from"，"subject"，"body"，
// "text"（件名と本文の結合）のいずれか．targetには，buildNotifiersに登録した
// target名，またはskipTarget（"skip"，通知をスキップする予約語）を指定する．
// 空行と"#"で始まる行は無視する．ファイルが存在しない場合はルールなしとして扱う．
func loadRoutes(path string) ([]route, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open route file: %w", err)
	}
	defer file.Close()

	var routes []route
	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			return nil, fmt.Errorf("parse route %s:%d: expected \"field:regex<TAB>...<TAB>target\"", path, lineNo)
		}

		target := strings.TrimSpace(fields[len(fields)-1])
		if target == "" {
			return nil, fmt.Errorf("parse route %s:%d: missing target", path, lineNo)
		}

		var conditions []routeCondition
		for _, raw := range fields[:len(fields)-1] {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}

			negate := strings.HasPrefix(raw, "!")
			raw = strings.TrimPrefix(raw, "!")

			// "header:<ヘッダー名>:regex" は，ヘッダー名自体にコロンを含まない
			// 前提で，2番目のコロンをfieldとregexの区切りとして扱う．
			// それ以外（from/subject/body/text）は，最初のコロンで区切る．
			var field, pattern string
			if rest, isHeader := strings.CutPrefix(raw, "header:"); isHeader {
				headerName, p, ok := strings.Cut(rest, ":")
				if !ok || strings.TrimSpace(headerName) == "" {
					return nil, fmt.Errorf("parse route %s:%d: expected \"header:<name>:regex\"", path, lineNo)
				}
				field = "header:" + strings.TrimSpace(headerName)
				pattern = p
			} else {
				f, p, ok := strings.Cut(raw, ":")
				if !ok {
					return nil, fmt.Errorf("parse route %s:%d: expected \"field:regex\"", path, lineNo)
				}
				f = strings.TrimSpace(f)
				switch f {
				case "from", "subject", "body", "text":
				default:
					return nil, fmt.Errorf("parse route %s:%d: unknown field %q (want from, subject, body, text, or header:<name>)", path, lineNo, f)
				}
				field = f
				pattern = p
			}

			regex, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("compile route regex %s:%d: %w", path, lineNo, err)
			}

			conditions = append(conditions, routeCondition{Field: field, Regex: regex, Negate: negate})
		}
		if len(conditions) == 0 {
			return nil, fmt.Errorf("parse route %s:%d: at least one condition is required", path, lineNo)
		}

		routes = append(routes, route{Conditions: conditions, Target: target})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read route file: %w", err)
	}
	return routes, nil
}

func loadState(path string) (state, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return state{}, false, nil
	}
	if err != nil {
		return state{}, false, fmt.Errorf("open state file: %w", err)
	}
	defer file.Close()

	var st state
	if err := json.NewDecoder(file).Decode(&st); err != nil {
		return state{}, false, fmt.Errorf("decode state file: %w", err)
	}
	return st, true, nil
}

func saveState(path string, st state) error {
	if err := os.MkdirAll(parentDir(path), 0700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(st); err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	return os.WriteFile(path, buf.Bytes(), 0600)
}

func getenvDefault(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func getenvInt(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}

func getenvBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func parentDir(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx == -1 {
		return "."
	}
	return path[:idx]
}
