package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

const alertCooldown = 30 * time.Minute

// App ties the store, the Weibo client and Telegram together.
type App struct {
	settings    *Settings
	store       *Store
	weibo       *WeiboClient
	telegram    *TelegramClient
	sender      *Sender
	botUsername string

	pollMu     sync.Mutex
	deliveryMu sync.Mutex

	alertMu    sync.Mutex
	alertTimes map[string]time.Time
	now        func() time.Time
}

func NewApp(settings *Settings, store *Store, weibo *WeiboClient, telegram *TelegramClient, sender *Sender) *App {
	return &App{
		settings: settings, store: store, weibo: weibo, telegram: telegram, sender: sender,
		alertTimes: map[string]time.Time{}, now: time.Now,
	}
}

// notifyTest sends an error alert to the test destinations only, at most once per key and cooldown.
func (a *App) notifyTest(ctx context.Context, key, text string) {
	a.alertMu.Lock()
	last, seen := a.alertTimes[key]
	if seen && a.now().Sub(last) < alertCooldown {
		a.alertMu.Unlock()
		return
	}
	a.alertTimes[key] = a.now()
	a.alertMu.Unlock()
	for _, destination := range a.store.Destinations(kindTest) {
		if err := a.telegram.SendMessage(ctx, destination.ChatID, truncateRunes(text, 3900), false); err != nil {
			slog.Warn("could not deliver an alert to a test destination", "chat", destination.ChatID, "error", err)
		}
	}
}

// deliverPending sends what is due. Only test destinations get diagnostic notes.
func (a *App) deliverPending(ctx context.Context, sourceID, limit int) (delivered, failed int) {
	a.deliveryMu.Lock()
	defer a.deliveryMu.Unlock()
	kinds := a.store.DestinationKinds()
	for _, entry := range a.store.PendingDeliveries(sourceID, limit) {
		if ctx.Err() != nil {
			return delivered, failed
		}
		err := a.sender.Send(ctx, entry.ChatID, entry.Source.Name, entry.Item, kinds[entry.ChatID] == kindTest)
		if err == nil {
			a.store.MarkDeliverySent(entry.Source.ID, entry.Item.ItemKey, entry.ChatID)
			delivered++
			slog.Info("delivered", "uid", entry.Source.UID, "type", entry.Item.EventType, "chat", entry.ChatID, "key", entry.Item.ItemKey[:12])
			continue
		}
		if ctx.Err() != nil {
			return delivered, failed
		}
		a.store.MarkDeliveryFailed(entry.Source.ID, entry.Item.ItemKey, entry.ChatID, err.Error())
		failed++
		slog.Error("delivery failed", "uid", entry.Source.UID, "type", entry.Item.EventType, "chat", entry.ChatID, "error", err)
		a.notifyTest(ctx, "deliver:"+entry.ChatID,
			fmt.Sprintf("[ERROR] Delivery to %s failed for %s (%s): %v", entry.ChatID, entry.Source.Name, entry.Item.EventType, err))
	}
	return delivered, failed
}

func reverseItems(items []FeedItem) []FeedItem {
	reversed := make([]FeedItem, len(items))
	for index, item := range items {
		reversed[len(items)-1-index] = item
	}
	return reversed
}

// numericStatusID is the id of a status as a number, 0 when it has none.
func numericStatusID(status obj) int64 {
	raw := statusIDOf(status)
	if !isDigits(raw) {
		return 0
	}
	value, _ := strconv.ParseInt(raw, 10, 64)
	return value
}

// pollSource checks one account and delivers what is new. Polls never run in parallel.
func (a *App) pollSource(ctx context.Context, source Source, forceComments bool) (string, error) {
	a.pollMu.Lock()
	defer a.pollMu.Unlock()
	return a.pollSourceLocked(ctx, source, forceComments)
}

