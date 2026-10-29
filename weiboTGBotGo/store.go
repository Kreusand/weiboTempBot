package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	stateVersion = 1
	// Payloads of finished posts are dropped after this long. The key stays, so a post that is
	// still on the timeline (a pinned one, for example) is never mistaken for a new one.
	payloadRetention = 14 * 24 * time.Hour

	postPending = "pending"
	postSent    = "sent"
	postSkipped = "skipped"

	deliveryPending   = "pending"
	deliverySent      = "sent"
	deliveryCancelled = "cancelled"
)

// Delivery is the state of one post for one destination chat.
type Delivery struct {
	ChatID        string `json:"chatId"`
	Status        string `json:"status"`
	Attempts      int    `json:"attempts"`
	NextAttemptAt string `json:"nextAttemptAt,omitempty"`
	SentAt        string `json:"sentAt,omitempty"`
	LastError     string `json:"lastError,omitempty"`
}

// Post is a known item of a source together with its delivery state.
type Post struct {
	SourceID     int         `json:"sourceId"`
	ItemKey      string      `json:"itemKey"`
	Seq          int64       `json:"seq"`
	Item         *FeedItem   `json:"item,omitempty"`
	Status       string      `json:"status"`
	DiscoveredAt string      `json:"discoveredAt"`
	SentAt       string      `json:"sentAt,omitempty"`
	LastError    string      `json:"lastError,omitempty"`
	Deliveries   []*Delivery `json:"deliveries,omitempty"`
}

type stateData struct {
	Version      int            `json:"version"`
	NextSourceID int            `json:"nextSourceId"`
	NextSeq      int64          `json:"nextSeq"`
	Paused       bool           `json:"paused"`
	Sources      []*Source      `json:"sources"`
	Destinations []*Destination `json:"destinations"`
	Posts        []*Post        `json:"posts"`
}

// PendingDelivery is one item waiting to be sent to one chat.
type PendingDelivery struct {
	Source Source
	Item   FeedItem
	ChatID string
}

// StoreStats is a summary for the /status command.
type StoreStats struct {
	Sources      int
	ActiveSource int
	Destinations int
	Pending      int
	Sent         int
	Errors       []Source
}

// Store keeps all state in memory and writes it to one JSON file after every change.
type Store struct {
	mu    sync.Mutex
	path  string
	state *stateData
	index map[string]*Post
	now   func() time.Time
}

func postIndexKey(sourceID int, itemKey string) string {
	return strconv.Itoa(sourceID) + ":" + itemKey
}

// OpenStore loads the state file, or starts empty when it does not exist.
// An empty path or ":memory:" keeps everything in memory only.
func OpenStore(path string) (*Store, error) {
	store := &Store{
		index: map[string]*Post{},
		now:   time.Now,
		state: &stateData{Version: stateVersion, NextSourceID: 1, NextSeq: 1},
	}
	if path == "" || path == ":memory:" {
		return store, nil
	}
	store.path = path
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, store.state); err != nil {
			return nil, fmt.Errorf("%s is not a valid state file (an old SQLite state.db cannot be reused, use a new STATE_PATH): %w", path, err)
		}
	}
	if store.state.NextSourceID < 1 {
		store.state.NextSourceID = 1
	}
	if store.state.NextSeq < 1 {
		store.state.NextSeq = 1
	}
	for _, source := range store.state.Sources {
		if !validKind(source.Kind) {
			source.Kind = kindTest
		}
	}
	for _, destination := range store.state.Destinations {
		if !validKind(destination.Kind) {
			destination.Kind = kindTest
		}
	}
	for _, post := range store.state.Posts {
		store.index[postIndexKey(post.SourceID, post.ItemKey)] = post
	}
	return store, nil
}

func (s *Store) nowIso() string {
	return s.now().UTC().Format(time.RFC3339)
}

