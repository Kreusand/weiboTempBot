package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeMedia serves files for the sender to download.
type fakeMedia struct {
	server  *httptest.Server
	cookies []string
	referer []string
}

func newFakeMedia(t *testing.T) *fakeMedia {
	t.Helper()
	fake := &fakeMedia{}
	mux := http.NewServeMux()
	serve := func(w http.ResponseWriter, r *http.Request, contentType string, body string) {
		fake.cookies = append(fake.cookies, r.Header.Get("Cookie"))
		fake.referer = append(fake.referer, r.Header.Get("Referer"))
		w.Header().Set("Content-Type", contentType)
		w.Write([]byte(body))
	}
	mux.HandleFunc("/photo/", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, "image/jpeg", "jpeg:"+strings.TrimPrefix(r.URL.Path, "/photo/"))
	})
	mux.HandleFunc("/video.mp4", func(w http.ResponseWriter, r *http.Request) { serve(w, r, "video/mp4", "mp4-bytes") })
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) { serve(w, r, "application/vnd.apple.mpegurl", "#EXTM3U") })
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) { serve(w, r, "text/html", "<html>blocked</html>") })
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", "image/jpeg") })
	mux.HandleFunc("/huge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", strconv.Itoa(photoSizeLimit+1))
		w.Write([]byte("x"))
	})
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/photo/after-redirect", http.StatusFound)
	})
	mux.HandleFunc("/leave", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example.com/steal", http.StatusFound)
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeMedia) url(path string) string { return f.server.URL + path }

func newTestSender(telegram *fakeTelegram) *Sender {
	sender := NewSender(telegram.client(), &Settings{MaxMediaBytes: 45_000_000, MaxMediaPerPost: 10, WeiboCookies: "SUB=secret"})
	sender.mediaHosts = []string{"127.0.0.1"}
	sender.cookieHosts = []string{"127.0.0.1"}
	return sender
}

func photoItem(urls ...string) FeedItem {
	item := FeedItem{ItemKey: "k", Title: "Target" + postTitleSuffix, Summary: "body text", Link: "https://weibo.com/1/abc", Tags: []string{tagPost}, EventType: eventPost}
	for _, mediaURL := range urls {
		item.Media = append(item.Media, MediaItem{URL: mediaURL, Kind: mediaPhoto})
	}
	return item
}

func TestMessageTextFormat(t *testing.T) {
	item := FeedItem{
		Title: "Target" + postTitleSuffix, Summary: "body", Link: "https://weibo.com/1/abc", Published: "today",
		Tags: []string{tagPost, tagPhoto}, EventType: eventPost,
	}
	want := "【" + typeNamePost + "】Name\n\n" + tagPost + " " + tagPhoto + "\n\nTarget" + postTitleSuffix + "\n\nbody\n\n" + labelTime + "today\n\n" + labelOrigin + "https://weibo.com/1/abc"
	if got := messageText("Name", item); got != want {
		t.Errorf("got %q want %q", got, want)
	}
	item.Title = "in body"
	item.Summary = "text in body here"
	item.ParentLink = "https://weibo.com/1/parent"
	item.EventType = eventCommentReply
	got := messageText("Name", item)
	if strings.Contains(got, "\n\nin body\n\n") {
		t.Error("a title already contained in the summary is not repeated")
	}
	if !strings.HasPrefix(got, "【"+typeNameReply+"】") || !strings.Contains(got, labelParentLink+"https://weibo.com/1/parent") {
		t.Errorf("got %q", got)
	}
	if got := messageText("Name", FeedItem{EventType: "other"}); got != "【"+typeNameOther+"】Name" {
		t.Errorf("got %q", got)
	}
}

