import asyncio
import functools
import logging
import re
import sys
import time
from datetime import datetime, timezone

import aiohttp
from telegram import Update
from telegram.error import TelegramError
from telegram.ext import Application, CommandHandler, ContextTypes

from config import Settings
from sender import TelegramSender
from storage import Storage
from weibo_client import WeiboAuthError, WeiboClient, WeiboRequestError

settings = Settings.load()
logging.basicConfig(
    level=getattr(logging, settings.logLevel, logging.INFO),
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)
# httpx logs every request URL at INFO level, and Telegram URLs contain the bot token.
for noisyLogger in ("httpx", "httpcore"):
    logging.getLogger(noisyLogger).setLevel(logging.WARNING)
logger = logging.getLogger("weibo-tg-bot")


alertCooldownSeconds = 1800


def adminOnly(func):
    """Admin commands work only in a private chat with an admin, or inside a test destination.

    Anywhere else (public destinations, unknown groups) they are silently ignored, so command
    replies, which can contain errors and internal state, never appear in a public chat.
    """

    @functools.wraps(func)
    async def wrapped(update: Update, context: ContextTypes.DEFAULT_TYPE):
        user = update.effective_user
        chat = update.effective_chat
        message = update.effective_message
        if user is None or chat is None or message is None:
            return
        isPrivate = chat.type == "private"
        if user.id not in settings.adminIds:
            if isPrivate:
                await message.reply_text("This command is not available.")
            return
        if not isPrivate and not isTestChat(context, chat):
            return
        return await func(update, context)
    return wrapped


def isTestChat(context: ContextTypes.DEFAULT_TYPE, chat) -> bool:
    testIds = {item["chatId"].lower() for item in store(context).activeDestinations("test")}
    if str(chat.id) in testIds:
        return True
    return chat.username is not None and "@" + chat.username.lower() in testIds


async def notifyTest(application: Application, key: str, text: str) -> None:
    """Send an error alert to test destinations only, at most once per key per cooldown period."""
    alertTimes: dict[str, float] = application.bot_data.setdefault("alertTimes", {})
    now = time.monotonic()
    lastSent = alertTimes.get(key)
    if lastSent is not None and now - lastSent < alertCooldownSeconds:
        return
    alertTimes[key] = now
    storage: Storage = application.bot_data["storage"]
    for destination in storage.activeDestinations("test"):
        try:
            await application.bot.send_message(chat_id=destination["chatId"], text=text[:3900])
        except Exception:
            logger.warning("Could not deliver an alert to test destination %s", destination["chatId"])


def store(context: ContextTypes.DEFAULT_TYPE) -> Storage:
    return context.application.bot_data["storage"]


def sourceListText(sources) -> str:
    if not sources:
        return "目前没有监控账号。使用 /add 微博UID 名称 添加。"
    lines = ["监控账号："]
    for source in sources:
        state = "运行中" if source.active else "已停用"
        postInit = "微博基线已建立" if source.initialized else "待建立微博基线"
        commentInit = "评论基线已建立" if source.commentsInitialized else "待建立评论基线"
        lastCheck = source.lastChecked or "未检查"
        lines.append(f"• {source.name}（{source.uid}）：{state}；{postInit}；{commentInit}；最近检查 {lastCheck}")
        if source.lastError:
            lines.append(f"  动态错误：{source.lastError[:180]}")
        if source.lastCommentError:
            lines.append(f"  评论错误：{source.lastCommentError[:180]}")
    return "\n".join(lines)


async def deliverPending(application: Application, sourceId: int | None = None, limit: int = 40) -> tuple[int, int]:
    lock = application.bot_data.get("deliveryLock")
    if lock is None:
        return await deliverPendingWorker(application, sourceId, limit)
    async with lock:
        return await deliverPendingWorker(application, sourceId, limit)


