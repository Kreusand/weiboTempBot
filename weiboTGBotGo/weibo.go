package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const mobileBase = "https://m.weibo.cn"

const mobileUserAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) " +
	"AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Mobile/15E148 Safari/604.1"

// Hosts the Weibo cookie may be sent to, and API hosts the client may talk to.
var weiboAPIHosts = []string{"weibo.cn", "weibo.com", "weibo.com.cn"}

// WeiboRequestError is a failed or unusable request to Weibo.
type WeiboRequestError struct{ Message string }

func (e *WeiboRequestError) Error() string { return e.Message }

// WeiboAuthError means the login cookie was rejected, expired or rate limited.
type WeiboAuthError struct{ Message string }

func (e *WeiboAuthError) Error() string { return e.Message }

func isAuthError(err error) bool {
	var target *WeiboAuthError
	return errors.As(err, &target)
}

// isWeiboError is true for request errors and auth errors alike.
func isWeiboError(err error) bool {
	var request *WeiboRequestError
	return isAuthError(err) || errors.As(err, &request)
}

func hostAllowed(host string, roots []string) bool {
	host = strings.ToLower(host)
	for _, root := range roots {
		if host == root || strings.HasSuffix(host, "."+root) {
			return true
		}
	}
	return false
}

// WeiboClient reads the mobile web API of Weibo with a login cookie.
type WeiboClient struct {
	http                  *http.Client
	cookies               string
	baseURL               string
	allowedHosts          []string
	timelinePages         int
	watchPosts            int
	commentsTopLevelLimit int
	replyThreadsPerPoll   int

	cacheMu       sync.Mutex
	fullText      map[string]string
	fullTextOrder []string
}

