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
	DiscordWebhookURL string

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

type state struct {
	Seen map[string]bool `json:"seen"`
}

type messageHeader struct {
	Key       string
	Path      string
	Subject   string
	From      string
	Recipient string
}

var emailTokenRE = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
var lowercaseRE = regexp.MustCompile(`[a-z]`)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	filters, err := loadExcludeFilters(cfg.ExcludeFile)
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
		if err := pollOnce(cfg, filters, &st); err != nil {
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

	cfg := config{
		DiscordWebhookURL: os.Getenv("DISCORD_WEBHOOK_URL"),
		MaildirPath:       os.Getenv("MAILDIR_PATH"),
		StaffAddress:      strings.ToLower(os.Getenv("STAFF_ADDRESS")),
		StateFile:         getenvDefault("STATE_FILE", "private/state.json"),
		ExcludeFile:       getenvDefault("EXCLUDE_FILE", "private/exclude_senders.txt"),
		PollInterval:      pollInterval,
		Location:          loc,
		StartHour:         startHour,
		EndHour:           endHour,
		RequireStaff:      getenvBool("REQUIRE_STAFF_ADDRESS", true),
	}

	var missing []string
	if cfg.DiscordWebhookURL == "" {
		missing = append(missing, "DISCORD_WEBHOOK_URL")
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

func pollOnce(cfg config, filters []*regexp.Regexp, st *state) error {
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

		content := sanitizeSubject(msg.Subject)
		if err := postDiscord(cfg.DiscordWebhookURL, content); err != nil {
			return fmt.Errorf("notify %s: %w", msg.Key, err)
		}
		log.Printf("notified %s", msg.Key)
	}

	if changed {
		return saveState(cfg.StateFile, *st)
	}
	return nil
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

	header, err := textproto.NewReader(bufio.NewReader(file)).ReadMIMEHeader()
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

	return messageHeader{
		Subject:   subject,
		From:      from,
		Recipient: recipientHeaders(header),
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
