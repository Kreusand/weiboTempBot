package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, Source) {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource("1234567890", "test account", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddDestination("-100123456", "test group", ""); err != nil {
		t.Fatal(err)
	}
	return store, source
}

func sampleItem(key string) FeedItem {
	return FeedItem{
		ItemKey: key, Title: "title", Summary: "body", Link: "https://weibo.com/123/abc",
		Published: "2026-10-28T10:00:00+08:00", Media: []MediaItem{{URL: "https://example.com/p.jpg", Kind: mediaPhoto}},
		Tags: []string{"#a", "#b"}, EventType: eventPost,
	}
}

func chatsOf(store *Store, sourceID int) map[string]bool {
	result := map[string]bool{}
	for _, entry := range store.PendingDeliveries(sourceID, 100) {
		result[entry.ChatID] = true
	}
	return result
}

func sameSet(got map[string]bool, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, key := range want {
		if !got[key] {
			return false
		}
	}
	return true
}

func TestPendingItemCreatesDelivery(t *testing.T) {
	store, source := newTestStore(t)
	if store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, false) != 1 {
		t.Fatal("item was not inserted")
	}
	pending := store.PendingDeliveries(source.ID, 10)
	if len(pending) != 1 || pending[0].ChatID != "-100123456" || len(pending[0].Item.Tags) != 2 {
		t.Fatalf("unexpected pending deliveries: %+v", pending)
	}
	if store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, false) != 0 {
		t.Fatal("duplicate item must not be inserted twice")
	}
}

func TestBaselineItemIsSkipped(t *testing.T) {
	store, source := newTestStore(t)
	store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, true)
	if len(store.PendingDeliveries(source.ID, 10)) != 0 {
		t.Fatal("baseline items must not be delivered")
	}
}

func TestNewDestinationGetsPendingItems(t *testing.T) {
	store, source := newTestStore(t)
	store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, false)
	store.AddDestination("@another", "another", "")
	if !sameSet(chatsOf(store, source.ID), "-100123456", "@another") {
		t.Fatalf("got %v", chatsOf(store, source.ID))
	}
}

func TestRemoveDestinationCancelsPending(t *testing.T) {
	store, source := newTestStore(t)
	store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, false)
	if !store.RemoveDestination("-100123456") {
		t.Fatal("destination not removed")
	}
	if len(store.PendingDeliveries(source.ID, 10)) != 0 {
		t.Fatal("removed destination must not receive anything")
	}
	if store.RemoveDestination("-100123456") {
		t.Fatal("removing twice must report false")
	}
}

func TestBackfillPromotesNewestSkippedItems(t *testing.T) {
	store, source := newTestStore(t)
	store.InsertItems(source.ID, []FeedItem{sampleItem("old"), sampleItem("mid"), sampleItem("new")}, true)
	if store.BackfillSkipped(source.ID, 2) != 2 {
		t.Fatal("expected two items to be queued")
	}
	pending := store.PendingDeliveries(source.ID, 10)
	if len(pending) != 2 || pending[0].Item.ItemKey != "mid" || pending[1].Item.ItemKey != "new" {
		t.Fatalf("expected the two newest, oldest first, got %+v", pending)
	}
}

func TestDestinationsDefaultToTest(t *testing.T) {
	store, _ := newTestStore(t)
	if got := store.Destinations(""); len(got) != 1 || got[0].Kind != kindTest {
		t.Fatalf("got %+v", got)
	}
	if len(store.Destinations(kindPublic)) != 0 {
		t.Fatal("no public destination expected")
	}
}

func TestSetDestinationKindAndReaddKeepsKind(t *testing.T) {
	store, _ := newTestStore(t)
	if ok, _ := store.SetDestinationKind("-100123456", kindPublic); !ok {
		t.Fatal("destination not found")
	}
	if ok, _ := store.SetDestinationKind("-100999", kindPublic); ok {
		t.Fatal("unknown destination must report false")
	}
	if _, err := store.SetDestinationKind("-100123456", "other"); err == nil {
		t.Fatal("invalid kind must be rejected")
	}
	store.AddDestination("-100123456", "renamed", "")
	if store.DestinationKinds()["-100123456"] != kindPublic {
		t.Fatal("re-adding must keep the kind")
	}
}