func NewWeiboClient(settings *Settings) *WeiboClient {
	return &WeiboClient{
		http: &http.Client{
			Timeout: settings.RequestTimeout,
			// Redirects are followed by hand so that every hop can be checked against the host list.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		cookies:               settings.WeiboCookies,
		baseURL:               mobileBase,
		allowedHosts:          weiboAPIHosts,
		timelinePages:         settings.TimelinePages,
		watchPosts:            settings.CommentsWatchPosts,
		commentsTopLevelLimit: settings.CommentsTopLevelLimit,
		replyThreadsPerPoll:   settings.ReplyThreadsPerPoll,
		fullText:              map[string]string{},
	}
}

func (c *WeiboClient) getJSON(ctx context.Context, path string, params url.Values, referer string) (obj, error) {
	target := path
	if !strings.HasPrefix(path, "http") {
		target = c.baseURL + path
	}
	query := params
	if referer == "" {
		referer = mobileBase + "/"
	}
	for hop := 0; hop < 6; hop++ {
		parsed, err := url.Parse(target)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !hostAllowed(parsed.Hostname(), c.allowedHosts) {
			return nil, &WeiboRequestError{"Weibo redirected to a host outside Weibo, request stopped"}
		}
		if len(query) > 0 {
			merged := parsed.Query()
			for key, values := range query {
				for _, value := range values {
					merged.Add(key, value)
				}
			}
			parsed.RawQuery = merged.Encode()
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return nil, &WeiboRequestError{err.Error()}
		}
		request.Header.Set("MWeibo-Pwa", "1")
		request.Header.Set("X-Requested-With", "XMLHttpRequest")
		request.Header.Set("Accept", "application/json, text/plain, */*")
		request.Header.Set("User-Agent", mobileUserAgent)
		request.Header.Set("Cookie", c.cookies)
		request.Header.Set("Referer", referer)

		response, err := c.http.Do(request)
		if err != nil {
			return nil, &WeiboRequestError{"Weibo request failed: " + err.Error()}
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		response.Body.Close()
		if readErr != nil {
			return nil, &WeiboRequestError{"reading the Weibo response failed: " + readErr.Error()}
		}

		switch response.StatusCode {
		case 301, 302, 303, 307, 308:
			location := response.Header.Get("Location")
			if location == "" {
				return nil, &WeiboRequestError{"Weibo redirect without a Location header"}
			}
			next, err := parsed.Parse(location)
			if err != nil {
				return nil, &WeiboRequestError{"Weibo redirect to an invalid address"}
			}
			target = next.String()
			query = nil
			continue
		}
		if response.StatusCode >= 400 {
			if response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 432 {
				return nil, &WeiboAuthError{fmt.Sprintf("Weibo returned HTTP %d, the login may have expired or the account is rate limited", response.StatusCode)}
			}
			return nil, &WeiboRequestError{fmt.Sprintf("Weibo returned HTTP %d: %s", response.StatusCode, truncateRunes(string(body), 200))}
		}
		decoded, err := decodeJSON(body)
		if err != nil {
			lowerBody := strings.ToLower(string(body))
			lowerURL := strings.ToLower(parsed.String())
			if strings.Contains(lowerURL, "passport") || strings.Contains(lowerURL, "login") || strings.Contains(lowerBody, "passport.weibo.cn") {
				return nil, &WeiboAuthError{"Weibo redirected to the login page, update WEIBO_COOKIES"}
			}
			return nil, &WeiboRequestError{"Weibo did not return JSON: " + truncateRunes(string(body), 200)}
		}
		data := asObj(decoded)
		if data == nil {
			return obj{"raw": decoded}, nil
		}
		retcode := asStr(data["retcode"])
		if asStr(data["ok"]) == "-100" || retcode == "6102" || retcode == "100001" {
			return nil, &WeiboAuthError{"the Weibo login expired or Weibo asks to log in again, update WEIBO_COOKIES"}
		}
		return data, nil
	}
	return nil, &WeiboRequestError{"too many Weibo redirects"}
}

// LoginInfo is what /api/config reports about the cookie.
type LoginInfo struct {
	UID   string
	Login string
}

func (c *WeiboClient) CheckLogin(ctx context.Context) (LoginInfo, error) {
	data, err := c.getJSON(ctx, "/api/config", nil, "")
	if err != nil {
		return LoginInfo{}, err
	}
	payload := asObj(data["data"])
	uid := firstStr(payload["uid"], asObj(payload["user"])["id"])
	if login := payload["login"]; login != nil && (isZeroish(login) || asStr(login) == "False") {
		return LoginInfo{}, &WeiboAuthError{"Weibo reports that the cookie is not logged in, copy the mobile web cookie again"}
	}
	return LoginInfo{UID: uid, Login: asStr(payload["login"])}, nil
}

// statusesFromCard extracts the posts of one timeline card and marks pinned ones.
func statusesFromCard(card obj) []obj {
	var result []obj
	if mblog := asObj(card["mblog"]); mblog != nil {
		if strings.HasPrefix(asStr(card["profile_type_id"]), "proweibotop") {
			mblog["isPinnedCard"] = true
		}
		result = append(result, mblog)
	}
	for _, child := range asList(card["card_group"]) {
		if mblog := asObj(asObj(child)["mblog"]); mblog != nil {
			result = append(result, mblog)
		}
	}
	return result
}

// GetTimeline returns the display name and the recent posts of an account in the order Weibo lists them.
func (c *WeiboClient) GetTimeline(ctx context.Context, uid string) (string, []obj, error) {
	referer := mobileBase + "/u/" + uid
	index, err := c.getJSON(ctx, "/api/container/getIndex", url.Values{"type": {"uid"}, "value": {uid}}, referer)
	if err != nil {
		return "", nil, err
	}
	payload := asObj(index["data"])
	name := firstStr(asObj(payload["userInfo"])["screen_name"])
	if name == "" {
		name = uid
	}
	containerID := ""
	for _, tab := range asList(asObj(payload["tabsInfo"])["tabs"]) {
		tabObj := asObj(tab)
		if asStr(tabObj["tab_type"]) == "weibo" && truthy(tabObj["containerid"]) {
			containerID = asStr(tabObj["containerid"])
			break
		}
	}
	if containerID == "" {
		containerID = "107603" + uid
	}

	var statuses []obj
	seen := map[string]bool{}
	for page := 1; page <= c.timelinePages; page++ {
		result, err := c.getJSON(ctx, "/api/container/getIndex", url.Values{
			"type": {"uid"}, "value": {uid}, "containerid": {containerID}, "page": {strconv.Itoa(page)},
		}, referer)
		if err != nil {
			return "", nil, err
		}
		cards := asList(asObj(result["data"])["cards"])
		if len(cards) == 0 && page == 1 {
			// A few response versions need only the container id, without uid and type.
			result, err = c.getJSON(ctx, "/api/container/getIndex", url.Values{"containerid": {containerID}, "page": {"1"}}, referer)
			if err != nil {
				return "", nil, err
			}
			cards = asList(asObj(result["data"])["cards"])
		}
		found := 0
		for _, card := range cards {
			for _, status := range statusesFromCard(asObj(card)) {
				statusID := statusIDOf(status)
				if statusID != "" && !seen[statusID] {
					seen[statusID] = true
					statuses = append(statuses, status)
					found++
				}
			}
		}
		if found == 0 {
			break
		}
	}
	if len(statuses) == 0 && (asStr(index["ok"]) == "0" || !truthy(payload)) {
		message := firstStr(index["msg"], index["message"])
		if message == "" {
			message = "no timeline returned"
		}
		return "", nil, &WeiboRequestError{fmt.Sprintf("reading UID %s failed: %s", uid, message)}
	}
	return name, statuses, nil
}

func (c *WeiboClient) cachedFullText(statusID string) (string, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	text, ok := c.fullText[statusID]
	return text, ok
}

func (c *WeiboClient) storeFullText(statusID, text string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if _, exists := c.fullText[statusID]; !exists {
		c.fullTextOrder = append(c.fullTextOrder, statusID)
	}
	c.fullText[statusID] = text
	for len(c.fullTextOrder) > 500 {
		delete(c.fullText, c.fullTextOrder[0])
		c.fullTextOrder = c.fullTextOrder[1:]
	}
}

// ToFeedItem converts one post into the item that gets published.
func (c *WeiboClient) ToFeedItem(ctx context.Context, uid string, status obj) FeedItem {
	statusID := statusIDOf(status)
	if statusID == "" {
		encoded, _ := jsonMarshalSorted(status)
		statusID = sha256Hex(encoded)[:24]
	}
	text := textOf(firstTruthy(status["text"], status["text_raw"]))
	if truthy(status["isLongText"]) && len([]rune(text)) < 500 {
		if cached, ok := c.cachedFullText(statusID); ok && cached != "" {
			text = cached
		} else {
			full, err := c.getJSON(ctx, "/statuses/extend", url.Values{"id": {statusID}}, mobileBase+"/detail/"+statusID)
			if err != nil {
				slog.Debug("reading the full text failed, keeping the timeline text", "id", statusID, "error", err)
			} else {
				data := asObj(full["data"])
				if fullText := firstTruthy(data["longTextContent"], data["text"]); fullText != nil {
					text = textOf(fullText)
					c.storeFullText(statusID, text)
				}
			}
		}
	}

	media := collectPhotos(status)
	videoMedia := collectVideo(status)
	if len(videoMedia) > 0 {
		kept := media[:0:0]
		for _, item := range media {
			if item.Kind != mediaVideo {
				kept = append(kept, item)
			}
		}
		media = append(kept, videoMedia...)
	}
	if retweeted := asObj(status["retweeted_status"]); retweeted != nil {
		originalText := textOf(firstTruthy(retweeted["text"], retweeted["text_raw"]))
		originalUser := firstStr(asObj(retweeted["user"])["screen_name"])
		if originalUser == "" {
			originalUser = repostDefaultAuthor
		}
		if originalText != "" {
			block := repostHeading + originalUser + "：\n" + originalText
			if text != "" {
				text += "\n\n" + block
			} else {
				text = block
			}
		}
		known := map[string]bool{}
		for _, item := range media {
			known[item.URL] = true
		}
		for _, item := range append(collectPhotos(retweeted), collectVideo(retweeted)...) {
			if !known[item.URL] {
				media = append(media, item)
			}
		}
	}

	author := firstStr(asObj(status["user"])["screen_name"])
	if author == "" {
		author = uid
	}
	if videoPage := videoPageLink(status, videoMedia); videoPage != "" && !strings.Contains(text, videoPage) {
		text = strings.TrimSpace(text + "\n\n" + labelVideoPage + videoPage)
	}
	if text == "" {
		text = emptyPostBody
	}
	return FeedItem{
		ItemKey:   sha256Hex("post:" + uid + ":" + statusID),
		Title:     author + postTitleSuffix,
		Summary:   text,
		Link:      makeStatusLink(uid, status),
		Published: dateString(status["created_at"]),
		Media:     media,
		Tags:      statusTags(status, media),
		EventType: eventPost,
	}
}

// FetchCommentEvents collects the comments and replies written by the monitored account itself
// under its own recent posts.
func (c *WeiboClient) FetchCommentEvents(ctx context.Context, uid string, statuses []obj) ([]FeedItem, error) {
	var result []FeedItem
	threadCount := 0
	limit := c.watchPosts
	if limit > len(statuses) {
		limit = len(statuses)
	}
	for _, status := range statuses[:limit] {
		postID := statusIDOf(status)
		if postID == "" {
			continue
		}
		postUser := asObj(status["user"])
		targetName := asStr(postUser["screen_name"])
		// Only threads under posts written by the monitored account are inspected.
		if userIDOf(postUser) != uid {
			continue
		}
		postText := textOf(firstTruthy(status["text"], status["text_raw"]))
		postLink := makeStatusLink(uid, status)

		comments, err := c.fetchTopComments(ctx, postID)
		if err != nil {
			if isAuthError(err) {
				return nil, err
			}
			slog.Warn("reading comments failed", "post", postID, "error", err)
			continue
		}
		if len(comments) > c.commentsTopLevelLimit {
			comments = comments[:c.commentsTopLevelLimit]
		}
		for _, comment := range comments {
			commentID := firstStr(comment["idstr"], comment["id"])
			if commentID == "" {
				continue
			}
			if isTargetUser(asObj(comment["user"]), uid, targetName) {
				if event, ok := commentItem(uid, status, comment, postText, postLink, false, nil); ok {
					result = append(result, event)
				}
			}
			nested := nestedReplies(comment)
			nestedIDs := map[string]bool{}
			for _, reply := range nested {
				if replyID := firstStr(reply["idstr"], reply["id"]); replyID != "" {
					nestedIDs[replyID] = true
				}
				if isTargetUser(asObj(reply["user"]), uid, targetName) {
					if event, ok := commentItem(uid, status, reply, postText, postLink, true, comment); ok {
						result = append(result, event)
					}
				}
			}
			total := asInt(firstTruthy(comment["total_number"], comment["reply_count"]))
			if total > len(nested) && threadCount < c.replyThreadsPerPoll {
				threadCount++
				replies, err := c.fetchChildReplies(ctx, postID, commentID)
				if err != nil {
					if isAuthError(err) {
						return nil, err
					}
					slog.Warn("reading comment replies failed", "post", postID, "comment", commentID, "error", err)
					continue
				}
				for _, reply := range replies {
					replyID := firstStr(reply["idstr"], reply["id"])
					if replyID == "" || nestedIDs[replyID] {
						continue
					}
					if isTargetUser(asObj(reply["user"]), uid, targetName) {
						if event, ok := commentItem(uid, status, reply, postText, postLink, true, comment); ok {
							result = append(result, event)
						}
					}
				}
			}
		}
	}
	// One event per key; the last occurrence wins but the first position is kept.
	position := map[string]int{}
	var unique []FeedItem
	for _, item := range result {
		if index, exists := position[item.ItemKey]; exists {
			unique[index] = item
			continue
		}
		position[item.ItemKey] = len(unique)
		unique = append(unique, item)
	}
	return unique, nil
}

func (c *WeiboClient) fetchTopComments(ctx context.Context, postID string) ([]obj, error) {
	referer := mobileBase + "/detail/" + postID
	data, err := c.getJSON(ctx, "/api/comments/show", url.Values{"id": {postID}, "page": {"1"}}, referer)
	if err == nil {
		if comments := extractCommentList(data); len(comments) > 0 {
			return comments, nil
		}
	} else if isAuthError(err) {
		return nil, err
	}
	data, err = c.getJSON(ctx, "/comments/hotflow", url.Values{
		"id": {postID}, "mid": {postID}, "max_id": {"0"}, "max_id_type": {"0"},
	}, referer)
	if err != nil {
		return nil, err
	}
	return extractCommentList(data), nil
}

func (c *WeiboClient) fetchChildReplies(ctx context.Context, postID, commentID string) ([]obj, error) {
	data, err := c.getJSON(ctx, "/comments/hotFlowChild", url.Values{
		"cid": {commentID}, "max_id": {"0"}, "max_id_type": {"0"},
	}, mobileBase+"/detail/"+postID+"?cid="+commentID)
	if err != nil {
		return nil, err
	}
	var replies []any
	switch payload := data["data"].(type) {
	case map[string]any:
		replies = asList(firstTruthy(payload["data"], payload["comments"]))
	case []any:
		replies = payload
	}
	return onlyObjects(replies), nil
}

func onlyObjects(values []any) []obj {
	var result []obj
	for _, value := range values {
		if object := asObj(value); object != nil {
			result = append(result, object)
		}
	}
	return result
}

func extractCommentList(data obj) []obj {
	var items []any
	switch payload := data["data"].(type) {
	case []any:
		items = payload
	case map[string]any:
		raw := firstTruthy(payload["data"], payload["comments"], payload["comment_list"])
		if nested := asObj(raw); nested != nil {
			raw = nested["data"]
		}
		items = asList(raw)
	}
	return onlyObjects(items)
}

func nestedReplies(comment obj) []obj {
	replies, ok := comment["comments"].([]any)
	if !ok {
		replies, _ = comment["comment"].([]any)
	}
	return onlyObjects(replies)
}

func isTargetUser(user obj, uid, targetName string) bool {
	if userID := userIDOf(user); userID != "" {
		return userID == uid
	}
	return targetName != "" && asStr(user["screen_name"]) == targetName
}
