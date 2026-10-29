import unittest

from models import FeedItem, MediaItem
from storage import Storage


class StorageTests(unittest.TestCase):
    def setUp(self):
        self.storage = Storage(":memory:")
        self.source = self.storage.addSource("1234567890", "测试账号")
        self.storage.addDestination("-100123456", "测试群")
        self.item = FeedItem(
            itemKey="post-key",
            title="测试微博",
            summary="正文内容",
            link="https://weibo.com/123/abc",
            published="2026-10-28T10:00:00+08:00",
            media=[MediaItem("https://example.com/p.jpg", "photo")],
            tags=["#微博动态", "#微博照片"],
        )

    def tearDown(self):
        self.storage.close()

    def test_pending_item_creates_delivery(self):
        self.assertTrue(self.storage.insertItem(self.source.id, self.item))
        pending = self.storage.pendingDeliveries(self.source.id)
        self.assertEqual(len(pending), 1)
        self.assertEqual(pending[0][2], "-100123456")
        self.assertEqual(pending[0][1].tags, ["#微博动态", "#微博照片"])

    def test_baseline_item_is_skipped(self):
        self.assertTrue(self.storage.insertItem(self.source.id, self.item, skip=True))
        self.assertEqual(self.storage.pendingDeliveries(self.source.id), [])

    def test_second_destination_gets_pending_items(self):
        self.storage.insertItem(self.source.id, self.item)
        self.storage.addDestination("@another_channel", "另一个频道")
        pending = self.storage.pendingDeliveries(self.source.id)
        self.assertEqual({entry[2] for entry in pending}, {"-100123456", "@another_channel"})

    def test_remove_destination_cancels_pending(self):
        self.storage.insertItem(self.source.id, self.item)
        self.storage.removeDestination("-100123456")
        self.assertEqual(self.storage.pendingDeliveries(self.source.id), [])

    def test_backfill_promotes_skipped_item(self):
        self.storage.insertItem(self.source.id, self.item, skip=True)
        self.assertEqual(self.storage.backfillSkipped(self.source.id, 1), 1)
        self.assertEqual(len(self.storage.pendingDeliveries(self.source.id)), 1)

    def test_destinations_default_to_test(self):
        self.assertEqual(self.storage.activeDestinations()[0]["kind"], "test")
        self.assertEqual(self.storage.activeDestinations("public"), [])

    def test_set_destination_kind(self):
        self.assertTrue(self.storage.setDestinationKind("-100123456", "public"))
        self.assertEqual(self.storage.destinationKinds(), {"-100123456": "public"})
        self.assertFalse(self.storage.setDestinationKind("-100999", "public"))
        with self.assertRaises(ValueError):
            self.storage.setDestinationKind("-100123456", "other")

    def test_readding_destination_keeps_kind(self):
        self.storage.setDestinationKind("-100123456", "public")
        self.storage.addDestination("-100123456", "renamed")
        self.assertEqual(self.storage.destinationKinds()["-100123456"], "public")

    def test_old_database_migrates_to_test_kind(self):
        import os
        import sqlite3
        import tempfile

        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "old.db")
            old = sqlite3.connect(path)
            old.execute("CREATE TABLE destinations (chatId TEXT PRIMARY KEY, name TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1, addedAt TEXT NOT NULL)")
            old.execute("INSERT INTO destinations VALUES('-1001','old',1,'2026-01-01T00:00:00+00:00')")
            old.commit()
            old.close()
            migrated = Storage(path)
            self.assertEqual(migrated.destinationKinds(), {"-1001": "test"})
            migrated.close()


    def chatsOf(self, sourceId: int) -> set[str]:
        return {entry[2] for entry in self.storage.pendingDeliveries(sourceId)}

    def test_accounts_default_to_test(self):
        self.assertEqual(self.source.kind, "test")

    def test_test_account_never_reaches_public_destination(self):
        self.storage.addDestination("-100public", "public group", "public")
        self.storage.insertItem(self.source.id, self.item)
        self.assertEqual(self.chatsOf(self.source.id), {"-100123456"})

    def test_public_account_reaches_public_and_test_destinations(self):
        publicSource = self.storage.addSource("5555555555", "public account", "public")
        self.storage.addDestination("-100public", "public group", "public")
        self.storage.insertItem(publicSource.id, self.item)
        self.assertEqual(self.chatsOf(publicSource.id), {"-100123456", "-100public"})

    def test_switching_account_to_test_cancels_pending_public_deliveries(self):
        publicSource = self.storage.addSource("5555555555", "public account", "public")
        self.storage.addDestination("-100public", "public group", "public")
        self.storage.insertItem(publicSource.id, self.item)
        self.assertTrue(self.storage.setSourceKind("5555555555", "test"))
        self.assertEqual(self.chatsOf(publicSource.id), {"-100123456"})
        self.assertEqual(self.storage.getSource("5555555555").kind, "test")

    def test_switching_account_to_public_does_not_publish_old_items(self):
        self.storage.addDestination("-100public", "public group", "public")
        self.storage.insertItem(self.source.id, self.item)
        self.storage.setSourceKind(self.source.uid, "public")
        self.assertEqual(self.chatsOf(self.source.id), {"-100123456"})

    def test_new_public_destination_skips_test_account_items(self):
        publicSource = self.storage.addSource("5555555555", "public account", "public")
        otherItem = FeedItem("other", "", "text", "https://weibo.com/1/b", "p", [], [])
        self.storage.insertItem(self.source.id, self.item)
        self.storage.insertItem(publicSource.id, otherItem)
        self.storage.addDestination("-100public", "public group", "public")
        self.assertEqual(self.chatsOf(self.source.id), {"-100123456"})
        self.assertEqual(self.chatsOf(publicSource.id), {"-100123456", "-100public"})

    def test_backfill_respects_account_kind(self):
        self.storage.addDestination("-100public", "public group", "public")
        self.storage.insertItem(self.source.id, self.item, skip=True)
        self.storage.backfillSkipped(self.source.id, 5)
        self.assertEqual(self.chatsOf(self.source.id), {"-100123456"})

    def test_sending_query_blocks_public_even_if_a_stale_row_exists(self):
        self.storage.addDestination("-100public", "public group", "public")
        self.storage.insertItem(self.source.id, self.item)
        self.storage.conn.execute(
            "INSERT INTO deliveries(sourceId,itemKey,chatId,status) VALUES(?,?,?,'pending')",
            (self.source.id, self.item.itemKey, "-100public"),
        )
        self.storage.conn.commit()
        self.assertEqual(self.chatsOf(self.source.id), {"-100123456"})

    def test_invalid_account_kind_is_rejected(self):
        with self.assertRaises(ValueError):
            self.storage.addSource("6666666666", "x", "other")
        with self.assertRaises(ValueError):
            self.storage.setSourceKind(self.source.uid, "other")
        self.assertFalse(self.storage.setSourceKind("unknown", "public"))

    def test_old_sources_table_migrates_to_test_kind(self):
        import os
        import sqlite3
        import tempfile

        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "old.db")
            old = sqlite3.connect(path)
            old.execute(
                "CREATE TABLE sources (id INTEGER PRIMARY KEY AUTOINCREMENT, uid TEXT NOT NULL UNIQUE, name TEXT NOT NULL, "
                "feedUrl TEXT NOT NULL DEFAULT '', active INTEGER NOT NULL DEFAULT 1, initialized INTEGER NOT NULL DEFAULT 0, "
                "lastChecked TEXT, lastError TEXT, createdAt TEXT NOT NULL, commentsInitialized INTEGER NOT NULL DEFAULT 0, "
                "lastCommentsChecked TEXT, lastCommentError TEXT)"
            )
            old.execute("INSERT INTO sources(uid,name,createdAt) VALUES('7198559139','old','2026-01-01T00:00:00+00:00')")
            old.commit()
            old.close()
            migrated = Storage(path)
            self.assertEqual(migrated.getSource("7198559139").kind, "test")
            migrated.close()


if __name__ == "__main__":
    unittest.main()