import hashlib
import json
import logging
import re
from datetime import datetime
from urllib.parse import quote, urljoin, urlparse

import aiohttp
from bs4 import BeautifulSoup

from models import FeedItem, MediaItem

logger = logging.getLogger(__name__)

MOBILE_BASE = "https://m.weibo.cn"
MOBILE_API_HEADERS = {
    "MWeibo-Pwa": "1",
    "X-Requested-With": "XMLHttpRequest",
    "Accept": "application/json, text/plain, */*",
    "User-Agent": (
        "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) "
        "AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 "
        "Mobile/15E148 Safari/604.1"
    ),
}
VIDEO_EXTENSIONS = (".mp4", ".m4v", ".mov", ".webm")


class WeiboRequestError(RuntimeError):
    pass


class WeiboAuthError(WeiboRequestError):
    pass


def cleanHtml(value: str) -> str:
    soup = BeautifulSoup(value or "", "html.parser")
    for node in soup(["script", "style", "iframe", "video", "source"]):
        node.decompose()
    return re.sub(r"\s+", " ", soup.get_text(" ", strip=True)).strip()


def textOrEmpty(value) -> str:
    return cleanHtml(str(value or ""))


def userIdOf(user: dict) -> str:
    return str(user.get("idstr") or user.get("id") or "")


def makeStatusLink(uid: str, status: dict) -> str:
    bid = str(status.get("bid") or status.get("mblogid") or "").strip()
    statusId = str(status.get("idstr") or status.get("id") or "").strip()
    if bid:
        return f"https://weibo.com/{uid}/{bid}"
    if statusId:
        return f"https://m.weibo.cn/detail/{statusId}"
    return f"https://weibo.com/u/{uid}"


def statusIdOf(status: dict) -> str:
    return str(status.get("idstr") or status.get("id") or status.get("mid") or "")


def sortNewestFirst(statuses: list[dict]) -> list[dict]:
    """Order statuses newest first.

    Weibo status ids grow with time, so sorting by id also moves a pinned old post
    to its real position instead of leaving it at the top of the timeline.
    """
    def idKey(status: dict) -> int:
        raw = statusIdOf(status)
        return int(raw) if raw.isdigit() else 0

    return sorted(statuses, key=idKey, reverse=True)


def isPinned(status: dict) -> bool:
    """True for a post the account pinned to the top of its timeline."""
    for field in ("isTop", "is_top", "isPinnedCard"):
        if status.get(field) in (1, "1", True):
            return True
    title = status.get("title")
    # "\u7f6e\u9876" is the pinned label Weibo attaches to such posts.
    return isinstance(title, dict) and "\u7f6e\u9876" in str(title.get("text") or "")


def videoPageLink(status: dict, videoMedia: list[MediaItem]) -> str:
    """Return the page link of a video post, or an empty string for any other attachment.

    page_info is also used for topic pages, articles and links, which are not videos.
    """
    pageInfo = status.get("page_info") or {}
    link = str(pageInfo.get("page_url") or "").strip()
    isVideo = str(pageInfo.get("type") or "").lower() == "video" or bool(videoMedia)
    if isVideo and link.startswith(("http://", "https://")):
        return link
    return ""


def dateString(value) -> str:
    if not value:
        return ""
    try:
        if isinstance(value, (int, float)):
            return datetime.fromtimestamp(float(value)).astimezone().isoformat(timespec="seconds")
        return str(value)
    except (ValueError, TypeError, OSError):
        return str(value)


def collectPhotos(status: dict) -> list[MediaItem]:
    result: list[MediaItem] = []
    seen: set[str] = set()
    pics = status.get("pics") or []
    if isinstance(pics, dict):
        pics = list(pics.values())
    for pic in pics:
        if not isinstance(pic, dict):
            continue
        large = pic.get("large") or pic.get("largest") or pic.get("pic_big") or {}
        url = str(large.get("url") or pic.get("url") or pic.get("pic_big") or "").strip()
        if url.startswith("//"):
            url = "https:" + url
        if url.startswith("http://") or url.startswith("https://"):
            if url not in seen:
                result.append(MediaItem(url, "photo"))
                seen.add(url)
    picInfos = status.get("pic_infos") or {}
    if isinstance(picInfos, dict):
        for pic in picInfos.values():
            if not isinstance(pic, dict):
                continue
            large = pic.get("largest") or pic.get("original") or pic.get("large") or {}
            url = str(large.get("url") or "").strip()
            if url.startswith("//"):
                url = "https:" + url
            if url.startswith(("http://", "https://")) and url not in seen:
                result.append(MediaItem(url, "photo"))
                seen.add(url)
    return result