func TestTestAccountNeverReachesPublicDestination(t *testing.T) {
	store, source := newTestStore(t)
	store.AddDestination("-100public", "public group", kindPublic)
	store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, false)
	if !sameSet(chatsOf(store, source.ID), "-100123456") {
		t.Fatalf("got %v", chatsOf(store, source.ID))
	}
}

func TestPublicAccountReachesEverything(t *testing.T) {
	store, _ := newTestStore(t)
	publicSource, _ := store.AddSource("5555555555", "public account", kindPublic)
	store.AddDestination("-100public", "public group", kindPublic)
	store.InsertItems(publicSource.ID, []FeedItem{sampleItem("k")}, false)
	if !sameSet(chatsOf(store, publicSource.ID), "-100123456", "-100public") {
		t.Fatalf("got %v", chatsOf(store, publicSource.ID))
	}
}

func TestSwitchingAccountToTestCancelsPublicDeliveries(t *testing.T) {
	store, _ := newTestStore(t)
	publicSource, _ := store.AddSource("5555555555", "public account", kindPublic)
	store.AddDestination("-100public", "public group", kindPublic)
	store.InsertItems(publicSource.ID, []FeedItem{sampleItem("k")}, false)
	if ok, _ := store.SetSourceKind("5555555555", kindTest); !ok {
		t.Fatal("account not found")
	}
	if !sameSet(chatsOf(store, publicSource.ID), "-100123456") {
		t.Fatalf("got %v", chatsOf(store, publicSource.ID))
	}
}

func TestSwitchingAccountToPublicDoesNotPublishOldItems(t *testing.T) {
	store, source := newTestStore(t)
	store.AddDestination("-100public", "public group", kindPublic)
	store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, false)
	store.SetSourceKind(source.UID, kindPublic)
	if !sameSet(chatsOf(store, source.ID), "-100123456") {
		t.Fatalf("got %v", chatsOf(store, source.ID))
	}
}

func TestNewPublicDestinationSkipsTestAccountItems(t *testing.T) {
	store, source := newTestStore(t)
	publicSource, _ := store.AddSource("5555555555", "public account", kindPublic)
	store.InsertItems(source.ID, []FeedItem{sampleItem("a")}, false)
	store.InsertItems(publicSource.ID, []FeedItem{sampleItem("b")}, false)
	store.AddDestination("-100public", "public group", kindPublic)
	if !sameSet(chatsOf(store, source.ID), "-100123456") {
		t.Fatalf("test account: got %v", chatsOf(store, source.ID))
	}
	if !sameSet(chatsOf(store, publicSource.ID), "-100123456", "-100public") {
		t.Fatalf("public account: got %v", chatsOf(store, publicSource.ID))
	}
}

func TestBackfillRespectsAccountKind(t *testing.T) {
	store, source := newTestStore(t)
	store.AddDestination("-100public", "public group", kindPublic)
	store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, true)
	store.BackfillSkipped(source.ID, 5)
	if !sameSet(chatsOf(store, source.ID), "-100123456") {
		t.Fatalf("got %v", chatsOf(store, source.ID))
	}
}

func TestSendingQueryBlocksPublicEvenWithStaleRecord(t *testing.T) {
	store, source := newTestStore(t)
	store.AddDestination("-100public", "public group", kindPublic)
	store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, false)
	store.mu.Lock()
	post := store.index[postIndexKey(source.ID, "k")]
	post.Deliveries = append(post.Deliveries, &Delivery{ChatID: "-100public", Status: deliveryPending})
	store.mu.Unlock()
	if !sameSet(chatsOf(store, source.ID), "-100123456") {
		t.Fatalf("got %v", chatsOf(store, source.ID))
	}
}

func TestInvalidKindsAreRejected(t *testing.T) {
	store, source := newTestStore(t)
	if _, err := store.AddSource("6666666666", "x", "other"); err == nil {
		t.Fatal("invalid account kind accepted")
	}
	if _, err := store.SetSourceKind(source.UID, "other"); err == nil {
		t.Fatal("invalid account kind accepted")
	}
	if ok, _ := store.SetSourceKind("unknown", kindPublic); ok {
		t.Fatal("unknown account reported as found")
	}
	if _, err := store.AddSource(source.UID, "again", ""); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate UID error expected, got %v", err)
	}
}

