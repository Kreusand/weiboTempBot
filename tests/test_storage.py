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


if __name__ == "__main__":
    unittest.main()