async def deliverPendingWorker(application: Application, sourceId: int | None, limit: int) -> tuple[int, int]:
    storage: Storage = application.bot_data["storage"]
    sender: TelegramSender = application.bot_data["sender"]
    delivered = 0
    failed = 0
    kinds = storage.destinationKinds()
    for source, item, chatId in storage.pendingDeliveries(sourceId, limit):
        try:
            # Only test destinations receive diagnostic notes; public ones get content only.
            await sender.send(application.bot, chatId, source.name, item, diagnostics=kinds.get(chatId) == "test")
            storage.markDeliverySent(source.id, item.itemKey, chatId)
            delivered += 1
            logger.info("推送成功 uid=%s type=%s chat=%s key=%s", source.uid, item.eventType, chatId, item.itemKey[:12])
        except Exception as exc:
            storage.markDeliveryFailed(source.id, item.itemKey, chatId, str(exc))
            failed += 1
            logger.exception("推送失败 uid=%s type=%s chat=%s", source.uid, item.eventType, chatId)
            await notifyTest(
                application,
                f"deliver:{chatId}",
                f"[ERROR] Delivery to {chatId} failed for {source.name} ({item.eventType}): {exc}",
            )
    return delivered, failed


async def pollSource(application: Application, source, forceComments: bool = False) -> str:
    lock = application.bot_data.get("pollLock")
    if lock is None:
        return await pollSourceWorker(application, source, forceComments)
    async with lock:
        return await pollSourceWorker(application, source, forceComments)


async def pollSourceWorker(application: Application, source, forceComments: bool = False) -> str:
    storage: Storage = application.bot_data["storage"]
    client: WeiboClient = application.bot_data["weiboClient"]
    try:
        name, statuses = await client.getTimeline(source.uid)
        if name and source.name == source.uid:
            # Replace the default UID label after the first successful profile read.
            storage.conn.execute("UPDATE sources SET name=? WHERE id=?", (name, source.id))
            storage.conn.commit()
            source = storage.getSourceById(source.id)

        parsedItems = []
        for status in statuses:
            parsedItems.append(await client.toFeedItem(source.uid, status))

        timelineCount = 0
        if not source.initialized:
            for item in parsedItems:
                storage.insertItem(source.id, item, skip=True)
            storage.setInitialized(source.id, True)
            logger.info("账号 %s 建立微博基线：%s 条", source.uid, len(parsedItems))
        else:
            for item in reversed(parsedItems):
                if storage.insertItem(source.id, item, skip=False):
                    timelineCount += 1

        commentCount = 0
        if forceComments or storage.commentsDue(source, settings.commentsPollIntervalSeconds):
            try:
                commentEvents = await client.fetchCommentEvents(source.uid, statuses)
                if not source.commentsInitialized:
                    for item in commentEvents:
                        storage.insertItem(source.id, item, skip=True)
                    storage.setCommentsInitialized(source.id, True)
                    logger.info("账号 %s 建立评论基线：%s 条", source.uid, len(commentEvents))
                else:
                    for item in commentEvents:
                        if storage.insertItem(source.id, item, skip=False):
                            commentCount += 1
                storage.markCommentsChecked(source.id)
            except WeiboAuthError as exc:
                storage.markCommentsChecked(source.id, str(exc))
                logger.warning("账号 %s 评论读取登录态错误：%s", source.uid, exc)
                await notifyTest(application, f"comments:{source.uid}", f"[ERROR] Comment auth problem for {source.name} ({source.uid}): {exc}")
            except Exception as exc:
                storage.markCommentsChecked(source.id, str(exc))
                logger.exception("账号 %s 评论扫描失败", source.uid)
                await notifyTest(application, f"comments:{source.uid}", f"[ERROR] Comment scan failed for {source.name} ({source.uid}): {exc}")

        storage.markChecked(source.id)
        sentCount, failedCount = await deliverPending(application, source.id, limit=max(20, settings.maxCatchupItems))
        return (
            f"{source.name}：读取 {len(statuses)} 条微博，发现 {timelineCount} 条新动态、{commentCount} 条新评论/回复；"
            f"发送 {sentCount} 条，失败 {failedCount} 条。"
        )
    except Exception as exc:
        storage.markChecked(source.id, str(exc))
        logger.exception("账号 %s 轮询失败", source.uid)
        raise


async def pollLoop(application: Application) -> None:
    storage: Storage = application.bot_data["storage"]
    while True:
        try:
            if not storage.isPaused():
                for source in storage.listSources(activeOnly=True):
                    try:
                        result = await pollSource(application, source)
                        logger.info(result)
                    except Exception as exc:
                        logger.warning("账号 %s 本轮跳过：%s", source.uid, exc)
                        await notifyTest(application, f"poll:{source.uid}", f"[ERROR] Polling failed for {source.name} ({source.uid}): {exc}")
                await deliverPending(application, limit=50)
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception("同步循环出现未处理错误")
        await asyncio.sleep(settings.pollIntervalSeconds)


