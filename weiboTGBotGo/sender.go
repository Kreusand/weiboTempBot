package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	messageChunkRunes = 3900
	captionRunes      = 900
	albumSize         = 10
	photoSizeLimit    = 9_000_000
	downloadTimeout   = 35 * time.Second
)

// Hosts media may be downloaded from, and the subset that gets the Weibo cookie and referer.
var (
	mediaHosts       = []string{"weibo.cn", "weibo.com", "weibo.com.cn", "sinaimg.cn", "weibocdn.com", "sina.com.cn", "sina.com", "sina.cn"}
	mediaCookieHosts = []string{"weibo.cn", "weibo.com", "sinaimg.cn", "weibocdn.com", "sina.com.cn", "sina.com", "sina.cn"}
)

// Sender turns feed items into Telegram messages.
type Sender struct {
	telegram        *TelegramClient
	http            *http.Client
	maxMediaBytes   int
	maxMediaPerPost int
	weiboCookies    string
	mediaHosts      []string
	cookieHosts     []string
}

func NewSender(telegram *TelegramClient, settings *Settings) *Sender {
	return &Sender{
		telegram: telegram,
		http: &http.Client{
			Timeout:       downloadTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		maxMediaBytes:   settings.MaxMediaBytes,
		maxMediaPerPost: settings.MaxMediaPerPost,
		weiboCookies:    settings.WeiboCookies,
		mediaHosts:      mediaHosts,
		cookieHosts:     mediaCookieHosts,
	}
}

// messageText renders the text of an item.
func messageText(accountName string, item FeedItem) string {
	typeName := typeNameOther
	switch item.EventType {
	case eventPost:
		typeName = typeNamePost
	case eventComment:
		typeName = typeNameComment
	case eventCommentReply:
		typeName = typeNameReply
	}
	parts := []string{"【" + typeName + "】" + accountName}
	if len(item.Tags) > 0 {
		parts = append(parts, strings.Join(item.Tags, " "))
	}
	if item.Title != "" && !strings.Contains(item.Summary, item.Title) {
		parts = append(parts, item.Title)
	}
	if item.Summary != "" {
		parts = append(parts, item.Summary)
	}
	if item.Published != "" {
		parts = append(parts, labelTime+item.Published)
	}
	if item.ParentLink != "" {
		parts = append(parts, labelParentLink+item.ParentLink)
	}
	if item.Link != "" {
		parts = append(parts, labelOrigin+item.Link)
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// splitMessage cuts a long text into chunks Telegram accepts, preferring line boundaries.
func splitMessage(text string) []string {
	remaining := []rune(strings.TrimSpace(text))
	var chunks []string
	for len(remaining) > 0 {
		cut := len(remaining)
		if cut > messageChunkRunes {
			cut = messageChunkRunes
			for index := messageChunkRunes - 1; index >= 2500; index-- {
				if remaining[index] == '\n' {
					cut = index
					break
				}
			}
		}
		chunk := strings.TrimRight(string(remaining[:cut]), " \t\r\n")
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		remaining = []rune(strings.TrimLeft(string(remaining[cut:]), " \t\r\n"))
	}
	return chunks
}

func (s *Sender) sendText(ctx context.Context, chatID, text string, disablePreview bool) error {
	for _, chunk := range splitMessage(text) {
		if err := s.telegram.SendMessage(ctx, chatID, chunk, disablePreview); err != nil {
			return err
		}
	}
	return nil
}

type downloadedMedia struct {
	media  MediaItem
	data   []byte
	suffix string
}

var contentTypeSuffixes = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif",
	"video/mp4": ".mp4", "video/quicktime": ".mov", "video/webm": ".webm", "video/x-m4v": ".m4v",
}

func minInt(values ...int) int {
	result := values[0]
	for _, value := range values[1:] {
		if value < result {
			result = value
		}
	}
	return result
}

// download fetches one media file itself, because Weibo hosts often refuse requests that come
// from Telegram's servers. It returns false when the file is unusable.
func (s *Sender) download(ctx context.Context, media MediaItem, budget int) (downloadedMedia, bool) {
	current := media.URL
	for hop := 0; hop < 6; hop++ {
		parsed, err := url.Parse(current)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return downloadedMedia{}, false
		}
		host := parsed.Hostname()
		if !hostAllowed(host, s.mediaHosts) {
			slog.Warn("skipping media outside the Weibo host list", "url", current)
			return downloadedMedia{}, false
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return downloadedMedia{}, false
		}
		// The cookie is attached to allowed hosts only, and redirects are followed by hand to keep it that way.
		if s.weiboCookies != "" && hostAllowed(host, s.cookieHosts) {
			request.Header.Set("Cookie", s.weiboCookies)
			request.Header.Set("Referer", mobileBase+"/")
		}
		response, err := s.http.Do(request)
		if err != nil {
			slog.Warn("media download failed", "url", current, "error", err)
			return downloadedMedia{}, false
		}

		switch response.StatusCode {
		case 301, 302, 303, 307, 308:
			location := response.Header.Get("Location")
			response.Body.Close()
			if location == "" {
				return downloadedMedia{}, false
			}
			next, err := parsed.Parse(location)
			if err != nil {
				return downloadedMedia{}, false
			}
			current = next.String()
			continue
		}
		result, ok := s.readMedia(response, parsed, media, budget)
		response.Body.Close()
		return result, ok
	}
	slog.Warn("too many media redirects", "url", media.URL)
	return downloadedMedia{}, false
}

func (s *Sender) readMedia(response *http.Response, parsed *url.URL, media MediaItem, budget int) (downloadedMedia, bool) {
	current := parsed.String()
	if response.StatusCode >= 400 {
		slog.Warn("media download failed", "status", response.StatusCode, "url", current)
		return downloadedMedia{}, false
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(response.Header.Get("Content-Type"), ";", 2)[0]))
	limit := minInt(s.maxMediaBytes, budget)
	if media.Kind == mediaPhoto {
		limit = minInt(limit, photoSizeLimit)
	}
	path := strings.ToLower(parsed.Path)
	if media.Kind == mediaVideo && strings.HasSuffix(path, ".m3u8") {
		slog.Info("an HLS playlist cannot be uploaded as a video file", "url", current)
		return downloadedMedia{}, false
	}
	if length, err := strconv.Atoi(response.Header.Get("Content-Length")); err == nil && length > limit {
		slog.Info("media file is too large", "bytes", length)
		return downloadedMedia{}, false
	}
	if media.Kind == mediaPhoto && contentType != "" && !strings.HasPrefix(contentType, "image/") &&
		!hasAnySuffix(path, []string{".jpg", ".jpeg", ".png", ".webp", ".gif"}) {
		slog.Info("media URL did not return an image", "url", current, "contentType", contentType)
		return downloadedMedia{}, false
	}
	if media.Kind == mediaVideo && contentType != "" && !strings.HasPrefix(contentType, "video/") && !hasAnySuffix(path, videoExtensions) {
		slog.Info("media URL is not a video file", "url", current, "contentType", contentType)
		return downloadedMedia{}, false
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		slog.Warn("media download failed", "url", current, "error", err)
		return downloadedMedia{}, false
	}
	if len(data) > limit {
		slog.Info("media file exceeds the size limit while streaming", "url", current)
		return downloadedMedia{}, false
	}
	if len(data) == 0 {
		return downloadedMedia{}, false
	}
	suffix := contentTypeSuffixes[contentType]
	if suffix == "" {
		name := path[strings.LastIndex(path, "/")+1:]
		if dot := strings.LastIndex(name, "."); dot >= 0 {
			suffix = name[dot:]
		} else {
			suffix = ".bin"
		}
	}
	return downloadedMedia{media: media, data: data, suffix: suffix}, true
}

