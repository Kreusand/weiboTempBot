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


if __name__ == "__main__":
    unittest.main()