async def postInit(application: Application) -> None:
    storage = Storage(settings.databasePath)
    if settings.defaultChatId:
        storage.addDestination(settings.defaultChatId, "默认 Telegram 目标")
    timeout = aiohttp.ClientTimeout(total=settings.requestTimeoutSeconds)
    session = aiohttp.ClientSession(timeout=timeout, headers={"User-Agent": "WeiboTelegramSyncBot/2.0"})
    application.bot_data["storage"] = storage
    application.bot_data["httpSession"] = session
    application.bot_data["weiboClient"] = WeiboClient(
        session,
        settings.weiboCookies,
        settings.requestTimeoutSeconds,
        settings.timelinePages,
        settings.commentsWatchPosts,
        settings.commentsTopLevelLimit,
        settings.replyThreadsPerPoll,
    )
    application.bot_data["sender"] = TelegramSender(
        session, settings.maxMediaBytes, settings.maxMediaPerPost, settings.weiboCookies
    )
    application.bot_data["pollLock"] = asyncio.Lock()
    application.bot_data["deliveryLock"] = asyncio.Lock()
    application.bot_data["pollTask"] = asyncio.create_task(pollLoop(application), name="weibo-poll-loop")
    logger.info(
        "Bot 已启动；微博轮询间隔 %s 秒，评论轮询间隔 %s 秒，监控账号 %s 个，目标群组 %s 个。",
        settings.pollIntervalSeconds,
        settings.commentsPollIntervalSeconds,
        len(storage.listSources()),
        len(storage.activeDestinations()),
    )


async def postShutdown(application: Application) -> None:
    task = application.bot_data.get("pollTask")
    if task:
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass
    session = application.bot_data.get("httpSession")
    if session:
        await session.close()
    storage = application.bot_data.get("storage")
    if storage:
        storage.close()


@adminOnly
async def startCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    await update.effective_message.reply_text(
        "微博 → Telegram 同步 Bot 已运行。\n使用 /help 查看命令。\n"
        "首次添加账号只建立动态和评论的基线，不会推送已有历史内容。"
    )


@adminOnly
async def helpCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    await update.effective_message.reply_text(
        "监控账号：\n"
        "/add <UID> [名称] — 添加微博账号\n"
        "/remove <UID或名称> — 删除微博账号\n"
        "/list — 查看账号、状态及错误\n"
        "/check [UID或名称] — 立即检查一个或全部账号\n"
        "/backfill <UID或名称> [条数] — 补发首次基线中最近的微博/事件\n"
        "/disable <UID或名称>、/enable <UID或名称> — 停用/启用账号\n\n"
        "Telegram 目标：\n"
        "/destadd <ChatID或@频道> [名称] — 增加推送群组/频道\n"
        "/destremove <ChatID或@频道> — 移除目标\n"
        "/destset <ChatID或@频道> <public|test> — 设置目标类型（新目标默认 test）\n"
        "/destlist — 列出推送目标\n"
        "/test — 向所有 test 目标发送测试消息\n\n"
        "public 目标只收到微博内容；错误、测试消息和诊断信息只发往 test 目标。\n"
        "管理命令只在与管理员的私聊或 test 目标中生效。\n\n"
        "其他：\n"
        "/me — 检查微博登录状态\n"
        "/pause、/resume — 暂停/恢复自动同步\n"
        "/status — 查看运行状态和队列\n"
        "/help — 显示帮助\n\n"
        "微博 Cookie 仅从服务器 .env 读取，Bot 不提供在 Telegram 中粘贴 Cookie 的命令。"
    )


@adminOnly
async def meCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    client: WeiboClient = context.application.bot_data["weiboClient"]
    try:
        profile = await client.checkLogin()
        uid = profile.get("uid") or "接口未返回 UID"
        login = profile.get("login")
        await update.effective_message.reply_text(
            f"微博接口可访问。login={login!r}；当前账号 UID={uid}。\n"
            "这只能确认基础登录态；目标粉丝可见动态是否可读，还需使用 /check 实测。"
        )
    except Exception as exc:
        await update.effective_message.reply_text(f"微博登录检查失败：{exc}")


