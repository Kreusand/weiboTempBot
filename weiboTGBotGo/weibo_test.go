package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWeibo serves canned responses by path and records the requests it received.
type fakeWeibo struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []string
	handlers map[string]func(w http.ResponseWriter, r *http.Request)
}

func newFakeWeibo(t *testing.T) *fakeWeibo {
	t.Helper()
	fake := &fakeWeibo{handlers: map[string]func(http.ResponseWriter, *http.Request){}}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.requests = append(fake.requests, r.URL.Path+"?"+r.URL.RawQuery)
		handler := fake.handlers[r.URL.Path]
		fake.mu.Unlock()
		if r.Header.Get("Cookie") != "SUB=secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if handler == nil {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeWeibo) json(path, body string) {
	f.handlers[path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}
}

func (f *fakeWeibo) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, request := range f.requests {
		if strings.HasPrefix(request, prefix) {
			total++
		}
	}
	return total
}

func (f *fakeWeibo) client() *WeiboClient {
	client := NewWeiboClient(&Settings{
		WeiboCookies: "SUB=secret", RequestTimeout: 5 * time.Second, TimelinePages: 2,
		CommentsWatchPosts: 10, CommentsTopLevelLimit: 10, ReplyThreadsPerPoll: 5,
	})
	client.baseURL = f.server.URL
	client.allowedHosts = append([]string{"127.0.0.1"}, weiboAPIHosts...)
	return client
}

func TestGetTimelineReadsPagesAndMarksPinned(t *testing.T) {
	fake := newFakeWeibo(t)
	fake.handlers["/api/container/getIndex"] = func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		switch {
		case query.Get("containerid") == "":
			w.Write([]byte(`{"ok":1,"data":{"userInfo":{"screen_name":"Target"},"tabsInfo":{"tabs":[{"tab_type":"profile"},{"tab_type":"weibo","containerid":"1076031234"}]}}}`))
		case query.Get("page") == "1":
			w.Write([]byte(`{"ok":1,"data":{"cards":[
				{"card_type":9,"profile_type_id":"proweibotop_1","mblog":{"idstr":"100","text":"pinned"}},
				{"card_type":9,"mblog":{"idstr":"5359216613130705","text":"newest"}},
				{"card_type":11,"card_group":[{"mblog":{"idstr":"300","text":"grouped"}}]},
				{"card_type":9,"mblog":{"idstr":"300","text":"duplicate"}}]}}`))
		case query.Get("page") == "2":
			w.Write([]byte(`{"ok":1,"data":{"cards":[{"card_type":9,"mblog":{"idstr":"50","text":"older"}}]}}`))
		default:
			w.Write([]byte(`{"ok":1,"data":{"cards":[]}}`))
		}
	}
	name, statuses, err := fake.client().GetTimeline(context.Background(), "1234")
	if err != nil {
		t.Fatal(err)
	}
	if name != "Target" || len(statuses) != 4 {
		t.Fatalf("name %q, %d statuses", name, len(statuses))
	}
	if !isPinned(statuses[0]) || isPinned(statuses[1]) {
		t.Error("only the card with a pinned profile type is pinned")
	}
	if statusIDOf(statuses[1]) != "5359216613130705" {
		t.Errorf("big ids must keep every digit, got %s", statusIDOf(statuses[1]))
	}
	if asStr(statuses[2]["text"]) != "grouped" {
		t.Error("the first copy of a duplicated post wins")
	}
}

func TestGetTimelineFallsBackToDefaultContainerAndReportsEmpty(t *testing.T) {
	fake := newFakeWeibo(t)
	fake.json("/api/container/getIndex", `{"ok":0,"msg":"user does not exist"}`)
	_, _, err := fake.client().GetTimeline(context.Background(), "1234")
	if err == nil || !strings.Contains(err.Error(), "user does not exist") {
		t.Fatalf("expected the API message, got %v", err)
	}
	if fake.count("/api/container/getIndex?containerid=1076031234") == 0 {
		t.Error("the default container id must be tried")
	}
}

