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
	"net/http"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type config struct {
	// DiscordWebhookURLs は，通知先の名前（target）とDiscord Webhook URLの対応表．
	// "default"は，DiscordRouteFileのどのルールにもマッチしなかった場合に使う
	// 既定の通知先名（DiscordDefaultTargetで変更可能）．
	DiscordWebhookURLs map[string]string
	DiscordRouteFile   string
	// DiscordDefaultTarget は，ルールにマッチしなかった場合に使う
	// DiscordWebhookURLsのキー名．
	DiscordDefaultTarget string

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

// skipTarget は，DiscordRouteFile内のルールで使う予約されたtarget名．
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

// discordRoute は，複数のroute condition（すべて満たした場合にマッチ，AND条件）と，
// マッチした場合の通知先target名（DiscordWebhookURLsのキー，またはskipTarget）を表す．
type discordRoute struct {
	Conditions []routeCondition
	Target     string
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

	routes, err := loadDiscordRoutes(cfg.DiscordRouteFile)
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

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		if err := pollOnce(cfg, filters, routes, &st); err != nil {
			log.Printf("poll failed: %v", err)
		}
		<-ticker.C
	}
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

	webhookURLs, err := parseWebhookURLs(os.Getenv("DISCORD_WEBHOOK_URLS"))
	if err != nil {
		return config{}, err
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

	cfg := config{
		DiscordWebhookURLs:   webhookURLs,
		DiscordRouteFile:     getenvDefault("DISCORD_ROUTE_FILE", "private/discord_routes.txt"),
		DiscordDefaultTarget: getenvDefault("DISCORD_DEFAULT_TARGET", "default"),
		MaildirPath:          os.Getenv("MAILDIR_PATH"),
		StaffAddress:         strings.ToLower(os.Getenv("STAFF_ADDRESS")),
		StateFile:            getenvDefault("STATE_FILE", "private/state.json"),
		ExcludeFile:          getenvDefault("EXCLUDE_FILE", "private/exclude_senders.txt"),
		PollInterval:         pollInterval,
		Location:             loc,
		StartHour:            startHour,
		EndHour:              endHour,
		RequireStaff:         getenvBool("REQUIRE_STAFF_ADDRESS", true),
	}

	var missing []string
	if len(cfg.DiscordWebhookURLs) == 0 {
		missing = append(missing, "DISCORD_WEBHOOK_URL or DISCORD_WEBHOOK_URLS")
	}
	if cfg.MaildirPath == "" {
		missing = append(missing, "MAILDIR_PATH")
	}
	if cfg.RequireStaff && cfg.StaffAddress == "" {
		missing = append(missing, "STAFF_ADDRESS")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return config{}, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

// parseWebhookURLs は，DISCORD_WEBHOOK_URLSの値（"name1=url1,name2=url2"形式）を
// target名とURLのマップへ変換する．空文字列の場合はnilを返す．
func parseWebhookURLs(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	urls := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, url, ok := strings.Cut(pair, "=")
		name = strings.TrimSpace(name)
		url = strings.TrimSpace(url)
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("invalid DISCORD_WEBHOOK_URLS entry %q: want name=url", pair)
		}
		if name == skipTarget {
			return nil, fmt.Errorf("invalid DISCORD_WEBHOOK_URLS entry %q: %q is a reserved target name", pair, skipTarget)
		}
		urls[name] = url
	}
	return urls, nil
}

func pollOnce(cfg config, filters []*regexp.Regexp, routes []discordRoute, st *state) error {
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
			target = cfg.DiscordDefaultTarget
		}
		if target == skipTarget {
			log.Printf("skip %s: routed to %q", msg.Key, skipTarget)
			continue
		}

		webhookURL, ok := cfg.DiscordWebhookURLs[target]
		if !ok || webhookURL == "" {
			log.Printf("skip %s: no webhook URL registered for target %q", msg.Key, target)
			continue
		}

		content := sanitizeSubject(msg.Subject)
		if err := postDiscord(webhookURL, content); err != nil {
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
func resolveTarget(routes []discordRoute, msg messageHeader) string {
	for _, route := range routes {
		matched := true
		for _, cond := range route.Conditions {
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
			return route.Target
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

	subject, err := new(mime.WordDecoder).DecodeHeader(header.Get("Subject"))
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

// loadDiscordRoutes は，DiscordRouteFileからルーティングルールを読み込む．
// 各行はタブ区切りで，最後の要素をtarget，それ以外の要素を条件として扱う．
// 条件は"field:regex"（一致することが条件）または"!field:regex"（一致しないことが
// 条件）の形式で指定し，1行内のすべての条件を満たした場合にのみマッチする（AND）．
// fieldは"from"，"subject"，"body"，"text"（件名と本文の結合）のいずれか．
// targetには，DISCORD_WEBHOOK_URLSで定義したtarget名，またはskipTarget（"skip"，
// 通知をスキップする予約語）を指定する．
// 空行と"#"で始まる行は無視する．ファイルが存在しない場合はルールなしとして扱う．
func loadDiscordRoutes(path string) ([]discordRoute, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open discord route file: %w", err)
	}
	defer file.Close()

	var routes []discordRoute
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
			return nil, fmt.Errorf("parse discord route %s:%d: expected \"field:regex<TAB>...<TAB>target\"", path, lineNo)
		}

		target := strings.TrimSpace(fields[len(fields)-1])
		if target == "" {
			return nil, fmt.Errorf("parse discord route %s:%d: missing target", path, lineNo)
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
					return nil, fmt.Errorf("parse discord route %s:%d: expected \"header:<name>:regex\"", path, lineNo)
				}
				field = "header:" + strings.TrimSpace(headerName)
				pattern = p
			} else {
				f, p, ok := strings.Cut(raw, ":")
				if !ok {
					return nil, fmt.Errorf("parse discord route %s:%d: expected \"field:regex\"", path, lineNo)
				}
				f = strings.TrimSpace(f)
				switch f {
				case "from", "subject", "body", "text":
				default:
					return nil, fmt.Errorf("parse discord route %s:%d: unknown field %q (want from, subject, body, text, or header:<name>)", path, lineNo, f)
				}
				field = f
				pattern = p
			}

			regex, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("compile discord route regex %s:%d: %w", path, lineNo, err)
			}

			conditions = append(conditions, routeCondition{Field: field, Regex: regex, Negate: negate})
		}
		if len(conditions) == 0 {
			return nil, fmt.Errorf("parse discord route %s:%d: at least one condition is required", path, lineNo)
		}

		routes = append(routes, discordRoute{Conditions: conditions, Target: target})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read discord route file: %w", err)
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

func postDiscord(webhookURL, content string) error {
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(body))
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
		return fmt.Errorf("discord status %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	return nil
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