@adminOnly
async def addCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if not context.args:
        await update.effective_message.reply_text("用法：/add <微博UID> [名称]")
        return
    uid = context.args[0].strip()
    if not re.fullmatch(r"\d{4,30}", uid):
        await update.effective_message.reply_text("UID 应是数字形式的微博用户 ID。")
        return
    name = " ".join(context.args[1:]).strip() or uid
    try:
        source = store(context).addSource(uid, name)
    except ValueError as exc:
        await update.effective_message.reply_text(str(exc))
        return
    await update.effective_message.reply_text(
        f"已添加微博账号：{source.name}（UID {uid}）。\n"
        "下一步执行 /check " + uid + "，首次检查只记录现有微博及评论作为基线。"
    )


@adminOnly
async def removeCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if not context.args:
        await update.effective_message.reply_text("用法：/remove <UID或名称>")
        return
    key = " ".join(context.args).strip()
    ok = store(context).removeSource(key)
    await update.effective_message.reply_text("已删除账号及其本地状态。" if ok else "没有找到这个微博账号。")


@adminOnly
async def listCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    await update.effective_message.reply_text(sourceListText(store(context).listSources()))


@adminOnly
async def checkCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    storage = store(context)
    if context.args:
        key = " ".join(context.args).strip()
        source = storage.getSource(key)
        if source is None:
            await update.effective_message.reply_text("没有找到该微博账号。用 /list 查看。")
            return
        sources = [source]
    else:
        sources = storage.listSources(activeOnly=True)
    if not sources:
        await update.effective_message.reply_text("没有启用的微博账号。先用 /add 添加。")
        return
    results = []
    for source in sources:
        try:
            results.append(await pollSource(context.application, source, forceComments=True))
        except Exception as exc:
            results.append(f"{source.name}（{source.uid}）：检查失败：{exc}")
    await update.effective_message.reply_text("\n\n".join(results)[:3900])


@adminOnly
async def backfillCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if not context.args:
        await update.effective_message.reply_text("用法：/backfill <UID或名称> [条数]")
        return
    source = store(context).getSource(context.args[0].strip())
    if source is None:
        await update.effective_message.reply_text("没有找到该账号。")
        return
    try:
        count = int(context.args[1]) if len(context.args) > 1 else 5
        count = max(1, min(count, 100))
    except ValueError:
        await update.effective_message.reply_text("条数必须是 1 到 100 的整数。")
        return
    requeued = store(context).backfillSkipped(source.id, count)
    sent, failed = await deliverPending(context.application, source.id, limit=max(20, count * 5))
    await update.effective_message.reply_text(
        f"已将 {requeued} 条首次基线内容放入补发队列；本次发送成功 {sent} 条，失败 {failed} 条。"
    )


@adminOnly
async def disableCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if not context.args:
        await update.effective_message.reply_text("用法：/disable <UID或名称>")
        return
    key = " ".join(context.args).strip()
    ok = store(context).setSourceActive(key, False)
    await update.effective_message.reply_text("已停用。" if ok else "没有找到该账号。")


@adminOnly
async def enableCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if not context.args:
        await update.effective_message.reply_text("用法：/enable <UID或名称>")
        return
    key = " ".join(context.args).strip()
    ok = store(context).setSourceActive(key, True)
    await update.effective_message.reply_text("已启用。" if ok else "没有找到该账号。")


@adminOnly
async def destAddCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if not context.args:
        await update.effective_message.reply_text("用法：/destadd <ChatID或@频道> [名称]")
        return
    chatId = context.args[0].strip()
    name = " ".join(context.args[1:]).strip() or chatId
    if not (chatId.startswith("@") or re.fullmatch(r"-?\d{4,30}", chatId)):
        await update.effective_message.reply_text("目标应是数字 Chat ID（通常为 -100...）或 @公开频道用户名。")
        return
    store(context).addDestination(chatId, name)
    await update.effective_message.reply_text(
        f"已添加目标：{name}（{chatId}），类型：test。请确认 Bot 已加入群组/频道并拥有发送消息权限。\n"
        f"Use /destset {chatId} public to make it a public destination."
    )


@adminOnly
async def destRemoveCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if not context.args:
        await update.effective_message.reply_text("用法：/destremove <ChatID或@频道>")
        return
    chatId = context.args[0].strip()
    ok = store(context).removeDestination(chatId)
    await update.effective_message.reply_text("已移除目标。" if ok else "没有找到这个目标。")