func TestAuthFailuresAreAuthErrors(t *testing.T) {
	fake := newFakeWeibo(t)
	fake.json("/api/config", `{"ok":-100}`)
	fake.json("/limited", `{"ok":1}`)
	fake.handlers["/blocked"] = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(432) }
	fake.handlers["/server"] = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500); w.Write([]byte("oops")) }
	fake.handlers["/html"] = func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>passport.weibo.cn</html>")) }
	fake.handlers["/text"] = func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("plain words")) }
	fake.json("/retcode", `{"retcode":6102}`)
	client := fake.client()
	ctx := context.Background()

	for path, wantAuth := range map[string]bool{"/api/config": true, "/blocked": true, "/html": true, "/retcode": true, "/server": false, "/text": false} {
		_, err := client.getJSON(ctx, path, nil, "")
		if err == nil {
			t.Errorf("%s: expected an error", path)
			continue
		}
		if isAuthError(err) != wantAuth || !isWeiboError(err) {
			t.Errorf("%s: auth=%v want %v (%v)", path, isAuthError(err), wantAuth, err)
		}
	}
	if _, err := client.getJSON(ctx, "/limited", nil, ""); err != nil {
		t.Errorf("a normal response must pass: %v", err)
	}
	wrongCookie := fake.client()
	wrongCookie.cookies = "SUB=wrong"
	if _, err := wrongCookie.getJSON(ctx, "/limited", nil, ""); !isAuthError(err) {
		t.Errorf("a rejected cookie is an auth error, got %v", err)
	}
}

func TestRedirectsStayOnWeiboHosts(t *testing.T) {
	fake := newFakeWeibo(t)
	fake.handlers["/leave"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/steal", http.StatusFound)
	}
	fake.handlers["/stay"] = func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/target", http.StatusFound) }
	fake.json("/target", `{"ok":1,"hello":"world"}`)
	fake.handlers["/loop"] = func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) }
	client := fake.client()
	ctx := context.Background()

	if _, err := client.getJSON(ctx, "/leave", nil, ""); err == nil || !strings.Contains(err.Error(), "outside Weibo") {
		t.Errorf("a redirect off Weibo must stop, got %v", err)
	}
	data, err := client.getJSON(ctx, "/stay", nil, "")
	if err != nil || asStr(data["hello"]) != "world" {
		t.Errorf("a redirect inside the allowed hosts is followed: %v %v", data, err)
	}
	if _, err := client.getJSON(ctx, "/loop", nil, ""); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Errorf("a redirect loop must stop, got %v", err)
	}
}

func TestCheckLogin(t *testing.T) {
	fake := newFakeWeibo(t)
	fake.json("/api/config", `{"ok":1,"data":{"login":true,"uid":"777"}}`)
	info, err := fake.client().CheckLogin(context.Background())
	if err != nil || info.UID != "777" {
		t.Fatalf("got %+v %v", info, err)
	}
	fake.json("/api/config", `{"ok":1,"data":{"login":false}}`)
	if _, err := fake.client().CheckLogin(context.Background()); !isAuthError(err) {
		t.Fatalf("not logged in must be an auth error, got %v", err)
	}
}

func TestToFeedItemExpandsLongTextOnce(t *testing.T) {
	fake := newFakeWeibo(t)
	fake.json("/statuses/extend", `{"ok":1,"data":{"longTextContent":"the <b>full</b> text<br>second line"}}`)
	client := fake.client()
	status := parseObj(t, `{"idstr":"100","bid":"abc","isLongText":true,"text":"short...","created_at":"Thu Oct 29 03:32:26 +0800 2026",
		"user":{"screen_name":"Target"},"pics":[{"large":{"url":"https://img.example/a.jpg"}}],"pic_num":1}`)
	item := client.ToFeedItem(context.Background(), "1", status)
	if item.Summary != "the full text\nsecond line" {
		t.Errorf("summary %q", item.Summary)
	}
	if item.Title != "Target"+postTitleSuffix || item.Link != "https://weibo.com/1/abc" || item.EventType != eventPost {
		t.Errorf("unexpected item %+v", item)
	}
	if len(item.Media) != 1 || !contains(item.Tags, tagPhoto) {
		t.Errorf("media %+v tags %v", item.Media, item.Tags)
	}
	client.ToFeedItem(context.Background(), "1", status)
	if fake.count("/statuses/extend") != 1 {
		t.Errorf("the full text must be cached, requests: %d", fake.count("/statuses/extend"))
	}
	if again := client.ToFeedItem(context.Background(), "1", status); again.ItemKey != item.ItemKey {
		t.Error("the item key must be stable")
	}
}

