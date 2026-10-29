package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Settings holds the configuration. The environment variable names are the ones the project has always used.
type Settings struct {
	TelegramToken         string
	DefaultChatID         string
	AdminIDs              map[int64]bool
	WeiboCookies          string
	PollInterval          time.Duration
	CommentsPollInterval  time.Duration
	RequestTimeout        time.Duration
	MaxMediaBytes         int
	MaxMediaPerPost       int
	MaxCatchupItems       int
	TimelinePages         int
	CommentsWatchPosts    int
	CommentsTopLevelLimit int
	ReplyThreadsPerPoll   int
	DatabasePath          string
	LogLevel              string
}

type lookupFunc func(name string) (string, bool)

// parseDotEnv reads KEY=VALUE lines. Comments, blank lines, an optional "export " prefix and
// matching quotes around a value are understood.
func parseDotEnv(path string) map[string]string {
	values := map[string]string{}
	file, err := os.Open(path)
	if err != nil {
		return values
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		} else if index := strings.Index(value, " #"); index >= 0 {
			value = strings.TrimSpace(value[:index])
		}
		values[name] = value
	}
	return values
}

// environmentWithDotEnv returns a lookup where real environment variables win over the .env file.
func environmentWithDotEnv(dotEnvPath string) lookupFunc {
	fileValues := parseDotEnv(dotEnvPath)
	return func(name string) (string, bool) {
		if value, ok := os.LookupEnv(name); ok {
			return value, true
		}
		value, ok := fileValues[name]
		return value, ok
	}
}

func intSetting(lookup lookupFunc, name string, defaultValue, minimum int) (int, error) {
	raw := strings.TrimSpace(strconv.Itoa(defaultValue))
	if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
		raw = strings.TrimSpace(value)
	}
	value, err := strconv.Atoi(strings.ReplaceAll(raw, "_", ""))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if value < minimum {
		return 0, fmt.Errorf("%s must be at least %d", name, minimum)
	}
	return value, nil
}

func requiredSetting(lookup lookupFunc, name string) (string, error) {
	value, _ := lookup(name)
	value = strings.TrimSpace(value)
	if value == "" || value == "replace_me" || value == "replace_with_your_token" {
		return "", fmt.Errorf("%s is missing, set it in .env", name)
	}
	return value, nil
}

// LoadSettings validates and returns the configuration.
func LoadSettings(lookup lookupFunc) (*Settings, error) {
	token, err := requiredSetting(lookup, "TELEGRAM_BOT_TOKEN")
	if err != nil {
		return nil, err
	}
	adminRaw, _ := lookup("ADMIN_IDS")
	admins := map[int64]bool{}
	for _, part := range strings.Split(adminRaw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ADMIN_IDS must be a comma separated list of numeric Telegram user ids")
		}
		admins[id] = true
	}
	if len(admins) == 0 {
		return nil, fmt.Errorf("ADMIN_IDS needs at least one numeric Telegram user id")
	}
	cookies, err := requiredSetting(lookup, "WEIBO_COOKIES")
	if err != nil {
		return nil, fmt.Errorf("WEIBO_COOKIES is missing, set the mobile web login cookie in .env")
	}

	settings := &Settings{TelegramToken: token, AdminIDs: admins, WeiboCookies: cookies}
	settings.DefaultChatID, _ = lookup("TELEGRAM_CHAT_ID")
	settings.DefaultChatID = strings.TrimSpace(settings.DefaultChatID)
	settings.DatabasePath = "./data/state.db"
	if value, ok := lookup("DATABASE_PATH"); ok && strings.TrimSpace(value) != "" {
		settings.DatabasePath = strings.TrimSpace(value)
	} else if value, ok := lookup("STATE_PATH"); ok && strings.TrimSpace(value) != "" {
		// Ignore the old JSON filename; the SQLite store must use a database path.
		legacyPath := strings.TrimSpace(value)
		if !strings.HasSuffix(strings.ToLower(legacyPath), ".json") {
			settings.DatabasePath = legacyPath
		}
	}
	settings.LogLevel = "INFO"
	if value, ok := lookup("LOG_LEVEL"); ok && strings.TrimSpace(value) != "" {
		settings.LogLevel = strings.ToUpper(strings.TrimSpace(value))
	}

	var seconds int
	if seconds, err = intSetting(lookup, "POLL_INTERVAL_SECONDS", 120, 60); err != nil {
		return nil, err
	}
	settings.PollInterval = time.Duration(seconds) * time.Second
	if seconds, err = intSetting(lookup, "COMMENTS_POLL_INTERVAL_SECONDS", 300, 60); err != nil {
		return nil, err
	}
	settings.CommentsPollInterval = time.Duration(seconds) * time.Second
	if seconds, err = intSetting(lookup, "REQUEST_TIMEOUT_SECONDS", 25, 5); err != nil {
		return nil, err
	}
	settings.RequestTimeout = time.Duration(seconds) * time.Second

	for _, field := range []struct {
		target       *int
		name         string
		defaultValue int
		minimum      int
	}{
		{&settings.MaxMediaBytes, "MAX_MEDIA_BYTES", 45_000_000, 1_000_000},
		{&settings.MaxMediaPerPost, "MAX_MEDIA_PER_POST", 10, 1},
		{&settings.MaxCatchupItems, "MAX_CATCHUP_ITEMS", 30, 1},
		{&settings.TimelinePages, "TIMELINE_PAGES", 3, 1},
		{&settings.CommentsWatchPosts, "COMMENTS_WATCH_POSTS", 20, 1},
		{&settings.CommentsTopLevelLimit, "COMMENTS_TOPLEVEL_LIMIT", 20, 1},
		{&settings.ReplyThreadsPerPoll, "REPLY_THREADS_PER_POLL", 12, 1},
	} {
		if *field.target, err = intSetting(lookup, field.name, field.defaultValue, field.minimum); err != nil {
			return nil, err
		}
	}
	return settings, nil
}
