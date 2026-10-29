package main

const (
	kindPublic = "public"
	kindTest   = "test"

	mediaPhoto = "photo"
	mediaVideo = "video"

	eventPost         = "post"
	eventComment      = "comment"
	eventCommentReply = "comment_reply"
)

// MediaItem is one photo or video attached to a post.
type MediaItem struct {
	URL  string `json:"url"`
	Kind string `json:"kind"`
}

// FeedItem is one thing to publish: a post, a comment or a comment reply.
type FeedItem struct {
	ItemKey    string      `json:"itemKey"`
	Title      string      `json:"title"`
	Summary    string      `json:"summary"`
	Link       string      `json:"link"`
	Published  string      `json:"published"`
	Media      []MediaItem `json:"media"`
	Tags       []string    `json:"tags"`
	EventType  string      `json:"eventType"`
	ParentLink string      `json:"parentLink"`
}

// Source is a monitored Weibo account.
type Source struct {
	ID                  int    `json:"id"`
	UID                 string `json:"uid"`
	Name                string `json:"name"`
	Active              bool   `json:"active"`
	Initialized         bool   `json:"initialized"`
	CommentsInitialized bool   `json:"commentsInitialized"`
	LastChecked         string `json:"lastChecked,omitempty"`
	LastError           string `json:"lastError,omitempty"`
	LastCommentsChecked string `json:"lastCommentsChecked,omitempty"`
	LastCommentError    string `json:"lastCommentError,omitempty"`
	CreatedAt           string `json:"createdAt"`
	// Kind is "public" or "test". Test accounts only ever reach test destinations.
	Kind string `json:"kind"`
}

// Destination is a Telegram chat or channel that receives content.
type Destination struct {
	ChatID  string `json:"chatId"`
	Name    string `json:"name"`
	AddedAt string `json:"addedAt"`
	// Kind is "public" or "test". Only test destinations receive diagnostics.
	Kind string `json:"kind"`
}

func validKind(kind string) bool {
	return kind == kindPublic || kind == kindTest
}