// saveLocked writes the state atomically: a temporary file is flushed and then renamed over the old one.
func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	cutoff := s.now().Add(-payloadRetention)
	for _, post := range s.state.Posts {
		if post.Item == nil || post.Status == postPending {
			continue
		}
		if discovered, err := time.Parse(time.RFC3339, post.DiscoveredAt); err == nil && discovered.Before(cutoff) {
			post.Item = nil
		}
	}
	data, err := json.Marshal(s.state)
	if err != nil {
		slog.Error("could not encode state", "error", err)
		return
	}
	temp := s.path + ".tmp"
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err == nil {
		_, err = file.Write(data)
		if err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err == nil {
		err = os.Rename(temp, s.path)
	}
	if err != nil {
		slog.Error("could not write state file", "path", s.path, "error", err)
	}
}

// Close flushes the state one last time.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveLocked()
}

// ---- sources ----

func (s *Store) matchingSourcesLocked(key string) []*Source {
	var result []*Source
	for _, source := range s.state.Sources {
		if source.UID == key || source.Name == key {
			result = append(result, source)
		}
	}
	return result
}

func (s *Store) sourceByIDLocked(id int) *Source {
	for _, source := range s.state.Sources {
		if source.ID == id {
			return source
		}
	}
	return nil
}

// AddSource registers an account. New accounts are test accounts unless kind says otherwise.
func (s *Store) AddSource(uid, name, kind string) (Source, error) {
	if kind == "" {
		kind = kindTest
	}
	if !validKind(kind) {
		return Source{}, errors.New("account kind must be 'public' or 'test'")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.state.Sources {
		if existing.UID == uid {
			return Source{}, fmt.Errorf("Weibo UID %s already exists", uid)
		}
	}
	source := &Source{
		ID: s.state.NextSourceID, UID: uid, Name: name, Active: true,
		CreatedAt: s.nowIso(), Kind: kind,
	}
	s.state.NextSourceID++
	s.state.Sources = append(s.state.Sources, source)
	s.saveLocked()
	return *source, nil
}

// GetSource finds an account by UID or by name.
func (s *Store) GetSource(key string) (Source, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	matches := s.matchingSourcesLocked(key)
	if len(matches) == 0 {
		return Source{}, false
	}
	return *matches[0], true
}

func (s *Store) GetSourceByID(id int) (Source, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if source := s.sourceByIDLocked(id); source != nil {
		return *source, true
	}
	return Source{}, false
}

func (s *Store) ListSources(activeOnly bool) []Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []Source
	for _, source := range s.state.Sources {
		if !activeOnly || source.Active {
			result = append(result, *source)
		}
	}
	return result
}

// RemoveSource deletes the matching accounts together with everything recorded for them.
func (s *Store) RemoveSource(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	matches := s.matchingSourcesLocked(key)
	if len(matches) == 0 {
		return false
	}
	removed := map[int]bool{}
	for _, source := range matches {
		removed[source.ID] = true
	}
	sources := s.state.Sources[:0]
	for _, source := range s.state.Sources {
		if !removed[source.ID] {
			sources = append(sources, source)
		}
	}
	s.state.Sources = sources
	posts := s.state.Posts[:0]
	for _, post := range s.state.Posts {
		if removed[post.SourceID] {
			delete(s.index, postIndexKey(post.SourceID, post.ItemKey))
			continue
		}
		posts = append(posts, post)
	}
	s.state.Posts = posts
	s.saveLocked()
	return true
}

func (s *Store) SetSourceActive(key string, active bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	matches := s.matchingSourcesLocked(key)
	for _, source := range matches {
		source.Active = active
	}
	if len(matches) > 0 {
		s.saveLocked()
	}
	return len(matches) > 0
}

func (s *Store) RenameSource(id int, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if source := s.sourceByIDLocked(id); source != nil {
		source.Name = name
		s.saveLocked()
	}
}

