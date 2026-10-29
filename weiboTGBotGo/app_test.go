package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	adminID      = 1
	privateChat  = int64(5)
	testChat     = "-1001"
	publicChat   = "-1002"
	targetUID    = "12345"
	privateChatS = "5"
)

// harness wires an App to a fake Weibo and a fake Telegram.
type harness struct {
	t        *testing.T
	app      *App
	telegram *fakeTelegram
	weibo    *fakeWeibo
	mu       sync.Mutex
	timeline []string // JSON objects of the posts on page 1, newest first
	comments string   // body of /api/comments/show
	clock    time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, telegram: newFakeTelegram(t), weibo: newFakeWeibo(t), comments: `{"ok":1,"data":{"data":[]}}`}
	h.clock = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

	h.weibo.handlers["/api/container/getIndex"] = func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		query := r.URL.Query()
		switch {
		case query.Get("containerid") == "":
			w.Write([]byte(`{"ok":1,"data":{"userInfo":{"screen_name":"Target"},"tabsInfo":{"tabs":[{"tab_type":"weibo","containerid":"107603` + targetUID + `"}]}}}`))
		case query.Get("page") == "1":
			cards := make([]string, 0, len(h.timeline))
			for _, post := range h.timeline {
				prefix := `{"card_type":9,`
				if strings.Contains(post, `"pinned":true`) {
					prefix = `{"card_type":9,"profile_type_id":"proweibotop_1",`
				}
				cards = append(cards, prefix+`"mblog":`+post+`}`)
			}
			w.Write([]byte(`{"ok":1,"data":{"cards":[` + strings.Join(cards, ",") + `]}}`))
		default:
			w.Write([]byte(`{"ok":1,"data":{"cards":[]}}`))
		}
	}
	h.weibo.handlers["/api/comments/show"] = func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.URL.Query().Get("id") == "300" {
			w.Write([]byte(h.comments))
			return
		}
		w.Write([]byte(`{"ok":1,"data":{"data":[]}}`))
	}
	h.weibo.json("/comments/hotflow", `{"ok":0}`)

	settings := &Settings{
		AdminIDs: map[int64]bool{adminID: true}, WeiboCookies: "SUB=secret",
		PollInterval: time.Minute, CommentsPollInterval: time.Minute, RequestTimeout: 5 * time.Second,
		MaxMediaBytes: 45_000_000, MaxMediaPerPost: 10, MaxCatchupItems: 30,
		TimelinePages: 2, CommentsWatchPosts: 10, CommentsTopLevelLimit: 10, ReplyThreadsPerPoll: 5,
	}
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.clock }
	telegram := h.telegram.client()
	weibo := h.weibo.client()
	h.app = NewApp(settings, store, weibo, telegram, NewSender(telegram, settings))
	h.app.botUsername = "testbot"
	h.app.now = store.now
	return h
}

func (h *harness) advance(wait time.Duration) {
	h.mu.Lock()
	h.clock = h.clock.Add(wait)
	h.mu.Unlock()
}

func (h *harness) setTimeline(posts ...string) {
	h.mu.Lock()
	h.timeline = posts
	h.mu.Unlock()
}