func TestSplitMessage(t *testing.T) {
	if got := splitMessage("  short  "); len(got) != 1 || got[0] != "short" {
		t.Errorf("got %q", got)
	}
	if len(splitMessage("   ")) != 0 {
		t.Error("blank text produces nothing")
	}
	line := strings.Repeat("字", 1000)
	text := line + "\n" + line + "\n" + line + "\n" + line + "\n" + line
	chunks := splitMessage(text)
	if len(chunks) < 2 {
		t.Fatalf("expected a split, got %d chunks", len(chunks))
	}
	for _, chunk := range chunks {
		if len([]rune(chunk)) > messageChunkRunes || chunk == "" {
			t.Errorf("bad chunk of %d characters", len([]rune(chunk)))
		}
		if strings.HasPrefix(chunk, "\n") || strings.HasSuffix(chunk, "\n") {
			t.Error("chunks are trimmed")
		}
	}
	if strings.ReplaceAll(strings.Join(chunks, ""), "\n", "") != strings.ReplaceAll(text, "\n", "") {
		t.Error("no text may be lost")
	}
	if !strings.HasSuffix(chunks[0], "字") || len([]rune(chunks[0])) != 3002 {
		t.Errorf("the first chunk must end at a line boundary, has %d characters", len([]rune(chunks[0])))
	}
	hard := splitMessage(strings.Repeat("a", 9000))
	if len(hard) != 3 || len([]rune(hard[0])) != messageChunkRunes {
		t.Errorf("without line breaks the cut is hard, got %d chunks", len(hard))
	}
}

func TestSendTextOnlyItem(t *testing.T) {
	telegram := newFakeTelegram(t)
	sender := newTestSender(telegram)
	if err := sender.Send(context.Background(), "-100", "Name", photoItem(), false); err != nil {
		t.Fatal(err)
	}
	if methods := telegram.methods(); len(methods) != 1 || methods[0] != "sendMessage" {
		t.Fatalf("methods %v", methods)
	}
	if !strings.Contains(telegram.last().fields["text"], "body text") {
		t.Error("text missing")
	}
}

func TestSendSinglePhotoUploadsTheBytes(t *testing.T) {
	telegram := newFakeTelegram(t)
	media := newFakeMedia(t)
	sender := newTestSender(telegram)
	if err := sender.Send(context.Background(), "-100", "Name", photoItem(media.url("/photo/one")), false); err != nil {
		t.Fatal(err)
	}
	call := telegram.last()
	if call.method != "sendPhoto" || string(call.files["photo"]) != "jpeg:one" || !strings.Contains(call.fields["caption"], "body text") {
		t.Fatalf("unexpected call %+v", call)
	}
	if media.cookies[0] != "SUB=secret" || media.referer[0] != mobileBase+"/" {
		t.Errorf("cookie %q referer %q", media.cookies[0], media.referer[0])
	}
}

func TestSendAlbumAttachesEveryPhoto(t *testing.T) {
	telegram := newFakeTelegram(t)
	media := newFakeMedia(t)
	sender := newTestSender(telegram)
	item := photoItem(media.url("/photo/a"), media.url("/photo/b"), media.url("/photo/c"))
	if err := sender.Send(context.Background(), "-100", "Name", item, false); err != nil {
		t.Fatal(err)
	}
	call := telegram.last()
	if call.method != "sendMediaGroup" || len(call.files) != 3 {
		t.Fatalf("unexpected call %+v", call)
	}
	for _, name := range []string{"file0", "file1", "file2"} {
		if !strings.Contains(call.fields["media"], "attach://"+name) || len(call.files[name]) == 0 {
			t.Errorf("%s is not attached", name)
		}
	}
	if strings.Count(call.fields["media"], `"caption"`) != 1 {
		t.Error("only the first member carries the caption")
	}
}

func TestSendMoreThanTenFilesSplitsAlbums(t *testing.T) {
	telegram := newFakeTelegram(t)
	media := newFakeMedia(t)
	sender := newTestSender(telegram)
	sender.maxMediaPerPost = 12
	var urls []string
	for index := 0; index < 11; index++ {
		urls = append(urls, media.url("/photo/"+strconv.Itoa(index)))
	}
	if err := sender.Send(context.Background(), "-100", "Name", photoItem(urls...), false); err != nil {
		t.Fatal(err)
	}
	methods := telegram.methods()
	if len(methods) != 2 || methods[0] != "sendMediaGroup" || methods[1] != "sendPhoto" {
		t.Fatalf("methods %v", methods)
	}
	if _, hasCaption := telegram.last().fields["caption"]; hasCaption {
		t.Error("the caption belongs to the first album only")
	}
}

