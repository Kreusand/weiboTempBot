from dataclasses import dataclass
import os
from pathlib import Path
from dotenv import load_dotenv

load_dotenv()


def intEnv(name: str, default: int, minimum: int = 1) -> int:
    raw = os.getenv(name, str(default)).strip()
    try:
        value = int(raw)
    except ValueError as exc:
        raise ValueError(f"环境变量 {name} 必须是整数") from exc
    if value < minimum:
        raise ValueError(f"环境变量 {name} 必须大于等于 {minimum}")
    return value


def requiredEnv(name: str) -> str:
    value = os.getenv(name, "").strip()
    if not value or value in {"replace_me", "replace_with_your_token"}:
        raise ValueError(f"请先在 .env 中配置 {name}")
    return value


@dataclass(frozen=True)
class Settings:
    telegramToken: str
    defaultChatId: str
    adminIds: set[int]
    weiboCookies: str
    pollIntervalSeconds: int
    commentsPollIntervalSeconds: int
    requestTimeoutSeconds: int
    maxMediaBytes: int
    maxMediaPerPost: int
    maxCatchupItems: int
    timelinePages: int
    commentsWatchPosts: int
    commentsTopLevelLimit: int
    replyThreadsPerPoll: int
    databasePath: str
    logLevel: str

    @classmethod
    def load(cls) -> "Settings":
        token = requiredEnv("TELEGRAM_BOT_TOKEN")
        adminRaw = os.getenv("ADMIN_IDS", "").strip()
        adminIds: set[int] = set()
        for part in adminRaw.split(","):
            part = part.strip()
            if part:
                try:
                    adminIds.add(int(part))
                except ValueError as exc:
                    raise ValueError("ADMIN_IDS 必须是逗号分隔的 Telegram 数字用户 ID") from exc
        if not adminIds:
            raise ValueError("ADMIN_IDS 至少需要配置一个 Telegram 数字用户 ID")

        cookies = os.getenv("WEIBO_COOKIES", "").strip()
        if not cookies:
            raise ValueError("请先在 .env 中配置 WEIBO_COOKIES（微博移动网页登录 Cookie）")
        defaultChatId = os.getenv("TELEGRAM_CHAT_ID", "").strip()
        databasePath = os.getenv("DATABASE_PATH", "./data/state.db").strip()
        if databasePath != ":memory:":
            Path(databasePath).expanduser().parent.mkdir(parents=True, exist_ok=True)

        return cls(
            telegramToken=token,
            defaultChatId=defaultChatId,
            adminIds=adminIds,
            weiboCookies=cookies,
            pollIntervalSeconds=intEnv("POLL_INTERVAL_SECONDS", 120, 60),
            commentsPollIntervalSeconds=intEnv("COMMENTS_POLL_INTERVAL_SECONDS", 300, 60),
            requestTimeoutSeconds=intEnv("REQUEST_TIMEOUT_SECONDS", 25, 5),
            maxMediaBytes=intEnv("MAX_MEDIA_BYTES", 45_000_000, 1_000_000),
            maxMediaPerPost=intEnv("MAX_MEDIA_PER_POST", 10, 1),
            maxCatchupItems=intEnv("MAX_CATCHUP_ITEMS", 30, 1),
            timelinePages=intEnv("TIMELINE_PAGES", 3, 1),
            commentsWatchPosts=intEnv("COMMENTS_WATCH_POSTS", 20, 1),
            commentsTopLevelLimit=intEnv("COMMENTS_TOPLEVEL_LIMIT", 20, 1),
            replyThreadsPerPoll=intEnv("REPLY_THREADS_PER_POLL", 12, 1),
            databasePath=databasePath,
            logLevel=os.getenv("LOG_LEVEL", "INFO").strip().upper(),
        )