// Send publishes one item to one chat. With diagnostics false (public destinations) only the
// content is ever sent; notes about failed media are added for test destinations only.
func (s *Sender) Send(ctx context.Context, chatID, accountName string, item FeedItem, diagnostics bool) error {
	text := messageText(accountName, item)
	mediaList := item.Media
	if len(mediaList) > s.maxMediaPerPost {
		mediaList = mediaList[:s.maxMediaPerPost]
	}
	var usable []downloadedMedia
	var failedURLs []string

	// Download first, so the text can travel as the caption of the first file.
	budget := s.maxMediaBytes
	for _, media := range mediaList {
		if budget <= 0 {
			failedURLs = append(failedURLs, media.URL)
			continue
		}
		downloaded, ok := s.download(ctx, media, budget)
		if !ok {
			failedURLs = append(failedURLs, media.URL)
			continue
		}
		usable = append(usable, downloaded)
		budget -= len(downloaded.data)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if len(usable) == 0 {
		if len(failedURLs) > 0 && diagnostics {
			text += "\n\n" + noteMediaDownloadFailed + "\n" + strings.Join(firstN(failedURLs, 5), "\n")
		}
		return s.sendText(ctx, chatID, text, false)
	}

	runes := []rune(text)
	caption, remainingText := text, ""
	if len(runes) > captionRunes {
		caption = string(runes[:captionRunes])
		remainingText = strings.TrimSpace(string(runes[captionRunes:]))
	}
	firstGroup := true
	for start := 0; start < len(usable); start += albumSize {
		end := minInt(start+albumSize, len(usable))
		group := usable[start:end]
		groupCaption := ""
		if firstGroup {
			groupCaption = caption
		}
		var err error
		if len(group) == 1 {
			err = s.telegram.SendSingle(ctx, chatID, group[0].media.Kind, groupCaption, uploadFile{"weibo-media" + group[0].suffix, group[0].data})
		} else {
			members := make([]groupMember, len(group))
			for index, entry := range group {
				members[index] = groupMember{kind: entry.media.Kind, file: uploadFile{"weibo-media" + entry.suffix, entry.data}}
			}
			members[0].caption = groupCaption
			err = s.telegram.SendAlbum(ctx, chatID, members)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A media file can be refused although it looked fine. Fall back to the text
			// instead of retrying the whole item forever.
			slog.Warn("media upload failed, sending the text only", "error", err)
			fallback := text
			if diagnostics {
				total := 0
				for _, entry := range usable {
					total += len(entry.data)
				}
				fallback += fmt.Sprintf("\n\nMedia upload failed: %v\n(%d files, %d KB in total)\n%s",
					err, len(usable), total/1024, strings.Join(mediaURLs(mediaList), "\n"))
			}
			return s.sendText(ctx, chatID, fallback, false)
		}
		firstGroup = false
	}

	if remainingText != "" {
		if err := s.sendText(ctx, chatID, remainingText, false); err != nil {
			return err
		}
	}
	if len(failedURLs) > 0 && diagnostics {
		return s.sendText(ctx, chatID, noteMediaNotUploaded+"\n"+strings.Join(firstN(failedURLs, 10), "\n"), true)
	}
	return nil
}

func firstN(values []string, limit int) []string {
	if len(values) > limit {
		return values[:limit]
	}
	return values
}

func mediaURLs(media []MediaItem) []string {
	urls := make([]string, 0, len(media))
	for _, item := range media {
		urls = append(urls, item.URL)
	}
	return firstN(urls, 10)
}