func TestDeliveryLifecycleAndBackoff(t *testing.T) {
	store, source := newTestStore(t)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, false)

	store.MarkDeliveryFailed(source.ID, "k", "-100123456", "boom")
	if len(store.PendingDeliveries(source.ID, 10)) != 0 {
		t.Fatal("a failed delivery must wait for its backoff")
	}
	now = now.Add(31 * time.Second)
	if len(store.PendingDeliveries(source.ID, 10)) != 1 {
		t.Fatal("delivery must be due again after 30 seconds")
	}
	store.MarkDeliveryFailed(source.ID, "k", "-100123456", "boom")
	now = now.Add(31 * time.Second)
	if len(store.PendingDeliveries(source.ID, 10)) != 0 {
		t.Fatal("second failure must back off for 60 seconds")
	}
	now = now.Add(31 * time.Second)
	store.MarkDeliverySent(source.ID, "k", "-100123456")
	if len(store.PendingDeliveries(source.ID, 10)) != 0 {
		t.Fatal("sent delivery must not be pending")
	}
	stats := store.Stats()
	if stats.Sent != 1 || stats.Pending != 0 {
		t.Fatalf("unexpected stats %+v", stats)
	}
}

func TestRemoveSourceDeletesItsItems(t *testing.T) {
	store, source := newTestStore(t)
	store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, false)
	if !store.RemoveSource(source.UID) {
		t.Fatal("source not removed")
	}
	if len(store.PendingDeliveries(0, 10)) != 0 || len(store.ListSources(false)) != 0 {
		t.Fatal("everything of the source must be gone")
	}
}

func TestCommentsDue(t *testing.T) {
	store, source := newTestStore(t)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	if !store.CommentsDue(source, time.Minute) {
		t.Fatal("never checked means due")
	}
	store.MarkCommentsChecked(source.ID, "")
	source, _ = store.GetSourceByID(source.ID)
	if store.CommentsDue(source, time.Minute) {
		t.Fatal("must not be due right after a check")
	}
	now = now.Add(61 * time.Second)
	if !store.CommentsDue(source, time.Minute) {
		t.Fatal("must be due after the interval")
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := store.AddSource("1234567890", "name", kindPublic)
	store.AddDestination("-100public", "public group", kindPublic)
	store.InsertItems(source.ID, []FeedItem{sampleItem("k")}, false)
	store.SetPaused(true)

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := reopened.GetSource("1234567890")
	if !ok || loaded.Kind != kindPublic || !reopened.IsPaused() {
		t.Fatalf("source or settings lost: %+v", loaded)
	}
	if !sameSet(chatsOf(reopened, loaded.ID), "-100public") {
		t.Fatalf("pending delivery lost: %v", chatsOf(reopened, loaded.ID))
	}
	if reopened.InsertItems(loaded.ID, []FeedItem{sampleItem("k")}, false) != 0 {
		t.Fatal("known item must stay known after a restart")
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("temporary file must not be left behind")
	}
}

func TestOldPayloadsAreDroppedButKeysStay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, _ := OpenStore(path)
	source, _ := store.AddSource("1234567890", "name", "")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	store.InsertItems(source.ID, []FeedItem{sampleItem("old")}, true)
	now = now.Add(30 * 24 * time.Hour)
	store.InsertItems(source.ID, []FeedItem{sampleItem("fresh")}, true)

	reopened, _ := OpenStore(path)
	if reopened.InsertItems(source.ID, []FeedItem{sampleItem("old")}, false) != 0 {
		t.Fatal("old key must still be known, or an old pinned post would be re-sent")
	}
	if reopened.BackfillSkipped(source.ID, 10) != 1 {
		t.Fatal("only the fresh item still has a payload to backfill")
	}
}

func TestOldSQLiteFileIsRejectedClearly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	os.WriteFile(path, []byte("SQLite format 3\x00binary"), 0o600)
	if _, err := OpenStore(path); err == nil || !strings.Contains(err.Error(), "STATE_PATH") {
		t.Fatalf("expected a clear error, got %v", err)
	}
}