func TestLongTextContinuesAfterTheCaption(t *testing.T) {
	telegram := newFakeTelegram(t)
	media := newFakeMedia(t)
	sender := newTestSender(telegram)
	item := photoItem(media.url("/photo/one"))
	item.Summary = strings.Repeat("字", 1500)
	if err := sender.Send(context.Background(), "-100", "Name", item, false); err != nil {
		t.Fatal(err)
	}
	methods := telegram.methods()
	if len(methods) != 2 || methods[0] != "sendPhoto" || methods[1] != "sendMessage" {
		t.Fatalf("methods %v", methods)
	}
	if len([]rune(telegram.calls[0].fields["caption"])) != captionRunes {
		t.Errorf("caption has %d characters", len([]rune(telegram.calls[0].fields["caption"])))
	}
	if !strings.Contains(telegram.last().fields["text"], labelOrigin) {
		t.Error("the rest of the text, with the origin link, follows the media")
	}
}

func TestVideoIsUploadedAsVideo(t *testing.T) {
	telegram := newFakeTelegram(t)
	media := newFakeMedia(t)
	sender := newTestSender(telegram)
	item := photoItem()
	item.Media = []MediaItem{{URL: media.url("/video.mp4"), Kind: mediaVideo}}
	if err := sender.Send(context.Background(), "-100", "Name", item, false); err != nil {
		t.Fatal(err)
	}
	if call := telegram.last(); call.method != "sendVideo" || string(call.files["video"]) != "mp4-bytes" {
		t.Fatalf("unexpected call %+v", call)
	}
}

func TestDownloadFailureNoteIsForTestDestinationsOnly(t *testing.T) {
	media := newFakeMedia(t)
	for _, diagnostics := range []bool{false, true} {
		telegram := newFakeTelegram(t)
		sender := newTestSender(telegram)
		item := photoItem(media.url("/missing"))
		if err := sender.Send(context.Background(), "-100", "Name", item, diagnostics); err != nil {
			t.Fatal(err)
		}
		text := telegram.last().fields["text"]
		if !strings.Contains(text, "body text") {
			t.Fatal("the post text is always sent")
		}
		if got := strings.Contains(text, "/missing"); got != diagnostics {
			t.Errorf("diagnostics=%v: media link in text = %v", diagnostics, got)
		}
	}
}

func TestPartialFailureNoteIsForTestDestinationsOnly(t *testing.T) {
	media := newFakeMedia(t)
	for _, diagnostics := range []bool{false, true} {
		telegram := newFakeTelegram(t)
		sender := newTestSender(telegram)
		item := photoItem(media.url("/photo/ok"), media.url("/missing"))
		if err := sender.Send(context.Background(), "-100", "Name", item, diagnostics); err != nil {
			t.Fatal(err)
		}
		methods := telegram.methods()
		if diagnostics && (len(methods) != 2 || methods[0] != "sendPhoto" || methods[1] != "sendMessage") {
			t.Errorf("a test destination gets the photo and a note, got %v", methods)
		}
		if !diagnostics && (len(methods) != 1 || methods[0] != "sendPhoto") {
			t.Errorf("a public destination gets the photo only, got %v", methods)
		}
	}
}

