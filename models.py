from dataclasses import asdict, dataclass
from typing import Any
import json


@dataclass
class MediaItem:
    url: str
    kind: str  # photo or video


@dataclass
class FeedItem:
    itemKey: str
    title: str
    summary: str
    link: str
    published: str
    media: list[MediaItem]
    tags: list[str]
    eventType: str = "post"
    parentLink: str = ""

    def toJson(self) -> str:
        return json.dumps(asdict(self), ensure_ascii=False)

    @classmethod
    def fromJson(cls, value: str) -> "FeedItem":
        raw: dict[str, Any] = json.loads(value)
        media = [MediaItem(**item) for item in raw.get("media", [])]
        return cls(
            itemKey=str(raw["itemKey"]),
            title=str(raw.get("title", "")),
            summary=str(raw.get("summary", "")),
            link=str(raw.get("link", "")),
            published=str(raw.get("published", "")),
            media=media,
            tags=[str(tag) for tag in raw.get("tags", [])],
            eventType=str(raw.get("eventType", "post")),
            parentLink=str(raw.get("parentLink", "")),
        )


@dataclass
class Source:
    id: int
    uid: str
    name: str
    active: bool
    initialized: bool
    commentsInitialized: bool
    lastChecked: str | None
    lastError: str | None
    lastCommentsChecked: str | None
    lastCommentError: str | None
    # "public" content may reach public destinations, "test" content only reaches test destinations.
    kind: str = "test"