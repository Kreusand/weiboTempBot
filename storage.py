import sqlite3
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from models import FeedItem, Source


def nowIso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


class Storage:
    def __init__(self, dbPath: str):
        self.dbPath = dbPath
        if dbPath != ":memory:":
            Path(dbPath).expanduser().parent.mkdir(parents=True, exist_ok=True)
        self.conn = sqlite3.connect(dbPath, check_same_thread=False, timeout=10)
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.conn.execute("PRAGMA foreign_keys=ON")
        self.conn.execute("PRAGMA busy_timeout=5000")
        self.initSchema()

    def initSchema(self) -> None:
        self.conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS sources (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                uid TEXT NOT NULL UNIQUE,
                name TEXT NOT NULL,
                feedUrl TEXT NOT NULL DEFAULT '',
                active INTEGER NOT NULL DEFAULT 1,
                initialized INTEGER NOT NULL DEFAULT 0,
                lastChecked TEXT,
                lastError TEXT,
                createdAt TEXT NOT NULL,
                commentsInitialized INTEGER NOT NULL DEFAULT 0,
                lastCommentsChecked TEXT,
                lastCommentError TEXT
            );
            CREATE TABLE IF NOT EXISTS posts (
                sourceId INTEGER NOT NULL,
                itemKey TEXT NOT NULL,
                payloadJson TEXT NOT NULL,
                status TEXT NOT NULL DEFAULT 'pending',
                attempts INTEGER NOT NULL DEFAULT 0,
                nextAttemptAt TEXT,
                discoveredAt TEXT NOT NULL,
                sentAt TEXT,
                lastError TEXT,
                PRIMARY KEY (sourceId, itemKey),
                FOREIGN KEY (sourceId) REFERENCES sources(id) ON DELETE CASCADE
            );
            CREATE INDEX IF NOT EXISTS idxPostsPending ON posts(status, nextAttemptAt, discoveredAt);
            CREATE TABLE IF NOT EXISTS destinations (
                chatId TEXT PRIMARY KEY,
                name TEXT NOT NULL,
                active INTEGER NOT NULL DEFAULT 1,
                addedAt TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS deliveries (
                sourceId INTEGER NOT NULL,
                itemKey TEXT NOT NULL,
                chatId TEXT NOT NULL,
                status TEXT NOT NULL DEFAULT 'pending',
                attempts INTEGER NOT NULL DEFAULT 0,
                nextAttemptAt TEXT,
                sentAt TEXT,
                lastError TEXT,
                PRIMARY KEY (sourceId, itemKey, chatId),
                FOREIGN KEY (sourceId, itemKey) REFERENCES posts(sourceId, itemKey) ON DELETE CASCADE
            );
            CREATE INDEX IF NOT EXISTS idxDeliveriesPending ON deliveries(status, nextAttemptAt, sourceId);
            CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
            INSERT OR IGNORE INTO settings(key, value) VALUES('paused', '0');
            """
        )
        # Upgrade databases created by the first RSS-only prototype.
        self.ensureColumn("sources", "commentsInitialized", "INTEGER NOT NULL DEFAULT 0")
        self.ensureColumn("sources", "lastCommentsChecked", "TEXT")
        self.ensureColumn("sources", "lastCommentError", "TEXT")
        self.conn.commit()

    def ensureColumn(self, table: str, column: str, definition: str) -> None:
        columns = {row["name"] for row in self.conn.execute(f"PRAGMA table_info({table})").fetchall()}
        if column not in columns:
            self.conn.execute(f"ALTER TABLE {table} ADD COLUMN {column} {definition}")

    def close(self) -> None:
        self.conn.close()

    def addSource(self, uid: str, name: str) -> Source:
        try:
            cur = self.conn.execute(
                "INSERT INTO sources(uid, name, feedUrl, createdAt) VALUES(?, ?, '', ?)",
                (uid, name, nowIso()),
            )
            self.conn.commit()
        except sqlite3.IntegrityError as exc:
            raise ValueError(f"微博 UID {uid} 已存在") from exc
        return self.getSourceById(cur.lastrowid)  # type: ignore[arg-type]

    def getSourceById(self, sourceId: int) -> Source:
        row = self.conn.execute("SELECT * FROM sources WHERE id=?", (sourceId,)).fetchone()
        if row is None:
            raise KeyError("找不到微博账号")
        return self.sourceFromRow(row)

    def getSource(self, uidOrName: str) -> Source | None:
        row = self.conn.execute(
            "SELECT * FROM sources WHERE uid=? OR name=? ORDER BY id LIMIT 1", (uidOrName, uidOrName)
        ).fetchone()
        return self.sourceFromRow(row) if row else None

    def listSources(self, activeOnly: bool = False) -> list[Source]:
        sql = "SELECT * FROM sources"
        if activeOnly:
            sql += " WHERE active=1"
        sql += " ORDER BY id"
        return [self.sourceFromRow(row) for row in self.conn.execute(sql).fetchall()]

    def removeSource(self, uidOrName: str) -> bool:
        cur = self.conn.execute("DELETE FROM sources WHERE uid=? OR name=?", (uidOrName, uidOrName))
        self.conn.commit()
        return cur.rowcount > 0

    def setSourceActive(self, uidOrName: str, active: bool) -> bool:
        cur = self.conn.execute(
            "UPDATE sources SET active=? WHERE uid=? OR name=?", (1 if active else 0, uidOrName, uidOrName)
        )
        self.conn.commit()
        return cur.rowcount > 0

    def setInitialized(self, sourceId: int, value: bool = True) -> None:
        self.conn.execute("UPDATE sources SET initialized=? WHERE id=?", (1 if value else 0, sourceId))
        self.conn.commit()

    def setCommentsInitialized(self, sourceId: int, value: bool = True) -> None:
        self.conn.execute("UPDATE sources SET commentsInitialized=? WHERE id=?", (1 if value else 0, sourceId))
        self.conn.commit()

    def markChecked(self, sourceId: int, error: str | None = None) -> None:
        self.conn.execute(
            "UPDATE sources SET lastChecked=?, lastError=? WHERE id=?",
            (nowIso(), error[:1000] if error else None, sourceId),
        )
        self.conn.commit()

    def markCommentsChecked(self, sourceId: int, error: str | None = None) -> None:
        self.conn.execute(
            "UPDATE sources SET lastCommentsChecked=?, lastCommentError=? WHERE id=?",
            (nowIso(), error[:1000] if error else None, sourceId),
        )
        self.conn.commit()

    def commentsDue(self, source: Source, intervalSeconds: int) -> bool:
        if not source.lastCommentsChecked:
            return True
        try:
            last = datetime.fromisoformat(source.lastCommentsChecked)
            if last.tzinfo is None:
                last = last.replace(tzinfo=timezone.utc)
            return (datetime.now(timezone.utc) - last.astimezone(timezone.utc)).total_seconds() >= intervalSeconds
        except ValueError:
            return True

    def insertItem(self, sourceId: int, item: FeedItem, skip: bool = False) -> bool:
        status = "skipped" if skip else "pending"
        cur = self.conn.execute(
            "INSERT OR IGNORE INTO posts(sourceId,itemKey,payloadJson,status,discoveredAt) VALUES(?,?,?,?,?)",
            (sourceId, item.itemKey, item.toJson(), status, nowIso()),
        )
        if cur.rowcount == 0:
            return False
        if not skip:
            destinations = self.activeDestinations()
            for destination in destinations:
                self.conn.execute(
                    "INSERT OR IGNORE INTO deliveries(sourceId,itemKey,chatId,status) VALUES(?,?,?,'pending')",
                    (sourceId, item.itemKey, destination["chatId"]),
                )
        self.conn.commit()
        return True

    def backfillSkipped(self, sourceId: int, count: int) -> int:
        rows = self.conn.execute(
            "SELECT itemKey FROM posts WHERE sourceId=? AND status='skipped' ORDER BY discoveredAt DESC LIMIT ?",
            (sourceId, count),
        ).fetchall()
        destinations = self.activeDestinations()
        updated = 0
        for row in rows:
            key = str(row["itemKey"])
            cur = self.conn.execute(
                "UPDATE posts SET status='pending', attempts=0, nextAttemptAt=NULL, lastError=NULL WHERE sourceId=? AND itemKey=? AND status='skipped'",
                (sourceId, key),
            )
            if cur.rowcount:
                updated += 1
                for destination in destinations:
                    self.conn.execute(
                        "INSERT OR REPLACE INTO deliveries(sourceId,itemKey,chatId,status,attempts,nextAttemptAt,sentAt,lastError) VALUES(?,?,?,'pending',0,NULL,NULL,NULL)",
                        (sourceId, key, destination["chatId"]),
                    )
        self.conn.commit()
        return updated

    def addDestination(self, chatId: str, name: str) -> None:
        chatId = chatId.strip()
        if not chatId:
            raise ValueError("Chat ID 不能为空")
        self.conn.execute(
            "INSERT INTO destinations(chatId,name,active,addedAt) VALUES(?,?,1,?) "
            "ON CONFLICT(chatId) DO UPDATE SET name=excluded.name,active=1",
            (chatId, name.strip() or chatId, nowIso()),
        )
        # If there are existing unsent queue items, a newly added target can receive them.
        rows = self.conn.execute("SELECT sourceId,itemKey FROM posts WHERE status='pending'").fetchall()
        for row in rows:
            self.conn.execute(
                "INSERT OR IGNORE INTO deliveries(sourceId,itemKey,chatId,status) VALUES(?,?,?,'pending')",
                (row["sourceId"], row["itemKey"], chatId),
            )
            self.conn.execute(
                "UPDATE deliveries SET status='pending',attempts=0,nextAttemptAt=NULL,lastError=NULL "
                "WHERE sourceId=? AND itemKey=? AND chatId=? AND status='cancelled'",
                (row["sourceId"], row["itemKey"], chatId),
            )
        self.conn.commit()

    def removeDestination(self, chatId: str) -> bool:
        exists = self.conn.execute("SELECT 1 FROM destinations WHERE chatId=?", (chatId,)).fetchone()
        if not exists:
            return False
        self.conn.execute("DELETE FROM destinations WHERE chatId=?", (chatId,))
        self.conn.execute(
            "UPDATE deliveries SET status='cancelled' WHERE chatId=? AND status='pending'", (chatId,)
        )
        self.conn.commit()
        return True

    def activeDestinations(self) -> list[dict[str, str]]:
        rows = self.conn.execute("SELECT chatId,name FROM destinations WHERE active=1 ORDER BY addedAt").fetchall()
        return [{"chatId": str(row["chatId"]), "name": str(row["name"])} for row in rows]

    def allDestinations(self) -> list[dict[str, str]]:
        rows = self.conn.execute("SELECT chatId,name FROM destinations ORDER BY addedAt").fetchall()
        return [{"chatId": str(row["chatId"]), "name": str(row["name"])} for row in rows]

    def pendingDeliveries(self, sourceId: int | None = None, limit: int = 50) -> list[tuple[Source, FeedItem, str]]:
        sql = (
            "SELECT d.sourceId,d.itemKey,d.chatId,p.payloadJson FROM deliveries d "
            "JOIN posts p ON p.sourceId=d.sourceId AND p.itemKey=d.itemKey "
            "JOIN destinations t ON t.chatId=d.chatId AND t.active=1 "
            "WHERE d.status='pending' AND (d.nextAttemptAt IS NULL OR d.nextAttemptAt<=?)"
        )
        params: list[Any] = [nowIso()]
        if sourceId is not None:
            sql += " AND d.sourceId=?"
            params.append(sourceId)
        sql += " ORDER BY p.discoveredAt ASC LIMIT ?"
        params.append(limit)
        rows = self.conn.execute(sql, params).fetchall()
        result = []
        for row in rows:
            try:
                source = self.getSourceById(int(row["sourceId"]))
                item = FeedItem.fromJson(str(row["payloadJson"]))
                result.append((source, item, str(row["chatId"])))
            except (KeyError, ValueError, TypeError):
                continue
        return result

    def markDeliverySent(self, sourceId: int, itemKey: str, chatId: str) -> None:
        self.conn.execute(
            "UPDATE deliveries SET status='sent',sentAt=?,lastError=NULL,nextAttemptAt=NULL WHERE sourceId=? AND itemKey=? AND chatId=?",
            (nowIso(), sourceId, itemKey, chatId),
        )
        remaining = self.conn.execute(
            "SELECT COUNT(*) AS n FROM deliveries WHERE sourceId=? AND itemKey=? AND status='pending'",
            (sourceId, itemKey),
        ).fetchone()["n"]
        if int(remaining) == 0:
            self.conn.execute(
                "UPDATE posts SET status='sent',sentAt=?,lastError=NULL,nextAttemptAt=NULL WHERE sourceId=? AND itemKey=?",
                (nowIso(), sourceId, itemKey),
            )
        self.conn.commit()

    def markDeliveryFailed(self, sourceId: int, itemKey: str, chatId: str, error: str) -> None:
        row = self.conn.execute(
            "SELECT attempts FROM deliveries WHERE sourceId=? AND itemKey=? AND chatId=?",
            (sourceId, itemKey, chatId),
        ).fetchone()
        attempts = int(row["attempts"] if row else 0) + 1
        delay = min(3600, 30 * (2 ** min(attempts - 1, 7)))
        retryAt = (datetime.now(timezone.utc) + timedelta(seconds=delay)).isoformat(timespec="seconds")
        self.conn.execute(
            "UPDATE deliveries SET attempts=?,nextAttemptAt=?,lastError=? WHERE sourceId=? AND itemKey=? AND chatId=?",
            (attempts, retryAt, error[:1000], sourceId, itemKey, chatId),
        )
        self.conn.execute(
            "UPDATE posts SET lastError=? WHERE sourceId=? AND itemKey=?", (error[:1000], sourceId, itemKey)
        )
        self.conn.commit()

    def setPaused(self, paused: bool) -> None:
        self.conn.execute("UPDATE settings SET value=? WHERE key='paused'", ("1" if paused else "0",))
        self.conn.commit()

    def isPaused(self) -> bool:
        row = self.conn.execute("SELECT value FROM settings WHERE key='paused'").fetchone()
        return bool(row and row["value"] == "1")

    def stats(self) -> dict[str, Any]:
        sources = self.conn.execute("SELECT COUNT(*) AS n FROM sources").fetchone()["n"]
        active = self.conn.execute("SELECT COUNT(*) AS n FROM sources WHERE active=1").fetchone()["n"]
        pending = self.conn.execute("SELECT COUNT(*) AS n FROM deliveries WHERE status='pending'").fetchone()["n"]
        sent = self.conn.execute("SELECT COUNT(*) AS n FROM deliveries WHERE status='sent'").fetchone()["n"]
        errors = self.conn.execute(
            "SELECT uid,name,lastChecked,lastError,lastCommentError FROM sources "
            "WHERE lastError IS NOT NULL OR lastCommentError IS NOT NULL ORDER BY lastChecked DESC LIMIT 5"
        ).fetchall()
        return {
            "sources": int(sources),
            "active": int(active),
            "destinations": len(self.activeDestinations()),
            "pending": int(pending),
            "sent": int(sent),
            "errors": [dict(row) for row in errors],
        }

    @staticmethod
    def sourceFromRow(row: sqlite3.Row) -> Source:
        return Source(
            id=int(row["id"]),
            uid=str(row["uid"]),
            name=str(row["name"]),
            active=bool(row["active"]),
            initialized=bool(row["initialized"]),
            commentsInitialized=bool(row["commentsInitialized"]),
            lastChecked=row["lastChecked"],
            lastError=row["lastError"],
            lastCommentsChecked=row["lastCommentsChecked"],
            lastCommentError=row["lastCommentError"],
        )