// SetSourceKind makes an account public or test. Switching to test cancels everything of the
// account that still waits for a public destination.
func (s *Store) SetSourceKind(key, kind string) (bool, error) {
	if !validKind(kind) {
		return false, errors.New("account kind must be 'public' or 'test'")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	matches := s.matchingSourcesLocked(key)
	if len(matches) == 0 {
		return false, nil
	}
	source := matches[0]
	source.Kind = kind
	if kind == kindTest {
		publicChats := map[string]bool{}
		for _, destination := range s.state.Destinations {
			if destination.Kind == kindPublic {
				publicChats[destination.ChatID] = true
			}
		}
		for _, post := range s.state.Posts {
			if post.SourceID != source.ID {
				continue
			}
			for _, delivery := range post.Deliveries {
				if delivery.Status == deliveryPending && publicChats[delivery.ChatID] {
					delivery.Status = deliveryCancelled
				}
			}
			if post.Status == postPending && !hasPendingDelivery(post) {
				post.Status = postSent
				post.SentAt = s.nowIso()
			}
		}
	}
	s.saveLocked()
	return true, nil
}

func hasPendingDelivery(post *Post) bool {
	for _, delivery := range post.Deliveries {
		if delivery.Status == deliveryPending {
			return true
		}
	}
	return false
}

func (s *Store) SetInitialized(id int, value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if source := s.sourceByIDLocked(id); source != nil {
		source.Initialized = value
		s.saveLocked()
	}
}

func (s *Store) SetCommentsInitialized(id int, value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if source := s.sourceByIDLocked(id); source != nil {
		source.CommentsInitialized = value
		s.saveLocked()
	}
}

// MarkChecked records the end of a timeline check; an empty errText clears the previous error.
func (s *Store) MarkChecked(id int, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if source := s.sourceByIDLocked(id); source != nil {
		source.LastChecked = s.nowIso()
		source.LastError = truncateRunes(errText, 1000)
		s.saveLocked()
	}
}

func (s *Store) MarkCommentsChecked(id int, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if source := s.sourceByIDLocked(id); source != nil {
		source.LastCommentsChecked = s.nowIso()
		source.LastCommentError = truncateRunes(errText, 1000)
		s.saveLocked()
	}
}

// CommentsDue reports whether the comments of an account have not been scanned for at least interval.
func (s *Store) CommentsDue(source Source, interval time.Duration) bool {
	if source.LastCommentsChecked == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339, source.LastCommentsChecked)
	if err != nil {
		return true
	}
	return s.now().Sub(last) >= interval
}

// ---- destinations ----

func (s *Store) destinationLocked(chatID string) *Destination {
	for _, destination := range s.state.Destinations {
		if destination.ChatID == chatID {
			return destination
		}
	}
	return nil
}

// eligibleLocked reports whether content of the source may be sent to the destination:
// test accounts only reach test destinations.
func eligible(source *Source, destination *Destination) bool {
	return source != nil && destination != nil && (source.Kind == kindPublic || destination.Kind == kindTest)
}

// AddDestination registers or renames a destination. An empty kind keeps the current kind of an
// existing destination and makes a new one a test destination. Pending items are queued for it.
func (s *Store) AddDestination(chatID, name, kind string) error {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return errors.New("chat id must not be empty")
	}
	if kind != "" && !validKind(kind) {
		return errors.New("destination kind must be 'public' or 'test'")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	destination := s.destinationLocked(chatID)
	if destination == nil {
		destination = &Destination{ChatID: chatID, AddedAt: s.nowIso(), Kind: kindTest}
		s.state.Destinations = append(s.state.Destinations, destination)
	}
	destination.Name = strings.TrimSpace(name)
	if destination.Name == "" {
		destination.Name = chatID
	}
	if kind != "" {
		destination.Kind = kind
	}
	// A new destination also gets the items that are still waiting, as far as it may receive them.
	for _, post := range s.state.Posts {
		if post.Status != postPending || !eligible(s.sourceByIDLocked(post.SourceID), destination) {
			continue
		}
		found := false
		for _, delivery := range post.Deliveries {
			if delivery.ChatID != chatID {
				continue
			}
			found = true
			if delivery.Status == deliveryCancelled {
				*delivery = Delivery{ChatID: chatID, Status: deliveryPending}
			}
		}
		if !found {
			post.Deliveries = append(post.Deliveries, &Delivery{ChatID: chatID, Status: deliveryPending})
		}
	}
	s.saveLocked()
	return nil
}