def collectVideo(status: dict) -> list[MediaItem]:
    pageInfo = status.get("page_info") or {}
    mediaInfo = pageInfo.get("media_info") or {}
    candidates = [
        mediaInfo.get("mp4_hd_url"),
        mediaInfo.get("stream_url_hd"),
        mediaInfo.get("stream_url"),
        mediaInfo.get("stream_url_hq"),
        mediaInfo.get("mp4_sd_url"),
    ]
    for candidate in candidates:
        url = str(candidate or "").strip()
        if url.startswith("//"):
            url = "https:" + url
        if not url.startswith(("http://", "https://")):
            continue
        path = url.split("?", 1)[0].lower()
        if path.endswith(VIDEO_EXTENSIONS) or "mp4" in url.split("?", 1)[-1].lower():
            return [MediaItem(url, "video")]
    return []


def statusTags(status: dict, media: list[MediaItem]) -> list[str]:
    tags = ["#微博动态"]
    visible = status.get("visible") or {}
    visibleType = visible.get("type") if isinstance(visible, dict) else status.get("visible_type")
    visibleList = visible.get("list_id") if isinstance(visible, dict) else None
    restrictedFlag = status.get("is_fans_only") or status.get("fans_only") or status.get("isFollowersOnly")
    if restrictedFlag or (visibleType not in (None, 0, "0")) or visibleList:
        tags.append("#微博粉丝可见动态")
    if status.get("retweeted_status"):
        tags.append("#微博转发")
    pageInfo = status.get("page_info") or {}
    mediaInfo = pageInfo.get("media_info") or {}
    hasVideo = any(item.kind == "video" for item in media) or str(pageInfo.get("type") or "").lower() == "video" or bool(
        mediaInfo.get("stream_url") or mediaInfo.get("mp4_hd_url") or mediaInfo.get("stream_url_hd")
    )
    if hasVideo:
        tags.append("#微博视频")
    try:
        photoCount = int(status.get("pic_num") or 0)
    except (TypeError, ValueError):
        photoCount = 0
    hasPhoto = any(item.kind == "photo" for item in media) or photoCount > 0
    if hasPhoto:
        tags.append("#微博照片")
    return tags