func TestToFeedItemKeepsTimelineTextWhenExpansionFails(t *testing.T) {
	fake := newFakeWeibo(t)
	fake.handlers["/statuses/extend"] = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }
	status := parseObj(t, `{"idstr":"100","isLongText":true,"text":"short text"}`)
	if item := fake.client().ToFeedItem(context.Background(), "1", status); item.Summary != "short text" {
		t.Errorf("got %q", item.Summary)
	}
}

func TestToFeedItemRepostVideoAndEmptyBody(t *testing.T) {
	client := newFakeWeibo(t).client()
	repost := parseObj(t, `{"idstr":"1","text":"my words","retweeted_status":{"text":"original <b>text</b>","user":{"screen_name":"Author"},
		"pics":[{"url":"https://img.example/o.jpg"}]},"pics":[{"url":"https://img.example/m.jpg"}]}`)
	item := client.ToFeedItem(context.Background(), "9", repost)
	if item.Summary != "my words\n\n"+repostHeading+"Author：\noriginal text" {
		t.Errorf("summary %q", item.Summary)
	}
	if len(item.Media) != 2 || !contains(item.Tags, tagPost) {
		t.Errorf("repost media %+v", item.Media)
	}

	video := parseObj(t, `{"idstr":"2","text":"clip","page_info":{"type":"video","page_url":"https://video.weibo.com/show?fid=1","media_info":{"mp4_hd_url":"https://video.example/a.mp4"}}}`)
	item = client.ToFeedItem(context.Background(), "9", video)
	if !strings.HasSuffix(item.Summary, labelVideoPage+"https://video.weibo.com/show?fid=1") || len(item.Media) != 1 || item.Media[0].Kind != mediaVideo {
		t.Errorf("video item %+v", item)
	}

	topic := parseObj(t, `{"idstr":"3","text":"plain","page_info":{"type":"topic","page_url":"https://m.weibo.cn/p/index?containerid=100808abc"}}`)
	if item = client.ToFeedItem(context.Background(), "9", topic); strings.Contains(item.Summary, labelVideoPage) {
		t.Errorf("a topic page must not be presented as a video: %q", item.Summary)
	}

	empty := parseObj(t, `{"idstr":"4","text":""}`)
	if item = client.ToFeedItem(context.Background(), "9", empty); item.Summary != emptyPostBody {
		t.Errorf("got %q", item.Summary)
	}
	if item.Link != "https://m.weibo.cn/detail/4" {
		t.Errorf("without a bid the detail link is used, got %q", item.Link)
	}
}

const commentsFixtureTop = `{"ok":1,"data":{"data":[
	{"idstr":"301","text":"target own comment","user":{"idstr":"12345","screen_name":"Target"},"comments":[],"total_number":0},
	{"idstr":"302","text":"someone else's comment","user":{"idstr":"99999","screen_name":"OtherUser"},
		"comments":[{"idstr":"303","text":"target reply","user":{"idstr":"12345","screen_name":"Target"}}],"total_number":3}]}}`