func (s *Store) RemoveDestination(chatID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	destinations := s.state.Destinations[:0]
	for _, destination := range s.state.Destinations {
		if destination.ChatID == chatID {
			found = true
			continue
		}
		destinations = append(destinations, destination)
	}
	if !found {
		return false
	}
	s.state.Destinations = destinations
	for _, post := range s.state.Posts {
		for _, delivery := range post.Deliveries {
			if delivery.ChatID == chatID && delivery.Status == deliveryPending {
				delivery.Status = deliveryCancelled
			}
		}
	}
	s.saveLocked()
	return true
}

func (s *Store) SetDestinationKind(chatID, kind string) (bool, error) {
	if !validKind(kind) {
		return false, errors.New("destination kind must be 'public' or 'test'")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	destination := s.destinationLocked(strings.TrimSpace(chatID))
	if destination == nil {
		return false, nil
	}
	destination.Kind = kind
	s.saveLocked()
	return true, nil
}

// Destinations lists destinations; an empty kind lists all of them.
func (s *Store) Destinations(kind string) []Destination {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []Destination
	for _, destination := range s.state.Destinations {
		if kind == "" || destination.Kind == kind {
			result = append(result, *destination)
		}
	}
	return result
}

func (s *Store) DestinationKinds() map[string]string {
	result := map[string]string{}
	for _, destination := range s.Destinations("") {
		result[destination.ChatID] = destination.Kind
	}
	return result
}

// ---- items and deliveries ----

func (s *Store) eligibleDestinationsLocked(source *Source) []*Destination {
	var result []*Destination
	for _, destination := range s.state.Destinations {
		if eligible(source, destination) {
			result = append(result, destination)
		}
	}
	return result
}

func (s *Store) insertLocked(source *Source, item FeedItem, skip bool) bool {
	key := postIndexKey(source.ID, item.ItemKey)
	if _, exists := s.index[key]; exists {
		return false
	}
	copied := item
	post := &Post{
		SourceID: source.ID, ItemKey: item.ItemKey, Seq: s.state.NextSeq,
		Item: &copied, Status: postPending, DiscoveredAt: s.nowIso(),
	}
	s.state.NextSeq++
	if skip {
		post.Status = postSkipped
	} else {
		for _, destination := range s.eligibleDestinationsLocked(source) {
			post.Deliveries = append(post.Deliveries, &Delivery{ChatID: destination.ChatID, Status: deliveryPending})
		}
	}
	s.state.Posts = append(s.state.Posts, post)
	s.index[key] = post
	return true
}

// InsertItems records items in the given order and returns how many were new. With skip the
// items only become known (the baseline) and are never sent. Pass items oldest first.
func (s *Store) InsertItems(sourceID int, items []FeedItem, skip bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	source := s.sourceByIDLocked(sourceID)
	if source == nil {
		return 0
	}
	inserted := 0
	for _, item := range items {
		if s.insertLocked(source, item, skip) {
			inserted++
		}
	}
	if inserted > 0 {
		s.saveLocked()
	}
	return inserted
}

// BackfillSkipped queues the most recent baseline items of an account again.
func (s *Store) BackfillSkipped(sourceID, count int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	source := s.sourceByIDLocked(sourceID)
	if source == nil {
		return 0
	}
	destinations := s.eligibleDestinationsLocked(source)
	updated := 0
	for index := len(s.state.Posts) - 1; index >= 0 && updated < count; index-- {
		post := s.state.Posts[index]
		if post.SourceID != sourceID || post.Status != postSkipped || post.Item == nil {
			continue
		}
		post.Status = postPending
		post.LastError = ""
		post.Deliveries = nil
		for _, destination := range destinations {
			post.Deliveries = append(post.Deliveries, &Delivery{ChatID: destination.ChatID, Status: deliveryPending})
		}
		updated++
	}
	if updated > 0 {
		s.saveLocked()
	}
	return updated
}

// PendingDeliveries returns what is due to be sent, oldest first. sourceID 0 means all accounts.
func (s *Store) PendingDeliveries(sourceID, limit int) []PendingDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var result []PendingDelivery
	for _, post := range s.state.Posts {
		if (sourceID != 0 && post.SourceID != sourceID) || post.Item == nil {
			continue
		}
		source := s.sourceByIDLocked(post.SourceID)
		for _, delivery := range post.Deliveries {
			if delivery.Status != deliveryPending {
				continue
			}
			if delivery.NextAttemptAt != "" {
				if due, err := time.Parse(time.RFC3339, delivery.NextAttemptAt); err == nil && due.After(now) {
					continue
				}
			}
			// Checked again at sending time, so a stale record can never reach a public chat.
			if !eligible(source, s.destinationLocked(delivery.ChatID)) {
				continue
			}
			result = append(result, PendingDelivery{Source: *source, Item: *post.Item, ChatID: delivery.ChatID})
			if len(result) >= limit {
				return result
			}
		}
	}
	return result
}