@adminOnly
async def destSetCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if len(context.args) != 2 or context.args[1].lower() not in ("public", "test"):
        await update.effective_message.reply_text("Usage: /destset <ChatID or @channel> <public|test>")
        return
    chatId = context.args[0].strip()
    kind = context.args[1].lower()
    ok = store(context).setDestinationKind(chatId, kind)
    await update.effective_message.reply_text(f"{chatId} is now a {kind} destination." if ok else "Destination not found.")


@adminOnly
async def destListCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    destinations = store(context).allDestinations()
    if not destinations:
        await update.effective_message.reply_text("尚无推送目标。可用 /destadd 添加，或设置 TELEGRAM_CHAT_ID。")
        return
    lines = ["Telegram 推送目标："]
    active = {item["chatId"] for item in store(context).activeDestinations()}
    for dest in destinations:
        status = "启用" if dest["chatId"] in active else "已移除"
        lines.append(f"• {dest['name']}：{dest['chatId']}（{status}，{dest['kind']}）")
    await update.effective_message.reply_text("\n".join(lines))


@adminOnly
async def testCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    bot = context.application.bot
    destinations = store(context).activeDestinations("test")
    if not destinations:
        await update.effective_message.reply_text("No test destination. Add one with /destadd, public destinations never receive test messages.")
        return
    results = []
    for destination in destinations:
        try:
            await bot.send_message(
                chat_id=destination["chatId"],
                text=f"微博同步 Bot 测试消息\n目标：{destination['name']}\n时间：{datetime.now(timezone.utc).isoformat(timespec='seconds')}",
            )
            results.append(f"✓ {destination['name']} ({destination['chatId']})")
        except TelegramError as exc:
            results.append(f"✗ {destination['name']} ({destination['chatId']})：{exc}")
    await update.effective_message.reply_text("\n".join(results)[:3900])


@adminOnly
async def pauseCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    store(context).setPaused(True)
    await update.effective_message.reply_text("自动同步已暂停。")


@adminOnly
async def resumeCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    store(context).setPaused(False)
    await update.effective_message.reply_text("自动同步已恢复。")


@adminOnly
async def statusCommand(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    storage = store(context)
    stats = storage.stats()
    try:
        queueLength = stats["pending"]
        lines = [
            "运行状态：",
            f"自动同步暂停：{'是' if storage.isPaused() else '否'}",
            f"监控账号：{stats['active']} 个启用 / 共 {stats['sources']} 个",
            f"Telegram 目标：{stats['destinations']} 个",
            f"待发送投递任务：{queueLength} 条；累计成功投递：{stats['sent']} 条",
            f"微博轮询间隔：{settings.pollIntervalSeconds} 秒",
            f"评论轮询间隔：{settings.commentsPollIntervalSeconds} 秒",
        ]
        if stats["errors"]:
            lines.append("最近错误：")
            for error in stats["errors"]:
                if error.get("lastError"):
                    lines.append(f"• {error['name']} 动态：{str(error['lastError'])[:150]}")
                if error.get("lastCommentError"):
                    lines.append(f"• {error['name']} 评论：{str(error['lastCommentError'])[:150]}")
        await update.effective_message.reply_text("\n".join(lines)[:3900])
    except Exception as exc:
        await update.effective_message.reply_text(f"读取状态失败：{exc}")


async def errorHandler(update: object, context: ContextTypes.DEFAULT_TYPE) -> None:
    error = context.error
    if error is not None:
        logger.error("Telegram handler 异常", exc_info=(type(error), error, error.__traceback__))


def main() -> None:
    application = (
        Application.builder()
        .token(settings.telegramToken)
        .post_init(postInit)
        .post_shutdown(postShutdown)
        .build()
    )
    for name, handler in [
        ("start", startCommand),
        ("help", helpCommand),
        ("me", meCommand),
        ("add", addCommand),
        ("remove", removeCommand),
        ("list", listCommand),
        ("check", checkCommand),
        ("backfill", backfillCommand),
        ("disable", disableCommand),
        ("enable", enableCommand),
        ("destadd", destAddCommand),
        ("destset", destSetCommand),
        ("destremove", destRemoveCommand),
        ("destlist", destListCommand),
        ("test", testCommand),
        ("pause", pauseCommand),
        ("resume", resumeCommand),
        ("status", statusCommand),
    ]:
        application.add_handler(CommandHandler(name, handler))
    application.add_error_handler(errorHandler)
    application.run_polling(allowed_updates=Update.ALL_TYPES, drop_pending_updates=False)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError) as exc:
        logger.error("启动失败：%s", exc)
        sys.exit(1)