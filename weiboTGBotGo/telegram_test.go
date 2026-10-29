package main

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "123456:SECRET-TOKEN"

// recordedCall is one request the fake Telegram server received.
type recordedCall struct {
	method string
	fields map[string]string
	files  map[string][]byte
}

type fakeTelegram struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []recordedCall
	// reply decides the answer for a call; the default is a plain success.
	reply func(call recordedCall, attempt int) (int, string)
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	fake := &fakeTelegram{}
	attempts := map[string]int{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+testToken+"/") {
			http.NotFound(w, r)
			return
		}
		call := recordedCall{method: strings.TrimPrefix(r.URL.Path, "/bot"+testToken+"/"), fields: map[string]string{}, files: map[string][]byte{}}
		mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if strings.HasPrefix(mediaType, "multipart/") {
			reader := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := reader.NextPart()
				if err != nil {
					break
				}
				content, _ := io.ReadAll(part)
				if part.FileName() != "" {
					call.files[part.FormName()] = content
				} else {
					call.fields[part.FormName()] = string(content)
				}
			}
		} else {
			var raw map[string]any
			json.NewDecoder(r.Body).Decode(&raw)
			for key, value := range raw {
				if text, ok := value.(string); ok {
					call.fields[key] = text
				}
			}
		}
		fake.mu.Lock()
		fake.calls = append(fake.calls, call)
		attempts[call.method]++
		attempt := attempts[call.method]
		reply := fake.reply
		fake.mu.Unlock()
		status, body := http.StatusOK, `{"ok":true,"result":true}`
		if reply != nil {
			status, body = reply(call, attempt)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeTelegram) client() *TelegramClient {
	client := NewTelegramClient(testToken)
	client.baseURL = f.server.URL
	client.sleep = func(context.Context, time.Duration) error { return nil }
	return client
}

func (f *fakeTelegram) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []string
	for _, call := range f.calls {
		result = append(result, call.method)
	}
	return result
}

func (f *fakeTelegram) last() recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func TestSendMessageAndDisablePreview(t *testing.T) {
	fake := newFakeTelegram(t)
	if err := fake.client().SendMessage(context.Background(), "-100", "hello", true); err != nil {
		t.Fatal(err)
	}
	call := fake.last()
	if call.method != "sendMessage" || call.fields["chat_id"] != "-100" || call.fields["text"] != "hello" || call.fields["disable_web_page_preview"] != "true" {
		t.Fatalf("unexpected call %+v", call)
	}
}

func TestSingleUploadIsNotEmpty(t *testing.T) {
	fake := newFakeTelegram(t)
	client := fake.client()
	if err := client.SendSingle(context.Background(), "-100", mediaPhoto, "caption", uploadFile{"a.jpg", []byte("image-bytes")}); err != nil {
		t.Fatal(err)
	}
	call := fake.last()
	if call.method != "sendPhoto" || string(call.files["photo"]) != "image-bytes" || call.fields["caption"] != "caption" {
		t.Fatalf("unexpected call %+v", call)
	}
	if err := client.SendSingle(context.Background(), "-100", mediaVideo, "", uploadFile{"a.mp4", []byte("video-bytes")}); err != nil {
		t.Fatal(err)
	}
	call = fake.last()
	if call.method != "sendVideo" || string(call.files["video"]) != "video-bytes" || call.fields["supports_streaming"] != "true" {
		t.Fatalf("unexpected call %+v", call)
	}
	if _, hasCaption := call.fields["caption"]; hasCaption {
		t.Error("an empty caption must not be sent")
	}
}

func TestAlbumMembersAreAttached(t *testing.T) {
	fake := newFakeTelegram(t)
	members := []groupMember{
		{kind: mediaPhoto, caption: "first caption", file: uploadFile{"a.jpg", []byte("one")}},
		{kind: mediaPhoto, file: uploadFile{"b.jpg", []byte("two")}},
		{kind: mediaVideo, file: uploadFile{"c.mp4", []byte("three")}},
	}
	if err := fake.client().SendAlbum(context.Background(), "-100", members); err != nil {
		t.Fatal(err)
	}
	call := fake.last()
	var entries []struct{ Type, Media, Caption string }
	if err := json.Unmarshal([]byte(call.fields["media"]), &entries); err != nil {
		t.Fatalf("the media description must be JSON: %v (%s)", err, call.fields["media"])
	}
	if len(entries) != 3 || len(call.files) != 3 {
		t.Fatalf("%d entries, %d files", len(entries), len(call.files))
	}
	want := []string{"one", "two", "three"}
	for index, entry := range entries {
		name := strings.TrimPrefix(entry.Media, "attach://")
		if name == entry.Media || string(call.files[name]) != want[index] {
			t.Errorf("entry %d is not attached to its file: %+v", index, entry)
		}
	}
	if entries[0].Caption != "first caption" || entries[1].Caption != "" || entries[2].Type != mediaVideo {
		t.Errorf("unexpected entries %+v", entries)
	}
}

func TestRateLimitIsRetriedAfterTheRequestedWait(t *testing.T) {
	fake := newFakeTelegram(t)
	fake.reply = func(call recordedCall, attempt int) (int, string) {
		if attempt < 3 {
			return 429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":7}}`
		}
		return 200, `{"ok":true,"result":true}`
	}
	client := fake.client()
	var waits []time.Duration
	client.sleep = func(_ context.Context, wait time.Duration) error { waits = append(waits, wait); return nil }
	if err := client.SendMessage(context.Background(), "-100", "x", false); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 || waits[0] != 7*time.Second {
		t.Fatalf("waits %v", waits)
	}

	fake.reply = func(recordedCall, int) (int, string) {
		return 429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":3000}}`
	}
	err := client.SendMessage(context.Background(), "-100", "x", false)
	if apiErr, ok := err.(*TelegramError); !ok || apiErr.Code != 429 {
		t.Fatalf("a very long wait must be reported, not slept: %v", err)
	}
}

func TestAPIErrorsCarryTheDescription(t *testing.T) {
	fake := newFakeTelegram(t)
	fake.reply = func(recordedCall, int) (int, string) {
		return 400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`
	}
	_, err := fake.client().GetChat(context.Background(), "-100")
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("got %v", err)
	}
}

func TestGetUpdatesAndGetMe(t *testing.T) {
	fake := newFakeTelegram(t)
	fake.reply = func(call recordedCall, _ int) (int, string) {
		if call.method == "getMe" {
			return 200, `{"ok":true,"result":{"id":1,"username":"testbot"}}`
		}
		return 200, `{"ok":true,"result":[{"update_id":5,"message":{"message_id":1,"from":{"id":42},"chat":{"id":-100,"type":"supergroup","title":"G"},"text":"/help"}}]}`
	}
	client := fake.client()
	me, err := client.GetMe(context.Background())
	if err != nil || me.Username != "testbot" {
		t.Fatalf("got %+v %v", me, err)
	}
	updates, err := client.GetUpdates(context.Background(), 5, 1)
	if err != nil || len(updates) != 1 || updates[0].Message.Text != "/help" || updates[0].Message.From.ID != 42 || updates[0].Message.Chat.ID != -100 {
		t.Fatalf("got %+v %v", updates, err)
	}
	if call := fake.last(); call.fields["offset"] != "5" {
		t.Errorf("offset not sent: %+v", call.fields)
	}
}

func TestTokenNeverAppearsInErrors(t *testing.T) {
	fake := newFakeTelegram(t)
	client := fake.client()
	fake.server.Close()
	err := client.SendMessage(context.Background(), "-100", "x", false)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("the token leaked into the error: %v", err)
	}
}
