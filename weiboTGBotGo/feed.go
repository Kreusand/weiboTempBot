package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var videoExtensions = []string{".mp4", ".m4v", ".mov", ".webm"}

func hasAnySuffix(text string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(text, suffix) {
			return true
		}
	}
	return false
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func userIDOf(user obj) string {
	return firstStr(user["idstr"], user["id"])
}

func statusIDOf(status obj) string {
	return firstStr(status["idstr"], status["id"], status["mid"])
}

func makeStatusLink(uid string, status obj) string {
	bid := strings.TrimSpace(firstStr(status["bid"], status["mblogid"]))
	statusID := strings.TrimSpace(firstStr(status["idstr"], status["id"]))
	if bid != "" {
		return "https://weibo.com/" + uid + "/" + bid
	}
	if statusID != "" {
		return "https://m.weibo.cn/detail/" + statusID
	}
	return "https://weibo.com/u/" + uid
}

// sortNewestFirst orders statuses newest first. Weibo status ids grow with time, so sorting by id
// also moves a pinned old post to its real position instead of leaving it at the top.
func sortNewestFirst(statuses []obj) []obj {
	sorted := append([]obj(nil), statuses...)
	idKey := func(status obj) int64 {
		raw := statusIDOf(status)
		if !isDigits(raw) {
			return 0
		}
		value, _ := strconv.ParseInt(raw, 10, 64)
		return value
	}
	sort.SliceStable(sorted, func(a, b int) bool { return idKey(sorted[a]) > idKey(sorted[b]) })
	return sorted
}

// isPinned is true for a post the account pinned to the top of its timeline.
func isPinned(status obj) bool {
	for _, field := range []string{"isTop", "is_top", "isPinnedCard"} {
		switch value := status[field].(type) {
		case bool:
			if value {
				return true
			}
		case json.Number:
			if value.String() == "1" {
				return true
			}
		case string:
			if value == "1" {
				return true
			}
		}
	}
	title := asObj(status["title"])
	return title != nil && strings.Contains(asStr(title["text"]), pinnedLabel)
}

// videoPageLink returns the page of a video post. page_info is also used for topic pages, articles
// and plain links, which must not be presented as videos.
func videoPageLink(status obj, videoMedia []MediaItem) string {
	pageInfo := asObj(status["page_info"])
	link := strings.TrimSpace(asStr(pageInfo["page_url"]))
	isVideo := strings.EqualFold(asStr(pageInfo["type"]), "video") || len(videoMedia) > 0
	if isVideo && (strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://")) {
		return link
	}
	return ""
}

func dateString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case json.Number:
		if seconds, err := typed.Float64(); err == nil && seconds != 0 {
			return time.Unix(int64(seconds), 0).Local().Format("2006-01-02T15:04:05-07:00")
		}
		return typed.String()
	case string:
		return typed
	}
	return asStr(value)
}

func normalizeMediaURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	return ""
}

func collectPhotos(status obj) []MediaItem {
	var result []MediaItem
	seen := map[string]bool{}
	add := func(raw string) {
		if mediaURL := normalizeMediaURL(raw); mediaURL != "" && !seen[mediaURL] {
			seen[mediaURL] = true
			result = append(result, MediaItem{URL: mediaURL, Kind: mediaPhoto})
		}
	}

	var pics []any
	switch typed := status["pics"].(type) {
	case []any:
		pics = typed
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			pics = append(pics, typed[key])
		}
	}
	for _, entry := range pics {
		pic := asObj(entry)
		if pic == nil {
			continue
		}
		large := asObj(firstTruthy(pic["large"], pic["largest"], pic["pic_big"]))
		add(firstStr(large["url"], pic["url"], pic["pic_big"]))
	}

	if picInfos := asObj(status["pic_infos"]); picInfos != nil {
		keys := make([]string, 0, len(picInfos))
		for key := range picInfos {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			pic := asObj(picInfos[key])
			if pic == nil {
				continue
			}
			large := asObj(firstTruthy(pic["largest"], pic["original"], pic["large"]))
			add(asStr(large["url"]))
		}
	}
	return result
}