func TestFetchCommentEventsOnlyTargetOnOwnPosts(t *testing.T) {
	fake := newFakeWeibo(t)
	fake.handlers["/api/comments/show"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") == "100" {
			w.Write([]byte(commentsFixtureTop))
			return
		}
		w.Write([]byte(`{"ok":1,"data":{"data":[]}}`))
	}
	fake.json("/comments/hotflow", `{"ok":0}`)
	fake.json("/comments/hotFlowChild", `{"ok":1,"data":{"data":[
		{"idstr":"303","text":"target reply","user":{"idstr":"12345"}},
		{"idstr":"304","text":"second target reply","user":{"idstr":"12345","screen_name":"Target"}},
		{"idstr":"305","text":"fan reply","user":{"idstr":"555","screen_name":"Fan"}}]}}`)
	statuses := []obj{
		parseObj(t, `{"idstr":"100","bid":"postbid","text":"target post","user":{"idstr":"12345","screen_name":"Target"}}`),
		parseObj(t, `{"idstr":"200","text":"other post","user":{"idstr":"98765","screen_name":"Other"}}`),
		parseObj(t, `{"idstr":"300","text":"unknown author","user":{"screen_name":"Target"}}`),
	}
	events, err := fake.client().FetchCommentEvents(context.Background(), "12345", statuses)
	if err != nil {
		t.Fatal(err)
	}
	if fake.count("/api/comments/show?id=100") != 1 || fake.count("/api/comments/show?id=200") != 0 || fake.count("/api/comments/show?id=300") != 0 {
		t.Errorf("only the target's own posts are inspected: %v", fake.requests)
	}
	if len(events) != 3 {
		t.Fatalf("expected the own comment and two replies, got %d: %+v", len(events), events)
	}
	var joined string
	for _, event := range events {
		joined += event.Summary + "|"
		if event.ParentLink != "https://weibo.com/12345/postbid" {
			t.Errorf("parent link %q", event.ParentLink)
		}
	}
	for _, want := range []string{"target own comment", "target reply", "second target reply"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, unwanted := range []string{"someone else's comment", "fan reply"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("must not include %q", unwanted)
		}
	}
	if fake.count("/comments/hotFlowChild") != 1 {
		t.Error("the thread with more replies than inlined must be expanded once")
	}
}

func TestFetchCommentEventsRespectsThreadLimitAndAuthErrors(t *testing.T) {
	fake := newFakeWeibo(t)
	fake.json("/api/comments/show", `{"ok":1,"data":{"data":[
		{"idstr":"1","user":{"idstr":"9"},"total_number":5},{"idstr":"2","user":{"idstr":"9"},"total_number":5}]}}`)
	fake.json("/comments/hotFlowChild", `{"ok":1,"data":{"data":[]}}`)
	client := fake.client()
	client.replyThreadsPerPoll = 1
	status := parseObj(t, `{"idstr":"100","user":{"idstr":"12345","screen_name":"Target"}}`)
	if _, err := client.FetchCommentEvents(context.Background(), "12345", []obj{status}); err != nil {
		t.Fatal(err)
	}
	if fake.count("/comments/hotFlowChild") != 1 {
		t.Errorf("at most one reply thread per poll, got %d", fake.count("/comments/hotFlowChild"))
	}

	denied := fake.client()
	denied.cookies = "SUB=wrong"
	if _, err := denied.FetchCommentEvents(context.Background(), "12345", []obj{status}); !isAuthError(err) {
		t.Errorf("an auth failure must abort the scan, got %v", err)
	}
}

func TestFetchCommentEventsFallsBackToHotflow(t *testing.T) {
	fake := newFakeWeibo(t)
	fake.json("/api/comments/show", `{"ok":0}`)
	fake.json("/comments/hotflow", `{"ok":1,"data":{"data":[{"idstr":"7","text":"via hotflow","user":{"idstr":"12345","screen_name":"Target"}}]}}`)
	status := parseObj(t, `{"idstr":"100","user":{"idstr":"12345","screen_name":"Target"}}`)
	events, err := fake.client().FetchCommentEvents(context.Background(), "12345", []obj{status})
	if err != nil || len(events) != 1 || !strings.Contains(events[0].Summary, "via hotflow") {
		t.Fatalf("got %+v %v", events, err)
	}
}
