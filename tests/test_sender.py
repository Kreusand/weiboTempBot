import asyncio
import json
import unittest
from unittest.mock import patch

from telegram.error import TelegramError
from telegram.request._requestparameter import RequestParameter

from models import FeedItem, MediaItem
from sender import TelegramSender


class FakeBot:
    def __init__(self, failMedia: bool = False):
        self.failMedia = failMedia
        self.texts: list[str] = []
        self.uploaded: list[bytes] = []

    async def send_message(self, chat_id, text, disable_web_page_preview=False):
        self.texts.append(text)

    async def send_media_group(self, chat_id, media, **kwargs):
        if self.failMedia:
            raise TelegramError("upload failed")
        # Serialize exactly like the library does: every album member must be referenced by
        # an attach:// entry, otherwise Telegram receives an album without files.
        parameter = RequestParameter.from_input("media", media)
        references = [entry.get("media") for entry in json.loads(parameter.json_value)]
        attached = {"attach://" + file.attach_name for file in (parameter.input_files or [])}
        if len(references) != len(media) or any(reference not in attached for reference in references):
            raise TelegramError("album members are not attached")
        self.uploaded.extend(file.input_file_content for file in parameter.input_files)

    async def send_photo(self, chat_id, photo, caption=None, **kwargs):
        if self.failMedia:
            raise TelegramError("upload failed")
        # Telegram rejects empty uploads with "File must be non-empty".
        if not photo.input_file_content:
            raise TelegramError("File must be non-empty")
        self.uploaded.append(photo.input_file_content)
        self.texts.append(caption or "")


def makeItem(photoCount: int = 1) -> FeedItem:
    return FeedItem(
        itemKey="k",
        title="",
        summary="body text",
        link="https://weibo.com/1/a",
        published="2026-10-28",
        media=[MediaItem(f"https://wx1.sinaimg.cn/{index}.jpg", "photo") for index in range(photoCount)],
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

    def test_album_members_are_attached(self):
        sender = TelegramSender(None, 45_000_000, 10, "")
        bot = FakeBot()

        async def fakeDownload(media, budget):
            return (b"image-" + media.url.encode(), ".jpg")

        with patch.object(sender, "_downloadMedia", fakeDownload):
            asyncio.run(sender.send(bot, "-1001", "name", makeItem(photoCount=3)))
        self.assertEqual(len(bot.uploaded), 3)

    def test_upload_failure_note_contains_reason_for_test_only(self):
        data = (b"x", ".jpg")
        self.assertIn("upload failed", self.runSend(data, True, diagnostics=True))
        self.assertNotIn("upload failed", self.runSend(data, True, diagnostics=False))

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