func post(id int, text string, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"idstr":"%d","bid":"bid%d","text":"%s","created_at":"Wed Oct 28 19:45:57 +0800 2026","user":{"idstr":"%s","screen_name":"Target"}%s}`,
		id, id, text, targetUID, extra)
}

func (h *harness) addDestination(chatID, kind string) {
	h.t.Helper()
	if err := h.app.store.AddDestination(chatID, chatID, kind); err != nil {
		h.t.Fatal(err)
	}
}

// say sends a command as the given user in the given chat.
func (h *harness) say(userID int64, chat TelegramChat, text string) {
	h.t.Helper()
	h.app.handleUpdate(context.Background(), TelegramUpdate{Message: &TelegramMessage{From: &TelegramUser{ID: userID}, Chat: chat, Text: text}})
}

func (h *harness) sayAsAdmin(text string) {
	h.t.Helper()
	h.say(adminID, TelegramChat{ID: privateChat, Type: "private"}, text)
}

// texts returns what was sent as plain messages to a chat.
func (h *harness) texts(chatID string) []string {
	h.telegram.mu.Lock()
	defer h.telegram.mu.Unlock()
	var result []string
	for _, call := range h.telegram.calls {
		if call.method == "sendMessage" && call.fields["chat_id"] == chatID {
			result = append(result, call.fields["text"])
		}
	}
	return result
}

func (h *harness) lastReply() string {
	h.t.Helper()
	replies := h.texts(privateChatS)
	if len(replies) == 0 {
		h.t.Fatal("no reply was sent")
	}
	return replies[len(replies)-1]
}

func (h *harness) reset() {
	h.telegram.mu.Lock()
	h.telegram.calls = nil
	h.telegram.mu.Unlock()
}

func (h *harness) poll(source Source, forceComments bool) string {
	h.t.Helper()
	result, err := h.app.pollSource(context.Background(), source, forceComments)
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *harness) addAccount(kind string) Source {
	h.t.Helper()
	source, err := h.app.store.AddSource(targetUID, "Target", kind)
	if err != nil {
		h.t.Fatal(err)
	}
	return source
}

// ---- command routing and permissions ----

func TestParseCommand(t *testing.T) {
	cases := []struct {
		text    string
		bot     string
		name    string
		args    string
		matches bool
	}{
		{"/help", "testbot", "help", "", true},
		{"/ADD 1234 public Name", "testbot", "add", "1234 public Name", true},
		{"/add@testbot 1234", "testbot", "add", "1234", true},
		{"/add@TestBot 1234", "testbot", "add", "1234", true},
		{"/add@otherbot 1234", "testbot", "", "", false},
		{"/add@otherbot 1234", "", "add", "1234", true},
		{"hello", "testbot", "", "", false},
		{"/", "testbot", "", "", false},
		{"   ", "testbot", "", "", false},
	}
	for _, tc := range cases {
		name, args, ok := parseCommand(tc.text, tc.bot)
		if ok != tc.matches || name != tc.name || strings.Join(args, " ") != tc.args {
			t.Errorf("parseCommand(%q) = %q %v %v", tc.text, name, args, ok)
		}
	}
}

func TestParseLatestArgs(t *testing.T) {
	cases := []struct {
		args  []string
		key   string
		count int
		media bool
	}{
		{nil, "", 1, false},
		{[]string{"7198559139", "3", "media"}, "7198559139", 3, true},
		{[]string{"Nana", "7mi", "99"}, "Nana 7mi", 10, false},
		{[]string{"0"}, "", 1, false},
		{[]string{"MEDIA", "2"}, "", 2, true},
	}
	for _, tc := range cases {
		key, count, media := parseLatestArgs(tc.args)
		if key != tc.key || count != tc.count || media != tc.media {
			t.Errorf("parseLatestArgs(%v) = %q %d %v", tc.args, key, count, media)
		}
	}
}

func TestCommandPermissions(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	h.addDestination(publicChat, kindPublic)
	cases := []struct {
		name      string
		user      int64
		chat      TelegramChat
		wantReply bool
		replyTo   string
	}{
		{"admin in private", adminID, TelegramChat{ID: 5, Type: "private"}, true, "5"},
		{"admin in a test group", adminID, TelegramChat{ID: -1001, Type: "supergroup"}, true, testChat},
		{"admin in a public group", adminID, TelegramChat{ID: -1002, Type: "supergroup"}, false, ""},
		{"admin in an unknown group", adminID, TelegramChat{ID: -1999, Type: "group"}, false, ""},
		{"stranger in a public group", 2, TelegramChat{ID: -1002, Type: "supergroup"}, false, ""},
		{"stranger in a test group", 2, TelegramChat{ID: -1001, Type: "supergroup"}, false, ""},
		{"stranger in private", 2, TelegramChat{ID: 7, Type: "private"}, true, "7"},
	}
	for _, tc := range cases {
		h.reset()
		h.say(tc.user, tc.chat, "/list")
		var got []string
		for _, chat := range []string{"5", "7", testChat, publicChat, "-1999"} {
			got = append(got, h.texts(chat)...)
		}
		if tc.wantReply != (len(got) == 1) {
			t.Errorf("%s: replies %v", tc.name, got)
			continue
		}
		if tc.wantReply && len(h.texts(tc.replyTo)) != 1 {
			t.Errorf("%s: the reply must go to the chat it came from", tc.name)
		}
	}
	h.reset()
	h.say(2, TelegramChat{ID: 7, Type: "private"}, "/list")
	if reply := h.texts("7")[0]; strings.Contains(reply, "ADMIN") || reply != "This command is not available." {
		t.Errorf("strangers get a neutral answer, got %q", reply)
	}
}

func TestUsernameMatchesTestDestination(t *testing.T) {
	h := newHarness(t)
	h.addDestination("@MyTestChannel", kindTest)
	h.say(adminID, TelegramChat{ID: -1500, Type: "supergroup", Username: "mytestchannel"}, "/list")
	if len(h.texts("-1500")) != 1 {
		t.Error("a channel registered by @name counts as a test destination")
	}
}

func TestUnknownAndForeignCommandsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.sayAsAdmin("/nonsense")
	h.sayAsAdmin("/list@otherbot")
	h.sayAsAdmin("just text")
	if len(h.telegram.methods()) != 0 {
		t.Errorf("nothing may be sent, got %v", h.telegram.methods())
	}
}

// ---- destinations ----

func TestDestSetCreatesChecksAndSwitches(t *testing.T) {
	h := newHarness(t)
	h.telegram.reply = func(call recordedCall, _ int) (int, string) {
		if call.method == "getChat" {
			switch call.fields["chat_id"] {
			case "-5316928386":
				return 200, `{"ok":true,"result":{"id":-5316928386,"type":"group","title":"My Public Group"}}`
			case "@chan":
				return 200, `{"ok":true,"result":{"id":-1003,"type":"channel","username":"chan"}}`
			}
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`
		}
		return 200, `{"ok":true,"result":true}`
	}
	h.sayAsAdmin("/destset -5316928386 public")
	if got := h.lastReply(); !strings.Contains(got, "Added My Public Group") || !strings.Contains(got, "public") {
		t.Errorf("got %q", got)
	}
	if h.app.store.DestinationKinds()["-5316928386"] != kindPublic {
		t.Error("destination not created as public")
	}
	h.sayAsAdmin("/destset -5316928386 test")
	if got := h.lastReply(); got != "-5316928386 is now a test destination." {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/destset @chan public")
	if h.app.store.Destinations("")[1].Name != "@chan" {
		t.Error("a channel without a title is named after its username")
	}
	h.sayAsAdmin("/destset -100999999 public")
	if got := h.lastReply(); !strings.Contains(got, "Could not access") || !strings.Contains(got, "chat not found") {
		t.Errorf("got %q", got)
	}
	if len(h.app.store.Destinations("")) != 2 {
		t.Error("an unreachable chat must not be registered")
	}
	h.sayAsAdmin("/destset abc public")
	if got := h.lastReply(); !strings.Contains(got, "numeric Chat ID") {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/destset -5316928386")
	if got := h.lastReply(); !strings.HasPrefix(got, "Usage:") {
		t.Errorf("got %q", got)
	}
}

func TestDestAddListRemoveAndTest(t *testing.T) {
	h := newHarness(t)
	h.sayAsAdmin("/destadd -100123456 My group")
	if got := h.lastReply(); !strings.Contains(got, "test destination") {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/destadd nonsense")
	if got := h.lastReply(); !strings.Contains(got, "numeric Chat ID") {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/destlist")
	if got := h.lastReply(); !strings.Contains(got, "My group: -100123456 (test)") {
		t.Errorf("got %q", got)
	}
	h.addDestination(publicChat, kindPublic)
	h.reset()
	h.sayAsAdmin("/test")
	if len(h.texts("-100123456")) != 1 || len(h.texts(publicChat)) != 0 {
		t.Errorf("/test must reach test destinations only: test=%v public=%v", h.texts("-100123456"), h.texts(publicChat))
	}
	h.sayAsAdmin("/destremove -100123456")
	h.sayAsAdmin("/destremove -100123456")
	if got := h.lastReply(); got != "Destination not found." {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/test")
	if got := h.lastReply(); !strings.Contains(got, "No test destination") {
		t.Errorf("got %q", got)
	}
}

// ---- accounts, baselines and delivery ----

func TestAddBuildsTheBaselineImmediately(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	h.setTimeline(post(300, "an existing post", ""), post(290, "an older post", ""))
	h.sayAsAdmin("/add " + targetUID)
	replies := h.texts(privateChatS)
	if len(replies) != 2 || !strings.Contains(replies[0], "as a test account") || !strings.Contains(replies[1], "Baseline ready") {
		t.Fatalf("replies %v", replies)
	}
	source, _ := h.app.store.GetSource(targetUID)
	if !source.Initialized || !source.CommentsInitialized || source.Name != "Target" {
		t.Errorf("baseline missing: %+v", source)
	}
	if len(h.texts(testChat)) != 0 {
		t.Error("existing posts must not be pushed")
	}
}

func TestAddParsingAndErrors(t *testing.T) {
	h := newHarness(t)
	h.setTimeline()
	h.sayAsAdmin("/add 5577657869 public My Nice Name")
	source, ok := h.app.store.GetSource("5577657869")
	if !ok || source.Kind != kindPublic || source.Name == "" {
		t.Fatalf("got %+v", source)
	}
	h.sayAsAdmin("/add 5577657870 test")
	if source, _ := h.app.store.GetSource("5577657870"); source.Kind != kindTest {
		t.Error("explicit test")
	}
	h.sayAsAdmin("/add 5577657871 Plain")
	if source, _ := h.app.store.GetSource("5577657871"); source.Kind != kindTest || source.Name != "Plain" {
		t.Errorf("default kind is test, got %+v", source)
	}
	h.sayAsAdmin("/add 5577657871")
	if got := h.texts(privateChatS); !strings.Contains(strings.Join(got, "|"), "already exists") {
		t.Errorf("duplicate: %v", got)
	}
	h.sayAsAdmin("/add abc")
	if got := h.lastReply(); !strings.Contains(got, "numeric Weibo user id") {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/add")
	if got := h.lastReply(); !strings.HasPrefix(got, "Usage:") {
		t.Errorf("got %q", got)
	}
}

func TestNewPostGoesToTestOnlyForTestAccounts(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	h.addDestination(publicChat, kindPublic)
	source := h.addAccount(kindTest)
	h.setTimeline(post(300, "old", ""))
	h.poll(source, true)
	h.setTimeline(post(301, "brand new post", ""), post(300, "old", ""))
	source, _ = h.app.store.GetSourceByID(source.ID)
	result := h.poll(source, false)
	if !strings.Contains(result, "1 new posts") || !strings.Contains(result, "sent 1") {
		t.Errorf("result %q", result)
	}
	if len(h.texts(testChat)) != 1 || len(h.texts(publicChat)) != 0 {
		t.Errorf("test=%v public=%v", h.texts(testChat), h.texts(publicChat))
	}
}

func TestNewPostGoesToEveryoneForPublicAccountsWithoutDiagnostics(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	h.addDestination(publicChat, kindPublic)
	source := h.addAccount(kindPublic)
	h.setTimeline(post(300, "old", ""))
	h.poll(source, true)
	// The new post has a photo that cannot be downloaded: only the test chat may hear about it.
	broken := post(301, "new with photo", `"pics":[{"url":"`+h.weibo.server.URL+`/missing.jpg"}]`)
	h.setTimeline(broken, post(300, "old", ""))
	source, _ = h.app.store.GetSourceByID(source.ID)
	h.app.sender.mediaHosts = []string{"127.0.0.1"}
	h.poll(source, false)
	publicTexts, testTexts := h.texts(publicChat), h.texts(testChat)
	if len(publicTexts) != 1 || len(testTexts) != 1 {
		t.Fatalf("test=%v public=%v", testTexts, publicTexts)
	}
	if strings.Contains(publicTexts[0], "missing.jpg") || !strings.Contains(publicTexts[0], "new with photo") {
		t.Errorf("the public chat gets content only: %q", publicTexts[0])
	}
	if !strings.Contains(testTexts[0], "missing.jpg") {
		t.Errorf("the test chat gets the diagnostic note: %q", testTexts[0])
	}
}

func TestPostsAreSentOldestFirstAndPinnedOldPostIsNotNews(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	source := h.addAccount(kindTest)
	h.setTimeline(post(300, "current", ""), post(10, "ancient pinned", `"pinned":true`))
	h.poll(source, true)
	h.reset()

	// Two new posts appear, and the account pins another very old post that was never seen.
	h.setTimeline(
		post(5, "old post pinned just now", `"pinned":true`),
		post(302, "second new", ""), post(301, "first new", ""), post(300, "current", ""),
	)
	source, _ = h.app.store.GetSourceByID(source.ID)
	h.poll(source, false)
	texts := h.texts(testChat)
	if len(texts) != 2 || !strings.Contains(texts[0], "first new") || !strings.Contains(texts[1], "second new") {
		t.Fatalf("new posts go out oldest first and the old pinned post stays quiet: %v", texts)
	}
	h.reset()
	h.poll(source, false)
	if len(h.texts(testChat)) != 0 {
		t.Error("nothing is sent twice")
	}
}

func TestNewPostThatIsPinnedImmediatelyIsStillNews(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	source := h.addAccount(kindTest)
	h.setTimeline(post(300, "current", ""))
	h.poll(source, true)
	h.setTimeline(post(310, "fresh and pinned", `"pinned":true`), post(300, "current", ""))
	source, _ = h.app.store.GetSourceByID(source.ID)
	h.poll(source, false)
	if texts := h.texts(testChat); len(texts) != 1 || !strings.Contains(texts[0], "fresh and pinned") {
		t.Errorf("got %v", texts)
	}
}

func TestCommentsBaselineThenNewComment(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	source := h.addAccount(kindTest)
	h.setTimeline(post(300, "my post", ""))
	h.comments = `{"ok":1,"data":{"data":[{"idstr":"1","text":"old comment","user":{"idstr":"` + targetUID + `","screen_name":"Target"}}]}}`
	h.poll(source, false)
	source, _ = h.app.store.GetSourceByID(source.ID)
	if !source.CommentsInitialized || len(h.texts(testChat)) != 0 {
		t.Fatal("existing comments are only a baseline")
	}

	h.comments = `{"ok":1,"data":{"data":[
		{"idstr":"2","text":"brand new comment","user":{"idstr":"` + targetUID + `","screen_name":"Target"}},
		{"idstr":"1","text":"old comment","user":{"idstr":"` + targetUID + `","screen_name":"Target"}}]}}`
	h.advance(30 * time.Second)
	h.poll(source, false)
	if len(h.texts(testChat)) != 0 {
		t.Error("comments are not scanned again before the interval has passed")
	}
	h.advance(31 * time.Second)
	source, _ = h.app.store.GetSourceByID(source.ID)
	result := h.poll(source, false)
	texts := h.texts(testChat)
	if len(texts) != 1 || !strings.Contains(texts[0], "brand new comment") || !strings.Contains(texts[0], tagComment) {
		t.Fatalf("texts %v (%s)", texts, result)
	}
	if !strings.Contains(texts[0], labelParentLink+"https://weibo.com/"+targetUID+"/bid300") {
		t.Errorf("the comment links to its post: %q", texts[0])
	}
}

// ---- failures and alerts ----

func TestPollingFailureAlertsTestDestinationsOnlyAndIsThrottled(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	h.addDestination(publicChat, kindPublic)
	h.addAccount(kindPublic)
	h.weibo.handlers["/api/container/getIndex"] = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(432) }

	h.app.pollRound(context.Background())
	h.app.pollRound(context.Background())
	if len(h.texts(publicChat)) != 0 {
		t.Fatalf("errors never reach a public chat: %v", h.texts(publicChat))
	}
	alerts := h.texts(testChat)
	if len(alerts) != 1 || !strings.Contains(alerts[0], "[ERROR] Polling failed for Target") {
		t.Fatalf("one throttled alert expected, got %v", alerts)
	}
	source, _ := h.app.store.GetSource(targetUID)
	if source.LastError == "" {
		t.Error("the error is recorded on the account")
	}
	h.advance(alertCooldown + time.Minute)
	h.app.pollRound(context.Background())
	if len(h.texts(testChat)) != 2 {
		t.Error("the alert comes again after the cooldown")
	}
}

func TestFailedDeliveryIsRetriedAfterBackoffAndAlerted(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	h.addDestination(publicChat, kindPublic)
	source := h.addAccount(kindPublic)
	h.setTimeline(post(300, "old", ""))
	h.poll(source, true)
	h.reset()

	blocked := true
	h.telegram.reply = func(call recordedCall, _ int) (int, string) {
		if blocked && call.fields["chat_id"] == publicChat && strings.Contains(call.fields["text"], "second") {
			return 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked"}`
		}
		return 200, `{"ok":true,"result":true}`
	}
	h.setTimeline(post(301, "second", ""), post(300, "old", ""))
	source, _ = h.app.store.GetSourceByID(source.ID)
	result := h.poll(source, false)
	if !strings.Contains(result, "sent 1, failed 1") {
		t.Errorf("result %q", result)
	}
	alert := ""
	for _, text := range h.texts(testChat) {
		if strings.Contains(text, "[ERROR] Delivery to "+publicChat) {
			alert = text
		}
	}
	if alert == "" || !strings.Contains(alert, "bot was kicked") {
		t.Errorf("the failure is reported to the test chat: %v", h.texts(testChat))
	}
	for _, text := range h.texts(publicChat) {
		if strings.Contains(text, "ERROR") {
			t.Error("a public chat never sees errors")
		}
	}

	blocked = false
	if sent, failed := h.app.deliverPending(context.Background(), 0, 10); sent != 0 || failed != 0 {
		t.Errorf("the delivery must wait for its backoff, sent %d failed %d", sent, failed)
	}
	h.advance(31 * time.Second)
	sent, failed := h.app.deliverPending(context.Background(), 0, 10)
	if sent != 1 || failed != 0 {
		t.Errorf("sent %d failed %d", sent, failed)
	}
}

func TestPausedLoopDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	h.addAccount(kindTest)
	h.setTimeline(post(300, "x", ""))
	h.sayAsAdmin("/pause")
	h.app.pollRound(context.Background())
	if source, _ := h.app.store.GetSource(targetUID); source.Initialized {
		t.Error("a paused bot must not poll")
	}
	h.sayAsAdmin("/resume")
	h.app.pollRound(context.Background())
	if source, _ := h.app.store.GetSource(targetUID); !source.Initialized {
		t.Error("a resumed bot polls again")
	}
}

// ---- /latest, /srcset, /check, /status and the rest ----

func TestLatestSendsToTestDestinationsAndSavesNothing(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	h.addDestination(publicChat, kindPublic)
	h.addAccount(kindPublic)
	h.setTimeline(
		post(100, "pinned old", `"pinned":true`),
		post(303, "newest text", ""), post(302, "photo post", `"pics":[{"url":"`+h.weibo.server.URL+`/missing.jpg"}]`), post(301, "older text", ""),
	)
	h.reset()
	h.sayAsAdmin("/latest 2")
	if len(h.texts(publicChat)) != 0 {
		t.Fatal("/latest never reaches a public destination")
	}
	texts := h.texts(testChat)
	if len(texts) != 2 || !strings.Contains(texts[0], "photo post") || !strings.Contains(texts[1], "newest text") {
		t.Fatalf("the two newest posts, oldest first: %v", texts)
	}
	if reply := h.lastReply(); !strings.Contains(reply, "Pinned posts skipped: 1") || !strings.Contains(reply, "Nothing was queued") {
		t.Errorf("reply %q", reply)
	}
	if source, _ := h.app.store.GetSource(targetUID); source.Initialized || len(h.app.store.PendingDeliveries(0, 10)) != 0 {
		t.Error("/latest must not touch the stored state")
	}

	h.reset()
	h.sayAsAdmin("/latest media")
	if texts := h.texts(testChat); len(texts) != 1 || !strings.Contains(texts[0], "photo post") {
		t.Errorf("media filter: %v", texts)
	}
	h.sayAsAdmin("/latest 99999 5")
	if got := h.lastReply(); !strings.Contains(got, "Could not read the timeline") && !strings.Contains(got, "Sent") {
		t.Errorf("an unknown numeric UID is tried directly, got %q", got)
	}
	h.sayAsAdmin("/latest nobody")
	if got := h.lastReply(); !strings.Contains(got, "Account not found") {
		t.Errorf("got %q", got)
	}
}

func TestLatestNeedsATestDestinationAndAnAccount(t *testing.T) {
	h := newHarness(t)
	h.sayAsAdmin("/latest")
	if got := h.lastReply(); !strings.Contains(got, "No active account") {
		t.Errorf("got %q", got)
	}
	h.addAccount(kindTest)
	h.sayAsAdmin("/latest")
	if got := h.lastReply(); !strings.Contains(got, "No test destination") {
		t.Errorf("got %q", got)
	}
}

func TestSrcSetAndList(t *testing.T) {
	h := newHarness(t)
	h.addAccount(kindTest)
	h.sayAsAdmin("/srcset Target public")
	if got := h.lastReply(); got != "Target is now a public account." {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/srcset " + targetUID + " test")
	if got := h.lastReply(); !strings.Contains(got, "now a test account") {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/srcset nobody public")
	if got := h.lastReply(); !strings.Contains(got, "Account not found") {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/srcset Target")
	if got := h.lastReply(); !strings.HasPrefix(got, "Usage:") {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/list")
	if got := h.lastReply(); !strings.Contains(got, "Target (12345): test; running; post baseline pending") {
		t.Errorf("got %q", got)
	}
}

func TestCheckEnableDisableRemoveBackfillStatusMeHelp(t *testing.T) {
	h := newHarness(t)
	h.addDestination(testChat, kindTest)
	h.weibo.json("/api/config", `{"ok":1,"data":{"login":true,"uid":"777"}}`)
	h.sayAsAdmin("/check")
	if got := h.lastReply(); !strings.Contains(got, "No active accounts") {
		t.Errorf("got %q", got)
	}
	h.addAccount(kindTest)
	h.setTimeline(post(300, "a", ""), post(299, "b", ""), post(298, "c", ""))
	h.sayAsAdmin("/check")
	if got := h.lastReply(); !strings.Contains(got, "read 3 posts") {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/check nobody")
	if got := h.lastReply(); !strings.Contains(got, "Account not found") {
		t.Errorf("got %q", got)
	}

	h.reset()
	h.sayAsAdmin("/backfill " + targetUID + " 2")
	if got := h.lastReply(); !strings.Contains(got, "Queued 2") || !strings.Contains(got, "sent 2") {
		t.Errorf("got %q", got)
	}
	if texts := h.texts(testChat); len(texts) != 2 || !strings.Contains(texts[0], "b") || !strings.Contains(texts[1], "a") {
		// The two newest baseline posts, oldest first.
		t.Errorf("backfill texts %v", texts)
	}
	h.sayAsAdmin("/backfill " + targetUID + " abc")
	if got := h.lastReply(); !strings.Contains(got, "must be an integer") {
		t.Errorf("got %q", got)
	}

	h.sayAsAdmin("/disable Target")
	if source, _ := h.app.store.GetSource(targetUID); source.Active {
		t.Error("not disabled")
	}
	h.sayAsAdmin("/enable Target")
	if source, _ := h.app.store.GetSource(targetUID); !source.Active {
		t.Error("not enabled")
	}
	h.sayAsAdmin("/disable")
	if got := h.lastReply(); !strings.HasPrefix(got, "Usage: /disable") {
		t.Errorf("got %q", got)
	}

	h.sayAsAdmin("/me")
	if got := h.lastReply(); !strings.Contains(got, "UID=777") {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/status")
	if got := h.lastReply(); !strings.Contains(got, "Accounts: 1 active of 1") || !strings.Contains(got, "Post poll interval: 60 s") {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/help")
	if got := h.lastReply(); !strings.Contains(got, "/srcset") || !strings.Contains(got, "/latest") {
		t.Errorf("got %q", got)
	}
	h.sayAsAdmin("/start")
	h.sayAsAdmin("/remove Target")
	if len(h.app.store.ListSources(false)) != 0 {
		t.Error("not removed")
	}
	h.sayAsAdmin("/remove Target")
	if got := h.lastReply(); got != "Account not found." {
		t.Errorf("got %q", got)
	}
}

func TestRunUpdatesHandlesCommandsUntilCancelled(t *testing.T) {
	h := newHarness(t)
	var once sync.Once
	h.telegram.reply = func(call recordedCall, _ int) (int, string) {
		if call.method != "getUpdates" {
			return 200, `{"ok":true,"result":true}`
		}
		time.Sleep(5 * time.Millisecond)
		body := `{"ok":true,"result":[]}`
		once.Do(func() {
			body = `{"ok":true,"result":[{"update_id":41,"message":{"message_id":1,"from":{"id":1},"chat":{"id":5,"type":"private"},"text":"/pause"}}]}`
		})
		return 200, body
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.app.runUpdates(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for !h.app.store.IsPaused() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runUpdates did not stop after cancellation")
	}
	if !h.app.store.IsPaused() {
		t.Error("the command from the update was not executed")
	}
	h.telegram.mu.Lock()
	var offsets []string
	for _, call := range h.telegram.calls {
		if call.method == "getUpdates" {
			offsets = append(offsets, call.fields["offset"])
		}
	}
	h.telegram.mu.Unlock()
	if len(offsets) < 2 || offsets[0] != "0" || offsets[1] != "42" {
		t.Errorf("the offset must move past handled updates: %v", offsets)
	}
}
