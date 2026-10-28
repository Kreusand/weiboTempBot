import asyncio
import logging
import mimetypes
from urllib.parse import urljoin, urlparse

import aiohttp
from telegram import InputFile, InputMediaPhoto, InputMediaVideo
from telegram.error import TelegramError

from models import FeedItem, MediaItem

logger = logging.getLogger(__name__)
VIDEO_EXTENSIONS = (".mp4", ".m4v", ".mov", ".webm")


class TelegramSender:
    def __init__(self, httpSession: aiohttp.ClientSession, maxMediaBytes: int, maxMediaPerPost: int, weiboCookies: str):
        self.httpSession = httpSession
        self.maxMediaBytes = maxMediaBytes
        self.maxMediaPerPost = maxMediaPerPost
        self.weiboCookies = weiboCookies

    @staticmethod
    async def _sendText(bot, chatId: str, text: str, disablePreview: bool = False) -> None:
        # Telegram text messages are limited to 4096 characters. Split rather than truncate
        # long Weibo posts, and prefer a newline boundary where one is available.
        remaining = text.strip()
        while remaining:
            if len(remaining) <= 3900:
                chunk = remaining
            else:
                boundary = remaining.rfind("\n", 0, 3900)
                if boundary < 2500:
                    boundary = 3900
                chunk = remaining[:boundary].rstrip()
            await bot.send_message(
                chat_id=chatId,
                text=chunk,
                disable_web_page_preview=disablePreview,
            )
            remaining = remaining[len(chunk):].lstrip()

    @staticmethod
    def _messageText(accountName: str, item: FeedItem) -> str:
        typeNames = {"post": "微博动态", "comment": "微博评论", "comment_reply": "微博评论回复"}
        typeName = typeNames.get(item.eventType, "微博更新")
        bodyParts = [f"【{typeName}】{accountName}"]
        if item.tags:
            bodyParts.append(" ".join(item.tags))
        if item.title and item.title not in item.summary:
            bodyParts.append(item.title)
        if item.summary:
            bodyParts.append(item.summary)
        if item.published:
            bodyParts.append(f"时间：{item.published}")
        if item.parentLink:
            bodyParts.append(f"对应微博：{item.parentLink}")
        if item.link:
            bodyParts.append(f"原文：{item.link}")
        return "\n\n".join(bodyParts).strip()

    async def _downloadMedia(self, media: MediaItem, budgetBytes: int) -> tuple[bytes, str] | None:
        timeout = aiohttp.ClientTimeout(total=35)
        currentUrl = media.url
        allowedCookieHosts = ("weibo.cn", "weibo.com", "sinaimg.cn", "weibocdn.com", "sina.com.cn", "sina.com", "sina.cn")
        allowedMediaHosts = ("weibo.cn", "weibo.com", "weibo.com.cn", "sinaimg.cn", "weibocdn.com", "sina.com.cn", "sina.com", "sina.cn")
        for _ in range(6):
            parsedUrl = urlparse(currentUrl)
            host = (parsedUrl.hostname or "").lower()
            if parsedUrl.scheme not in ("http", "https") or not host:
                return None
            if not any(host == root or host.endswith("." + root) for root in allowedMediaHosts):
                logger.warning("跳过不在微博媒体域名白名单中的 URL：%s", currentUrl)
                return None
            headers = {}
            if self.weiboCookies and any(host == root or host.endswith("." + root) for root in allowedCookieHosts):
                headers["Cookie"] = self.weiboCookies
                headers["Referer"] = "https://m.weibo.cn/"
            try:
                async with self.httpSession.get(
                    currentUrl, headers=headers, timeout=timeout, allow_redirects=False
                ) as response:
                    if response.status in (301, 302, 303, 307, 308):
                        location = response.headers.get("Location")
                        if not location:
                            return None
                        # Follow redirects manually so the Weibo Cookie is attached only to an allowlisted host.
                        currentUrl = urljoin(currentUrl, location)
                        continue
                    if response.status >= 400:
                        logger.warning("媒体下载失败 HTTP %s: %s", response.status, currentUrl)
                        return None
                    contentType = response.headers.get("Content-Type", "").split(";", 1)[0].lower()
                    length = response.headers.get("Content-Length")
                    perFileLimit = min(self.maxMediaBytes, budgetBytes, 9_000_000) if media.kind == "photo" else min(self.maxMediaBytes, budgetBytes)
                    urlPath = urlparse(currentUrl).path.lower()
                    if media.kind == "video" and urlPath.endswith(".m3u8"):
                        logger.info("HLS 播放列表不能直接作为视频文件上传，跳过：%s", currentUrl)
                        return None
                    if length and length.isdigit() and int(length) > perFileLimit:
                        logger.info("跳过过大的媒体文件：%s bytes", length)
                        return None
                    if media.kind == "photo" and contentType and not contentType.startswith("image/"):
                        if not urlPath.endswith((".jpg", ".jpeg", ".png", ".webp", ".gif")):
                            logger.info("媒体 URL 未返回图片类型，跳过：%s (%s)", currentUrl, contentType)
                            return None
                    if media.kind == "video" and contentType and not contentType.startswith("video/"):
                        if not urlPath.endswith(VIDEO_EXTENSIONS):
                            logger.info("媒体 URL 不是可直接上传的视频文件，跳过：%s (%s)", currentUrl, contentType)
                            return None
                    buffer = bytearray()
                    async for chunk in response.content.iter_chunked(64 * 1024):
                        buffer.extend(chunk)
                        if len(buffer) > perFileLimit:
                            logger.info("流式下载超过大小上限，跳过媒体：%s", currentUrl)
                            return None
                    if not buffer:
                        return None
                    suffix = mimetypes.guess_extension(contentType) or ""
                    if not suffix:
                        pathSuffix = urlparse(currentUrl).path.rsplit("/", 1)[-1]
                        suffix = "." + pathSuffix.rsplit(".", 1)[-1] if "." in pathSuffix else ".bin"
                    return bytes(buffer), suffix
            except (aiohttp.ClientError, asyncio.TimeoutError, ValueError) as exc:
                logger.warning("媒体下载异常：%s (%s)", currentUrl, exc)
                return None
        logger.warning("媒体重定向过多，停止下载：%s", media.url)
        return None

    async def send(self, bot, chatId: str, accountName: str, item: FeedItem, diagnostics: bool = False) -> None:
        # With diagnostics=False (public destinations) only the post content is ever sent.
        # Notes about failed media downloads or uploads are added only for test destinations.
        text = self._messageText(accountName, item)
        mediaList = item.media[: self.maxMediaPerPost]
        usableMedia: list[tuple[MediaItem, bytes, str]] = []
        failedUrls: list[str] = []

        # Download media first so that the text can be included with the first media caption.
        # Some Weibo image/video hosts reject requests from Telegram's servers, so the bot
        # downloads the resource itself and uploads the bytes when possible.
        mediaBudget = self.maxMediaBytes
        for media in mediaList:
            if mediaBudget <= 0:
                failedUrls.append(media.url)
                continue
            downloaded = await self._downloadMedia(media, mediaBudget)
            if downloaded is None:
                failedUrls.append(media.url)
            else:
                usableMedia.append((media, downloaded[0], downloaded[1]))
                mediaBudget -= len(downloaded[0])

        if not usableMedia:
            if failedUrls and diagnostics:
                text += "\n\n部分媒体无法下载：\n" + "\n".join(failedUrls[:5])
            await self._sendText(bot, chatId, text, disablePreview=False)
            return

        caption = text[:900]
        remainingText = text[900:].strip() if len(text) > 900 else ""
        mediaGroups = [usableMedia[index:index + 10] for index in range(0, len(usableMedia), 10)]
        firstGroup = True
        try:
            for group in mediaGroups:
                built = []
                inputFiles = []
                for media, data, suffix in group:
                    # Build each InputFile exactly once from raw bytes. Building a second one from a
                    # BytesIO that the first already consumed produces an empty upload.
                    inputFile = InputFile(data, filename=f"weibo-media{suffix}")
                    inputFiles.append(inputFile)
                    if media.kind == "video":
                        built.append(InputMediaVideo(media=inputFile, caption=caption if firstGroup and not built else None))
                    else:
                        built.append(InputMediaPhoto(media=inputFile, caption=caption if firstGroup and not built else None))
                if len(built) == 1:
                    media = group[0][0]
                    if media.kind == "video":
                        await bot.send_video(chat_id=chatId, video=inputFiles[0], caption=caption if firstGroup else None)
                    else:
                        await bot.send_photo(chat_id=chatId, photo=inputFiles[0], caption=caption if firstGroup else None)
                else:
                    await bot.send_media_group(chat_id=chatId, media=built)
                firstGroup = False
        except TelegramError as exc:
            # A media URL may produce an unsupported format despite looking like a media link.
            # Fall back to the textual post instead of retrying the whole item forever.
            logger.warning("Telegram 媒体上传失败，退回文字与原文链接：%s", exc)
            fallbackText = text
            if diagnostics:
                fallbackText += "\n\n媒体上传失败，链接如下：\n" + "\n".join(x.url for x in mediaList[:10])
            await self._sendText(bot, chatId, fallbackText, disablePreview=False)
            return

        if remainingText:
            await self._sendText(bot, chatId, remainingText, disablePreview=False)
        if failedUrls and diagnostics:
            await self._sendText(
                bot,
                chatId,
                "以下媒体未能上传，请从原文或链接查看：\n" + "\n".join(failedUrls[:10]),
                disablePreview=True,
            )