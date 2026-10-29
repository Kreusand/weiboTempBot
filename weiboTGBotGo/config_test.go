package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lookupOf(values map[string]string) lookupFunc {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func baseSettings() map[string]string {
	return map[string]string{"TELEGRAM_BOT_TOKEN": "123:abc", "ADMIN_IDS": "1, 2", "WEIBO_COOKIES": "SUB=x"}
}

func TestDefaultSettings(t *testing.T) {
	settings, err := LoadSettings(lookupOf(baseSettings()))
	if err != nil {
		t.Fatal(err)
	}
	if settings.PollInterval != 120*time.Second || settings.CommentsPollInterval != 300*time.Second || settings.RequestTimeout != 25*time.Second {
		t.Errorf("unexpected intervals %+v", settings)
	}
	if settings.MaxMediaBytes != 45_000_000 || settings.TimelinePages != 3 || settings.CommentsWatchPosts != 20 || settings.ReplyThreadsPerPoll != 12 {
		t.Errorf("unexpected defaults %+v", settings)
	}
	if !settings.AdminIDs[1] || !settings.AdminIDs[2] || len(settings.AdminIDs) != 2 || settings.StatePath != "./data/state.json" || settings.LogLevel != "INFO" {
		t.Errorf("unexpected values %+v", settings)
	}
}

func TestOneMinuteIntervalsAreAllowedButNotLess(t *testing.T) {
	values := baseSettings()
	values["POLL_INTERVAL_SECONDS"] = "60"
	values["COMMENTS_POLL_INTERVAL_SECONDS"] = "60"
	settings, err := LoadSettings(lookupOf(values))
	if err != nil || settings.PollInterval != time.Minute || settings.CommentsPollInterval != time.Minute {
		t.Fatalf("got %+v %v", settings, err)
	}
	for _, name := range []string{"POLL_INTERVAL_SECONDS", "COMMENTS_POLL_INTERVAL_SECONDS"} {
		values := baseSettings()
		values[name] = "59"
		if _, err := LoadSettings(lookupOf(values)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=59 must be rejected, got %v", name, err)
		}
	}
}

func TestInvalidSettingsAreRejected(t *testing.T) {
	cases := map[string]func(map[string]string){
		"TELEGRAM_BOT_TOKEN": func(v map[string]string) { v["TELEGRAM_BOT_TOKEN"] = "replace_with_your_token" },
		"ADMIN_IDS":          func(v map[string]string) { v["ADMIN_IDS"] = "abc" },
		"at least one":       func(v map[string]string) { v["ADMIN_IDS"] = " , " },
		"WEIBO_COOKIES":      func(v map[string]string) { delete(v, "WEIBO_COOKIES") },
		"TIMELINE_PAGES":     func(v map[string]string) { v["TIMELINE_PAGES"] = "many" },
		"MAX_MEDIA_BYTES":    func(v map[string]string) { v["MAX_MEDIA_BYTES"] = "10" },
	}
	for want, mutate := range cases {
		values := baseSettings()
		mutate(values)
		if _, err := LoadSettings(lookupOf(values)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

func TestOptionalSettings(t *testing.T) {
	values := baseSettings()
	values["TELEGRAM_CHAT_ID"] = " -100500 "
	values["STATE_PATH"] = "/tmp/x.json"
	values["LOG_LEVEL"] = "debug"
	values["MAX_MEDIA_BYTES"] = "45_000_000"
	settings, err := LoadSettings(lookupOf(values))
	if err != nil || settings.DefaultChatID != "-100500" || settings.StatePath != "/tmp/x.json" || settings.LogLevel != "DEBUG" || settings.MaxMediaBytes != 45_000_000 {
		t.Fatalf("got %+v %v", settings, err)
	}
}

func TestDotEnvParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "\ufeff# comment\n\nTELEGRAM_BOT_TOKEN=123:abc\nexport ADMIN_IDS=1,2\nWEIBO_COOKIES=\"SUB=a; SUBP=b\"\nSINGLE='quoted value'\nINLINE=value # trailing comment\nEMPTY=\nCOOKIE_WITH_HASH=a#b\nnot a line\n"
	os.WriteFile(path, []byte(content), 0o600)
	values := parseDotEnv(path)
	want := map[string]string{
		"TELEGRAM_BOT_TOKEN": "123:abc", "ADMIN_IDS": "1,2", "WEIBO_COOKIES": "SUB=a; SUBP=b",
		"SINGLE": "quoted value", "INLINE": "value", "EMPTY": "", "COOKIE_WITH_HASH": "a#b",
	}
	for key, value := range want {
		if got, ok := values[key]; !ok || got != value {
			t.Errorf("%s = %q (present %v), want %q", key, got, ok, value)
		}
	}
	if len(parseDotEnv(filepath.Join(t.TempDir(), "missing"))) != 0 {
		t.Error("a missing .env file is not an error")
	}
}

func TestRealEnvironmentBeatsDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("FROM_BOTH=file\nONLY_FILE=file\n"), 0o600)
	t.Setenv("FROM_BOTH", "environment")
	lookup := environmentWithDotEnv(path)
	if value, _ := lookup("FROM_BOTH"); value != "environment" {
		t.Errorf("got %q", value)
	}
	if value, ok := lookup("ONLY_FILE"); !ok || value != "file" {
		t.Errorf("got %q %v", value, ok)
	}
	if _, ok := lookup("NOWHERE"); ok {
		t.Error("unknown names are absent")
	}
}
