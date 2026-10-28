import asyncio
import unittest
from unittest.mock import patch

from telegram.error import TelegramError

from models import FeedItem, MediaItem
from sender import TelegramSender


class FakeBot:
    def __init__(self, failMedia: bool = False):
        self.failMedia = failMedia
        self.texts: list[str] = []
        self.uploaded: list[bytes] = []

    async def send_message(self, chat_id, text, disable_web_page_preview=False):
        self.texts.append(text)

    async def send_photo(self, chat_id, photo, caption=None):
        if self.failMedia:
            raise TelegramError("upload failed")
        # Telegram rejects empty uploads with "File must be non-empty".
        if not photo.input_file_content:
            raise TelegramError("File must be non-empty")
        self.uploaded.append(photo.input_file_content)
        self.texts.append(caption or "")


def makeItem() -> FeedItem:
    return FeedItem(
        itemKey="k",
        title="",
        summary="body text",
        link="https://weibo.com/1/a",
        published="2026-10-28",
        media=[MediaItem("https://wx1.sinaimg.cn/a.jpg", "photo")],
        tags=["#tag"],
    )


class SenderDiagnosticsTests(unittest.TestCase):
    def runSend(self, downloaded, failMedia: bool, diagnostics: bool) -> str:
        sender = TelegramSender(None, 45_000_000, 10, "")
        bot = FakeBot(failMedia)

        async def fakeDownload(media, budget):
            return downloaded

        with patch.object(sender, "_downloadMedia", fakeDownload):
            asyncio.run(sender.send(bot, "-1001", "name", makeItem(), diagnostics=diagnostics))
        return "\n".join(bot.texts)

    def test_single_photo_upload_is_not_empty(self):
        sender = TelegramSender(None, 45_000_000, 10, "")
        bot = FakeBot()

        async def fakeDownload(media, budget):
            return (b"image-bytes", ".jpg")

        with patch.object(sender, "_downloadMedia", fakeDownload):
            asyncio.run(sender.send(bot, "-1001", "name", makeItem()))
        self.assertEqual(bot.uploaded, [b"image-bytes"])
        self.assertNotIn("sinaimg.cn", "\n".join(bot.texts))

    def test_download_failure_note_only_for_test(self):
        publicText = self.runSend(None, False, diagnostics=False)
        testText = self.runSend(None, False, diagnostics=True)
        self.assertIn("body text", publicText)
        self.assertNotIn("sinaimg.cn", publicText)
        self.assertIn("sinaimg.cn", testText)

    def test_upload_failure_note_only_for_test(self):
        data = (b"x", ".jpg")
        publicText = self.runSend(data, True, diagnostics=False)
        testText = self.runSend(data, True, diagnostics=True)
        self.assertIn("body text", publicText)
        self.assertNotIn("sinaimg.cn", publicText)
        self.assertIn("sinaimg.cn", testText)


if __name__ == "__main__":
    unittest.main()