class WeiboClient:
    def __init__(self, session: aiohttp.ClientSession, cookies: str, timeoutSeconds: int, timelinePages: int,
                 watchPosts: int, commentsTopLevelLimit: int, replyThreadsPerPoll: int):
        self.session = session
        self.cookies = cookies
        self.timeout = aiohttp.ClientTimeout(total=timeoutSeconds)
        self.timelinePages = timelinePages
        self.watchPosts = watchPosts
        self.commentsTopLevelLimit = commentsTopLevelLimit
        self.replyThreadsPerPoll = replyThreadsPerPoll
        self.fullTextCache: dict[str, str] = {}

    async def getJson(self, path: str, params: dict | None = None, referer: str = "https://m.weibo.cn/") -> dict:
        url = path if path.startswith("http") else MOBILE_BASE + path
        queryParams = dict(params or {})
        allowedApiHosts = ("weibo.cn", "weibo.com", "weibo.com.cn")
        for _ in range(6):
            parsedUrl = urlparse(url)
            host = (parsedUrl.hostname or "").lower()
            if parsedUrl.scheme not in ("http", "https") or not any(
                host == root or host.endswith("." + root) for root in allowedApiHosts
            ):
                raise WeiboRequestError("微博接口重定向到非微博域名，已停止请求")
            headers = {**MOBILE_API_HEADERS, "Cookie": self.cookies, "Referer": referer}
            async with self.session.get(
                url, params=queryParams, headers=headers, timeout=self.timeout, allow_redirects=False
            ) as response:
                if response.status in (301, 302, 303, 307, 308):
                    location = response.headers.get("Location")
                    if not location:
                        raise WeiboRequestError("微博接口返回无 Location 的重定向")
                    url = urljoin(url, location)
                    queryParams = {}
                    continue
                text = await response.text(errors="replace")
                if response.status >= 400:
                    if response.status in (401, 403, 432):
                        raise WeiboAuthError(f"微博接口返回 HTTP {response.status}；登录态可能失效或被限制")
                    raise WeiboRequestError(f"微博接口返回 HTTP {response.status}: {text[:200]}")
                try:
                    data = json.loads(text)
                except Exception as exc:
                    lowerText = text.lower()
                    if "passport" in str(response.url) or "login" in str(response.url) or "passport.weibo.cn" in lowerText:
                        raise WeiboAuthError("微博接口跳转到登录页；请更新 WEIBO_COOKIES") from exc
                    raise WeiboRequestError(f"微博接口没有返回 JSON：{text[:200]}") from exc
                if isinstance(data, dict) and (data.get("ok") == -100 or data.get("retcode") in (6102, 100001)):
                    raise WeiboAuthError("微博登录态失效或接口要求重新登录，请更新 WEIBO_COOKIES")
                return data if isinstance(data, dict) else {"raw": data}
        raise WeiboRequestError("微博接口重定向次数过多")

    async def checkLogin(self) -> dict:
        data = await self.getJson("/api/config")
        payload = data.get("data") or {}
        login = payload.get("login")
        uid = str(payload.get("uid") or payload.get("user", {}).get("id") or "")
        if login in (False, "False", 0, "0"):
            raise WeiboAuthError("微博接口返回未登录状态，请重新复制移动网页 Cookie")
        return {"uid": uid, "login": login, "data": payload}

    async def getTimeline(self, uid: str) -> tuple[str, list[dict]]:
        index = await self.getJson("/api/container/getIndex", {"type": "uid", "value": uid}, f"{MOBILE_BASE}/u/{uid}")
        payload = index.get("data") or {}
        userInfo = payload.get("userInfo") or {}
        name = str(userInfo.get("screen_name") or uid)
        tabs = (payload.get("tabsInfo") or {}).get("tabs") or []
        containerId = ""
        for tab in tabs:
            if tab.get("tab_type") == "weibo" and tab.get("containerid"):
                containerId = str(tab["containerid"])
                break
        if not containerId:
            containerId = f"107603{uid}"

        statuses: list[dict] = []
        seen: set[str] = set()
        for page in range(1, self.timelinePages + 1):
            result = await self.getJson(
                "/api/container/getIndex",
                {"type": "uid", "value": uid, "containerid": containerId, "page": page},
                f"{MOBILE_BASE}/u/{uid}",
            )
            data = result.get("data") or {}
            cards = data.get("cards") or []
            if not cards and page == 1:
                # A few response versions require only containerid, without uid/type.
                result = await self.getJson(
                    "/api/container/getIndex", {"containerid": containerId, "page": page}, f"{MOBILE_BASE}/u/{uid}"
                )
                cards = (result.get("data") or {}).get("cards") or []
            found = 0
            for card in cards:
                for status in self._statusesFromCard(card):
                    statusId = statusIdOf(status)
                    if statusId and statusId not in seen:
                        seen.add(statusId)
                        statuses.append(status)
                        found += 1
            if found == 0:
                break
        if not statuses and (index.get("ok") == 0 or not payload):
            message = str(index.get("msg") or index.get("message") or "没有返回用户时间线")
            raise WeiboRequestError(f"读取 UID {uid} 失败：{message}")
        # Most recent first. This order is convenient for comment monitoring.
        return name, statuses

    @staticmethod
    def _statusesFromCard(card: dict) -> list[dict]:
        result: list[dict] = []
        if isinstance(card.get("mblog"), dict):
            if str(card.get("profile_type_id") or "").startswith("proweibotop"):
                card["mblog"]["isPinnedCard"] = True
            result.append(card["mblog"])
        for child in card.get("card_group") or []:
            if isinstance(child, dict) and isinstance(child.get("mblog"), dict):
                result.append(child["mblog"])
        return result

    async def toFeedItem(self, uid: str, status: dict) -> FeedItem:
        statusId = statusIdOf(status)
        if not statusId:
            statusId = hashlib.sha256(repr(status).encode("utf-8", errors="ignore")).hexdigest()[:24]
        text = textOrEmpty(status.get("text") or status.get("text_raw") or "")
        if status.get("isLongText") and len(text) < 500:
            cachedText = self.fullTextCache.get(statusId)
            if cachedText:
                text = cachedText
            else:
                try:
                    full = await self.getJson("/statuses/extend", {"id": statusId}, f"{MOBILE_BASE}/detail/{statusId}")
                    fullText = (full.get("data") or {}).get("longTextContent") or (full.get("data") or {}).get("text")
                    if fullText:
                        text = textOrEmpty(fullText)
                        self.fullTextCache[statusId] = text
                        if len(self.fullTextCache) > 500:
                            self.fullTextCache.pop(next(iter(self.fullTextCache)))
                except WeiboRequestError as exc:
                    logger.debug("读取长微博正文失败，保留时间线文本：%s", exc)

        media = collectPhotos(status)
        videoMedia = collectVideo(status)
        if videoMedia:
            media = [item for item in media if item.kind != "video"] + videoMedia
        retweeted = status.get("retweeted_status")
        if isinstance(retweeted, dict):
            originalText = textOrEmpty(retweeted.get("text") or retweeted.get("text_raw") or "")
            originalUser = (retweeted.get("user") or {}).get("screen_name") or "原作者"
            if originalText:
                text = f"{text}\n\n转发内容 @{originalUser}：\n{originalText}" if text else f"转发内容 @{originalUser}：\n{originalText}"
            retweetMedia = collectPhotos(retweeted) + collectVideo(retweeted)
            known = {item.url for item in media}
            media.extend(item for item in retweetMedia if item.url not in known)
        userInfo = status.get("user") or {}
        author = str(userInfo.get("screen_name") or uid)
        title = author + " 的微博"
        link = makeStatusLink(uid, status)
        videoPage = videoPageLink(status, videoMedia)
        if videoPage and videoPage not in text:
            text = (text + "\n\n视频页面：" + videoPage).strip()
        published = dateString(status.get("created_at"))
        tags = statusTags(status, media)
        key = hashlib.sha256(f"post:{uid}:{statusId}".encode()).hexdigest()
        return FeedItem(key, title, text or "（微博正文为空，请查看原文链接。）", link, published, media, tags, "post")

    async def fetchCommentEvents(self, uid: str, statuses: list[dict]) -> list[FeedItem]:
        result: list[FeedItem] = []
        threadCount = 0
        for status in statuses[: self.watchPosts]:
            postId = statusIdOf(status)
            if not postId:
                continue
            postUser = status.get("user") or {}
            postUserId = userIdOf(postUser)
            targetName = str(postUser.get("screen_name") or "")
            # Only inspect comment threads attached to posts authored by the monitored UID.
            # If the API omitted the author ID, skip rather than risk scanning another user's post.
            if postUserId != uid:
                continue
            postText = textOrEmpty(status.get("text") or status.get("text_raw") or "")
            postLink = makeStatusLink(uid, status)
            try:
                comments = await self._fetchTopComments(postId)
            except WeiboAuthError:
                raise
            except WeiboRequestError as exc:
                logger.warning("读取微博评论失败 post=%s: %s", postId, exc)
                continue
            for comment in comments[: self.commentsTopLevelLimit]:
                if not isinstance(comment, dict):
                    continue
                commentUser = comment.get("user") or {}
                commentId = str(comment.get("idstr") or comment.get("id") or "")
                if not commentId:
                    continue
                if self.isTargetUser(commentUser, uid, targetName):
                    event = self._commentItem(uid, status, comment, postText, postLink, False)
                    if event:
                        result.append(event)

                nestedReplies = self._nestedReplies(comment)
                nestedIds = set()
                for reply in nestedReplies:
                    replyId = str(reply.get("idstr") or reply.get("id") or "")
                    if replyId:
                        nestedIds.add(replyId)
                    if self.isTargetUser(reply.get("user") or {}, uid, targetName):
                        event = self._commentItem(uid, status, reply, postText, postLink, True, comment)
                        if event:
                            result.append(event)

                total = comment.get("total_number") or comment.get("reply_count") or 0
                try:
                    total = int(total)
                except (ValueError, TypeError):
                    total = 0
                if total > len(nestedReplies) and threadCount < self.replyThreadsPerPoll:
                    threadCount += 1
                    try:
                        replies = await self._fetchChildReplies(postId, commentId)
                        for reply in replies:
                            replyId = str(reply.get("idstr") or reply.get("id") or "")
                            if replyId in nestedIds or not replyId:
                                continue
                            if self.isTargetUser(reply.get("user") or {}, uid, targetName):
                                event = self._commentItem(uid, status, reply, postText, postLink, True, comment)
                                if event:
                                    result.append(event)
                    except WeiboRequestError as exc:
                        logger.warning("读取评论回复失败 post=%s comment=%s: %s", postId, commentId, exc)
        unique: dict[str, FeedItem] = {}
        for item in result:
            unique[item.itemKey] = item
        return list(unique.values())

    async def _fetchTopComments(self, postId: str) -> list[dict]:
        params = {"id": postId, "page": 1}
        try:
            data = await self.getJson("/api/comments/show", params, f"{MOBILE_BASE}/detail/{postId}")
            comments = self._extractCommentList(data)
            if comments:
                return comments
        except WeiboAuthError:
            raise
        except WeiboRequestError:
            pass
        data = await self.getJson(
            "/comments/hotflow",
            {"id": postId, "mid": postId, "max_id": 0, "max_id_type": 0},
            f"{MOBILE_BASE}/detail/{postId}",
        )
        return self._extractCommentList(data)

    async def _fetchChildReplies(self, postId: str, commentId: str) -> list[dict]:
        data = await self.getJson(
            "/comments/hotFlowChild",
            {"cid": commentId, "max_id": 0, "max_id_type": 0},
            f"{MOBILE_BASE}/detail/{postId}?cid={commentId}",
        )
        payload = data.get("data")
        if isinstance(payload, dict):
            replies = payload.get("data") or payload.get("comments") or []
        elif isinstance(payload, list):
            replies = payload
        else:
            replies = []
        return [item for item in replies if isinstance(item, dict)]

    @staticmethod
    def _extractCommentList(data: dict) -> list[dict]:
        payload = data.get("data")
        if isinstance(payload, list):
            items = payload
        elif isinstance(payload, dict):
            items = payload.get("data") or payload.get("comments") or payload.get("comment_list") or []
            if isinstance(items, dict):
                items = items.get("data") or []
        else:
            items = []
        return [item for item in items if isinstance(item, dict)] if isinstance(items, list) else []

    @staticmethod
    def _nestedReplies(comment: dict) -> list[dict]:
        replies = comment.get("comments")
        if not isinstance(replies, list):
            replies = comment.get("comment")
        if not isinstance(replies, list):
            replies = []
        return [reply for reply in replies if isinstance(reply, dict)]

    @staticmethod
    def isTargetUser(user: dict, uid: str, targetName: str) -> bool:
        userId = userIdOf(user)
        if userId:
            return userId == uid
        return bool(targetName and str(user.get("screen_name") or "") == targetName)

    @staticmethod
    def _commentItem(uid: str, status: dict, comment: dict, postText: str, postLink: str,
                     isReply: bool, parent: dict | None = None) -> FeedItem | None:
        commentId = str(comment.get("idstr") or comment.get("id") or "")
        if not commentId:
            return None
        commentText = textOrEmpty(comment.get("text_raw") or comment.get("text") or comment.get("reply_original_text") or "")
        commentUser = comment.get("user") or {}
        commentName = str(commentUser.get("screen_name") or uid)
        if not commentText:
            commentText = "（评论内容为空或仅包含表情）"
        label = "#微博评论回复" if isReply else "#微博评论"
        tags = [label]
        parentName = ""
        if isReply and parent:
            parentName = str((parent.get("user") or {}).get("screen_name") or "用户")
        summary = f"{commentName}：{commentText}"
        if parentName:
            summary += f"\n回复 @{parentName}"
        if postText:
            summary += f"\n\n对应微博：{postText[:700]}"
        published = dateString(comment.get("created_at"))
        link = f"https://m.weibo.cn/detail/{statusIdOf(status)}?cid={quote(commentId)}"
        keyPrefix = "reply" if isReply else "comment"
        key = hashlib.sha256(f"{keyPrefix}:{uid}:{statusIdOf(status)}:{commentId}".encode()).hexdigest()
        title = f"{commentName} 的评论回复" if isReply else f"{commentName} 的评论"
        return FeedItem(key, title, summary, link, published, [], tags, "comment_reply" if isReply else "comment", postLink)