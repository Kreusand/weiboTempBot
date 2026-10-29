package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func parseObj(t *testing.T, raw string) obj {
	t.Helper()
	value, err := decodeJSON([]byte(raw))
	if err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	return asObj(value)
}

func TestCleanHTML(t *testing.T) {
	cases := map[string]string{
		"<span>你好</span><br><span>世界</span>":                           "你好\n世界",
		"你好<a href=\"/n/x\">@某人</a>，再见":                                "你好@某人，再见",
		"a &amp; b &lt;c&gt; &quot;d&quot;":                            "a & b <c> \"d\"",
		"x<script>alert('<b>')</script>y<style>p{}</style>z":           "xyz",
		"<video src=a>fallback</video>text<iframe></iframe>":           "text",
		"line1<br />line2<br/><br/><br/>line3":                         "line1\nline2\n\nline3",
		"  spaced   out \t text  ":                                     "spaced out text",
		"<span class=\"url-icon\"><img alt=\"[doge]\" src=x></span>ok": "ok",
		"":                   "",
		"broken <b unclosed": "broken <b unclosed",
	}
	for input, want := range cases {
		if got := cleanHTML(input); got != want {
			t.Errorf("cleanHTML(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCollectPhotos(t *testing.T) {
	status := parseObj(t, `{"pics":[{"large":{"url":"https://img.example/a.jpg"}},{"url":"//img.example/b.jpg"},{"large":{"url":"https://img.example/a.jpg"}},{"url":"ftp://x/c.jpg"}],
		"pic_infos":{"x":{"largest":{"url":"https://img.example/d.jpg"}},"y":{"original":{"url":"https://img.example/a.jpg"}}}}`)
	media := collectPhotos(status)
	var urls []string
	for _, item := range media {
		if item.Kind != mediaPhoto {
			t.Fatalf("unexpected kind %s", item.Kind)
		}
		urls = append(urls, item.URL)
	}
	want := "https://img.example/a.jpg,https://img.example/b.jpg,https://img.example/d.jpg"
	if strings.Join(urls, ",") != want {
		t.Fatalf("got %v", urls)
	}
}

func TestCollectVideoOnlyDirectFiles(t *testing.T) {
	direct := parseObj(t, `{"page_info":{"media_info":{"mp4_hd_url":"https://video.example/a.mp4"},"page_url":"https://weibo.com/tv/show/123"}}`)
	if got := collectVideo(direct); len(got) != 1 || got[0].URL != "https://video.example/a.mp4" || got[0].Kind != mediaVideo {
		t.Fatalf("got %+v", got)
	}
	pageOnly := parseObj(t, `{"page_info":{"page_url":"https://weibo.com/tv/show/123"}}`)
	if got := collectVideo(pageOnly); len(got) != 0 {
		t.Fatalf("a page link is not a video file: %+v", got)
	}
	playlist := parseObj(t, `{"page_info":{"media_info":{"stream_url":"https://video.example/live.m3u8"}}}`)
	if got := collectVideo(playlist); len(got) != 0 {
		t.Fatalf("a playlist is not a video file: %+v", got)
	}
	signed := parseObj(t, `{"page_info":{"media_info":{"stream_url":"https://video.example/v?label=mp4_720p&token=1"}}}`)
	if got := collectVideo(signed); len(got) != 1 {
		t.Fatalf("an mp4 label in the query counts: %+v", got)
	}
}

func TestStatusTags(t *testing.T) {
	tags := statusTags(parseObj(t, `{"visible":{"type":1},"retweeted_status":{"id":"1"},"pic_num":1,"page_info":{"type":"video"}}`), nil)
	for _, want := range []string{tagPost, tagFansOnly, tagRepost, tagVideo, tagPhoto} {
		if !contains(tags, want) {
			t.Errorf("missing tag %s in %v", want, tags)
		}
	}
	plain := statusTags(parseObj(t, `{"visible":{"type":0,"list_id":0},"pic_num":0}`), nil)
	if len(plain) != 1 || plain[0] != tagPost {
		t.Errorf("a public text post only has the post tag, got %v", plain)
	}
	if !contains(statusTags(parseObj(t, `{"is_fans_only":true}`), nil), tagFansOnly) {
		t.Error("is_fans_only must mark the post")
	}
	withMedia := statusTags(obj{}, []MediaItem{{URL: "u", Kind: mediaPhoto}, {URL: "v", Kind: mediaVideo}})
	if !contains(withMedia, tagPhoto) || !contains(withMedia, tagVideo) {
		t.Errorf("media kinds must produce tags, got %v", withMedia)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestVideoPageLink(t *testing.T) {
	topic := parseObj(t, `{"page_info":{"type":"topic","page_url":"https://m.weibo.cn/p/index?containerid=100808abc"}}`)
	if got := videoPageLink(topic, nil); got != "" {
		t.Errorf("a topic page is not a video page: %q", got)
	}
	video := parseObj(t, `{"page_info":{"type":"video","page_url":"https://video.weibo.com/show?fid=1"}}`)
	if got := videoPageLink(video, nil); got != "https://video.weibo.com/show?fid=1" {
		t.Errorf("got %q", got)
	}
	other := parseObj(t, `{"page_info":{"type":"article","page_url":"https://weibo.com/ttarticle/1"}}`)
	if got := videoPageLink(other, []MediaItem{{URL: "v", Kind: mediaVideo}}); got == "" {
		t.Error("a post with a real video file keeps its page link")
	}
}

func TestIsPinnedSignals(t *testing.T) {
	pinned := []string{`{"isTop":1}`, `{"isTop":"1"}`, `{"is_top":1}`, `{"isPinnedCard":true}`, `{"title":{"text":"` + pinnedLabel + `"}}`}
	for _, raw := range pinned {
		if !isPinned(parseObj(t, raw)) {
			t.Errorf("%s must count as pinned", raw)
		}
	}
	for _, raw := range []string{`{"isTop":0,"title":{"text":"other"}}`, `{}`, `{"isTop":false}`} {
		if isPinned(parseObj(t, raw)) {
			t.Errorf("%s must not count as pinned", raw)
		}
	}
}

func TestSortNewestFirstMovesPinnedPostDown(t *testing.T) {
	sorted := sortNewestFirst([]obj{{"idstr": "100"}, {"idstr": "300"}, {"idstr": "200"}})
	got := []string{asStr(sorted[0]["idstr"]), asStr(sorted[1]["idstr"]), asStr(sorted[2]["idstr"])}
	if strings.Join(got, ",") != "300,200,100" {
		t.Fatalf("got %v", got)
	}
}

func TestLargeIDsKeepEveryDigit(t *testing.T) {
	status := parseObj(t, `{"id":5359216613130705,"mid":"5359216613130705"}`)
	if got := statusIDOf(status); got != "5359216613130705" {
		t.Fatalf("id lost precision: %s", got)
	}
}

func TestCommentListShapes(t *testing.T) {
	nested := parseObj(t, `{"data":{"data":[{"id":"11"},{"id":"12"}]}}`)
	if len(extractCommentList(nested)) != 2 {
		t.Error("nested data shape")
	}
	flat := parseObj(t, `{"data":[{"id":"11"},"junk",{"id":"12"}]}`)
	if len(extractCommentList(flat)) != 2 {
		t.Error("flat list shape must skip non-objects")
	}
	deep := parseObj(t, `{"data":{"comments":{"data":[{"id":"1"}]}}}`)
	if len(extractCommentList(deep)) != 1 {
		t.Error("comments wrapper shape")
	}
	if len(extractCommentList(parseObj(t, `{"data":"nothing"}`))) != 0 {
		t.Error("unknown shape must be empty")
	}
	if len(nestedReplies(parseObj(t, `{"comment":[{"id":"22"},{"id":"23"}]}`))) != 2 {
		t.Error("nested replies under 'comment'")
	}
	if len(nestedReplies(parseObj(t, `{"comments":[{"id":"22"}]}`))) != 1 {
		t.Error("nested replies under 'comments'")
	}
}

func TestCommentItemFormat(t *testing.T) {
	status := parseObj(t, `{"idstr":"100","bid":"postbid"}`)
	comment := parseObj(t, `{"idstr":"301","text":"<b>hello</b>","user":{"idstr":"1","screen_name":"Target"},"created_at":"Thu Oct 29 03:32:26 +0800 2026"}`)
	parent := parseObj(t, `{"user":{"screen_name":"Fan"}}`)
	item, ok := commentItem("1", status, comment, "post text", "https://weibo.com/1/postbid", true, parent)
	if !ok {
		t.Fatal("comment with an id must produce an item")
	}
	if item.EventType != eventCommentReply || item.Tags[0] != tagCommentRepl || item.ParentLink != "https://weibo.com/1/postbid" {
		t.Errorf("unexpected item %+v", item)
	}
	if !strings.Contains(item.Summary, "Target：hello") || !strings.Contains(item.Summary, replyToPrefix+"Fan") || !strings.Contains(item.Summary, parentPostPrefix+"post text") {
		t.Errorf("summary %q", item.Summary)
	}
	if item.Link != "https://m.weibo.cn/detail/100?cid=301" || item.Published != "Thu Oct 29 03:32:26 +0800 2026" {
		t.Errorf("link %q published %q", item.Link, item.Published)
	}
	if _, ok := commentItem("1", status, obj{}, "", "", false, nil); ok {
		t.Error("a comment without an id has no item")
	}
	empty, _ := commentItem("1", status, parseObj(t, `{"idstr":"5"}`), "", "", false, nil)
	if !strings.Contains(empty.Summary, emptyCommentBody) {
		t.Errorf("empty comments get a placeholder, got %q", empty.Summary)
	}
}

func TestDateString(t *testing.T) {
	if got := dateString(json.Number("0")); got != "0" {
		t.Errorf("got %q", got)
	}
	if got := dateString(nil); got != "" {
		t.Errorf("got %q", got)
	}
	if got := dateString(json.Number("1793000000")); !strings.HasPrefix(got, "2026-") {
		t.Errorf("a unix timestamp becomes a local ISO time, got %q", got)
	}
}