// collectVideo returns the direct video file of a post, if the post has one.
func collectVideo(status obj) []MediaItem {
	mediaInfo := asObj(asObj(status["page_info"])["media_info"])
	for _, field := range []string{"mp4_hd_url", "stream_url_hd", "stream_url", "stream_url_hq", "mp4_sd_url"} {
		videoURL := normalizeMediaURL(asStr(mediaInfo[field]))
		if videoURL == "" {
			continue
		}
		path, query, hasQuery := strings.Cut(videoURL, "?")
		if !hasQuery {
			query = videoURL
		}
		if hasAnySuffix(strings.ToLower(path), videoExtensions) || strings.Contains(strings.ToLower(query), "mp4") {
			return []MediaItem{{URL: videoURL, Kind: mediaVideo}}
		}
	}
	return nil
}

func hasKind(media []MediaItem, kind string) bool {
	for _, item := range media {
		if item.Kind == kind {
			return true
		}
	}
	return false
}

func statusTags(status obj, media []MediaItem) []string {
	tags := []string{tagPost}
	visible := asObj(status["visible"])
	var visibleType, visibleList any
	if visible != nil || status["visible"] == nil || !truthy(status["visible"]) {
		visibleType = visible["type"]
		visibleList = visible["list_id"]
	} else {
		visibleType = status["visible_type"]
	}
	restricted := firstTruthy(status["is_fans_only"], status["fans_only"], status["isFollowersOnly"])
	if truthy(restricted) || !isZeroish(visibleType) || truthy(visibleList) {
		tags = append(tags, tagFansOnly)
	}
	if truthy(status["retweeted_status"]) {
		tags = append(tags, tagRepost)
	}
	pageInfo := asObj(status["page_info"])
	mediaInfo := asObj(pageInfo["media_info"])
	hasVideo := hasKind(media, mediaVideo) || strings.EqualFold(asStr(pageInfo["type"]), "video") ||
		truthy(mediaInfo["stream_url"]) || truthy(mediaInfo["mp4_hd_url"]) || truthy(mediaInfo["stream_url_hd"])
	if hasVideo {
		tags = append(tags, tagVideo)
	}
	if hasKind(media, mediaPhoto) || asInt(status["pic_num"]) > 0 {
		tags = append(tags, tagPhoto)
	}
	return tags
}

// commentItem builds the item of a comment or of a reply. It returns false when the comment has no id.
func commentItem(uid string, status, comment obj, postText, postLink string, isReply bool, parent obj) (FeedItem, bool) {
	commentID := firstStr(comment["idstr"], comment["id"])
	if commentID == "" {
		return FeedItem{}, false
	}
	commentText := textOf(firstTruthy(comment["text_raw"], comment["text"], comment["reply_original_text"]))
	commentName := firstStr(asObj(comment["user"])["screen_name"])
	if commentName == "" {
		commentName = uid
	}
	if commentText == "" {
		commentText = emptyCommentBody
	}
	tag, keyPrefix, title, eventType := tagComment, "comment", commentName+commentTitleSuffix, eventComment
	if isReply {
		tag, keyPrefix, title, eventType = tagCommentRepl, "reply", commentName+replyTitleSuffix, eventCommentReply
	}
	summary := commentName + "：" + commentText
	if isReply && parent != nil {
		parentName := firstStr(asObj(parent["user"])["screen_name"])
		if parentName == "" {
			parentName = defaultParentName
		}
		summary += "\n" + replyToPrefix + parentName
	}
	if postText != "" {
		summary += "\n\n" + parentPostPrefix + truncateRunes(postText, 700)
	}
	statusID := statusIDOf(status)
	return FeedItem{
		ItemKey:    sha256Hex(fmt.Sprintf("%s:%s:%s:%s", keyPrefix, uid, statusID, commentID)),
		Title:      title,
		Summary:    summary,
		Link:       "https://m.weibo.cn/detail/" + statusID + "?cid=" + url.PathEscape(commentID),
		Published:  dateString(comment["created_at"]),
		Tags:       []string{tag},
		EventType:  eventType,
		ParentLink: postLink,
	}, true
}

// jsonMarshalSorted encodes a value as JSON; Go sorts map keys, so the result is stable.
func jsonMarshalSorted(value any) (string, error) {
	encoded, err := json.Marshal(value)
	return string(encoded), err
}