func (a *App) pollSourceLocked(ctx context.Context, source Source, forceComments bool) (summary string, err error) {
	defer func() {
		if err != nil {
			a.store.MarkChecked(source.ID, err.Error())
			slog.Error("polling failed", "uid", source.UID, "error", err)
		}
	}()
	name, statuses, err := a.weibo.GetTimeline(ctx, source.UID)
	if err != nil {
		return "", err
	}
	if name != "" && source.Name == source.UID {
		// Replace the placeholder name after the first successful profile read.
		a.store.RenameSource(source.ID, name)
		source, _ = a.store.GetSourceByID(source.ID)
	}

	// Newest first by status id, so a pinned old post sits where it belongs in time.
	ordered := sortNewestFirst(statuses)
	var newestUnpinned int64
	for _, status := range ordered {
		if !isPinned(status) {
			if id := numericStatusID(status); id > newestUnpinned {
				newestUnpinned = id
			}
		}
	}
	var fresh, quiet []FeedItem
	for index := len(ordered) - 1; index >= 0; index-- { // oldest first
		status := ordered[index]
		item := a.weibo.ToFeedItem(ctx, source.UID, status)
		if source.Initialized && isPinned(status) && numericStatusID(status) < newestUnpinned {
			// An old post that was pinned later is not news: remember it, never send it.
			quiet = append(quiet, item)
			continue
		}
		fresh = append(fresh, item)
	}

	timelineCount := 0
	if !source.Initialized {
		a.store.InsertItems(source.ID, fresh, true)
		a.store.SetInitialized(source.ID, true)
		slog.Info("timeline baseline built", "uid", source.UID, "posts", len(fresh))
	} else {
		a.store.InsertItems(source.ID, quiet, true)
		timelineCount = a.store.InsertItems(source.ID, fresh, false)
	}

	commentCount := 0
	if forceComments || a.store.CommentsDue(source, a.settings.CommentsPollInterval) {
		events, commentErr := a.weibo.FetchCommentEvents(ctx, source.UID, statuses)
		switch {
		case commentErr != nil:
			a.store.MarkCommentsChecked(source.ID, commentErr.Error())
			slog.Warn("comment scan failed", "uid", source.UID, "error", commentErr)
			label := "Comment scan failed"
			if isAuthError(commentErr) {
				label = "Comment auth problem"
			}
			a.notifyTest(ctx, "comments:"+source.UID, fmt.Sprintf("[ERROR] %s for %s (%s): %v", label, source.Name, source.UID, commentErr))
		case !source.CommentsInitialized:
			a.store.InsertItems(source.ID, events, true)
			a.store.SetCommentsInitialized(source.ID, true)
			a.store.MarkCommentsChecked(source.ID, "")
			slog.Info("comment baseline built", "uid", source.UID, "events", len(events))
		default:
			commentCount = a.store.InsertItems(source.ID, events, false)
			a.store.MarkCommentsChecked(source.ID, "")
		}
	}

	a.store.MarkChecked(source.ID, "")
	limit := a.settings.MaxCatchupItems
	if limit < 20 {
		limit = 20
	}
	sent, failed := a.deliverPending(ctx, source.ID, limit)
	return fmt.Sprintf("%s: read %d posts, found %d new posts and %d new comments/replies; sent %d, failed %d.",
		source.Name, len(statuses), timelineCount, commentCount, sent, failed), nil
}

// pollLoop checks all active accounts, then waits for the configured interval.
func (a *App) pollLoop(ctx context.Context) {
	for {
		a.pollRound(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.settings.PollInterval):
		}
	}
}

func (a *App) pollRound(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("unexpected error in the sync loop", "panic", recovered)
		}
	}()
	if a.store.IsPaused() {
		return
	}
	for _, source := range a.store.ListSources(true) {
		if ctx.Err() != nil {
			return
		}
		summary, err := a.pollSource(ctx, source, false)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("account skipped this round", "uid", source.UID, "error", err)
			a.notifyTest(ctx, "poll:"+source.UID, fmt.Sprintf("[ERROR] Polling failed for %s (%s): %v", source.Name, source.UID, err))
			continue
		}
		slog.Info(summary)
	}
	a.deliverPending(ctx, 0, 50)
}