func TestUploadFailureFallsBackToText(t *testing.T) {
	media := newFakeMedia(t)
	for _, diagnostics := range []bool{false, true} {
		telegram := newFakeTelegram(t)
		telegram.reply = func(call recordedCall, _ int) (int, string) {
			if call.method == "sendPhoto" {
				return 400, `{"ok":false,"error_code":400,"description":"Bad Request: IMAGE_PROCESS_FAILED"}`
			}
			return 200, `{"ok":true,"result":true}`
		}
		sender := newTestSender(telegram)
		if err := sender.Send(context.Background(), "-100", "Name", photoItem(media.url("/photo/one")), diagnostics); err != nil {
			t.Fatalf("a refused upload must not fail the delivery: %v", err)
		}
		text := telegram.last().fields["text"]
		if !strings.Contains(text, "body text") {
			t.Fatal("the text must still be sent")
		}
		if got := strings.Contains(text, "IMAGE_PROCESS_FAILED"); got != diagnostics {
			t.Errorf("diagnostics=%v: error reason in text = %v", diagnostics, got)
		}
		if !diagnostics && strings.Contains(text, "/photo/one") {
			t.Error("a public chat never gets raw media links")
		}
	}
}

func TestOtherTelegramFailuresStillFailTheDelivery(t *testing.T) {
	telegram := newFakeTelegram(t)
	telegram.reply = func(recordedCall, int) (int, string) {
		return 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked"}`
	}
	if err := newTestSender(telegram).Send(context.Background(), "-100", "Name", photoItem(), false); err == nil {
		t.Fatal("a rejected text message must be reported so it is retried")
	}
}

func TestDownloadRules(t *testing.T) {
	media := newFakeMedia(t)
	sender := newTestSender(newFakeTelegram(t))
	ctx := context.Background()
	photo := func(path string) MediaItem { return MediaItem{URL: media.url(path), Kind: mediaPhoto} }

	if got, ok := sender.download(ctx, photo("/photo/x"), 1_000_000); !ok || string(got.data) != "jpeg:x" || got.suffix != ".jpg" {
		t.Errorf("a normal photo: %+v %v", got, ok)
	}
	if got, ok := sender.download(ctx, photo("/hop"), 1_000_000); !ok || string(got.data) != "jpeg:after-redirect" {
		t.Errorf("redirects inside the host list are followed: %+v %v", got, ok)
	}
	for name, item := range map[string]MediaItem{
		"error status":  photo("/missing"),
		"not an image":  photo("/html"),
		"empty body":    photo("/empty"),
		"declared huge": photo("/huge"),
		"redirect off":  photo("/leave"),
		"playlist":      {URL: media.url("/live.m3u8"), Kind: mediaVideo},
		"foreign host":  {URL: "http://evil.example.com/a.jpg", Kind: mediaPhoto},
		"bad scheme":    {URL: "ftp://127.0.0.1/a.jpg", Kind: mediaPhoto},
		"not a video":   {URL: media.url("/html"), Kind: mediaVideo},
	} {
		if _, ok := sender.download(ctx, item, 1_000_000); ok {
			t.Errorf("%s must be rejected", name)
		}
	}
	if _, ok := sender.download(ctx, photo("/photo/x"), 3); ok {
		t.Error("a file larger than the remaining budget is rejected while streaming")
	}
}

func TestCookieOnlyGoesToCookieHosts(t *testing.T) {
	media := newFakeMedia(t)
	sender := newTestSender(newFakeTelegram(t))
	sender.cookieHosts = []string{"weibo.com"}
	if _, ok := sender.download(context.Background(), MediaItem{URL: media.url("/photo/x"), Kind: mediaPhoto}, 1_000_000); !ok {
		t.Fatal("download failed")
	}
	if media.cookies[len(media.cookies)-1] != "" || media.referer[len(media.referer)-1] != "" {
		t.Error("the Weibo cookie must not be sent to a host outside the cookie list")
	}
}

func TestSuffixFallsBackToTheURL(t *testing.T) {
	sender := newTestSender(newFakeTelegram(t))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("data"))
	}))
	defer server.Close()
	got, ok := sender.download(context.Background(), MediaItem{URL: server.URL + "/clip.mov", Kind: mediaVideo}, 1_000_000)
	if !ok || got.suffix != ".mov" {
		t.Errorf("got %+v %v", got, ok)
	}
	got, ok = sender.download(context.Background(), MediaItem{URL: server.URL + "/noext", Kind: mediaVideo}, 1_000_000)
	if ok && got.suffix != ".bin" {
		t.Errorf("got %+v", got)
	}
}