func (s *Store) deliveryLocked(sourceID int, itemKey, chatID string) (*Post, *Delivery) {
	post := s.index[postIndexKey(sourceID, itemKey)]
	if post == nil {
		return nil, nil
	}
	for _, delivery := range post.Deliveries {
		if delivery.ChatID == chatID {
			return post, delivery
		}
	}
	return post, nil
}

func (s *Store) MarkDeliverySent(sourceID int, itemKey, chatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	post, delivery := s.deliveryLocked(sourceID, itemKey, chatID)
	if delivery == nil {
		return
	}
	delivery.Status = deliverySent
	delivery.SentAt = s.nowIso()
	delivery.LastError = ""
	delivery.NextAttemptAt = ""
	if !hasPendingDelivery(post) {
		post.Status = postSent
		post.SentAt = s.nowIso()
		post.LastError = ""
	}
	s.saveLocked()
}

// MarkDeliveryFailed schedules a retry with exponential backoff (30 s doubling up to one hour).
func (s *Store) MarkDeliveryFailed(sourceID int, itemKey, chatID, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	post, delivery := s.deliveryLocked(sourceID, itemKey, chatID)
	if delivery == nil {
		return
	}
	delivery.Attempts++
	exponent := delivery.Attempts - 1
	if exponent > 7 {
		exponent = 7
	}
	delay := 30 * time.Second * time.Duration(1<<exponent)
	if delay > time.Hour {
		delay = time.Hour
	}
	delivery.NextAttemptAt = s.now().Add(delay).UTC().Format(time.RFC3339)
	delivery.LastError = truncateRunes(errText, 1000)
	post.LastError = delivery.LastError
	s.saveLocked()
}

// ---- settings and statistics ----

func (s *Store) SetPaused(paused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Paused = paused
	s.saveLocked()
}

func (s *Store) IsPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Paused
}

func (s *Store) Stats() StoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := StoreStats{Sources: len(s.state.Sources), Destinations: len(s.state.Destinations)}
	for _, source := range s.state.Sources {
		if source.Active {
			stats.ActiveSource++
		}
		if source.LastError != "" || source.LastCommentError != "" {
			stats.Errors = append(stats.Errors, *source)
		}
	}
	// Most recently checked first, five at most.
	for i := 1; i < len(stats.Errors); i++ {
		for j := i; j > 0 && stats.Errors[j].LastChecked > stats.Errors[j-1].LastChecked; j-- {
			stats.Errors[j], stats.Errors[j-1] = stats.Errors[j-1], stats.Errors[j]
		}
	}
	if len(stats.Errors) > 5 {
		stats.Errors = stats.Errors[:5]
	}
	for _, post := range s.state.Posts {
		for _, delivery := range post.Deliveries {
			switch delivery.Status {
			case deliveryPending:
				stats.Pending++
			case deliverySent:
				stats.Sent++
			}
		}
	}
	return stats
}

// truncateRunes shortens text to at most limit characters.
func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
