# 微博 → Telegram 同步 Bot（免付费官方 API 方案）

这个项目直接请求微博移动网页端的 JSON 接口，不使用微博官方付费 API，也不依赖 RSSHub。它以 `WEIBO_COOKIES` 中的登录态读取你能访问的微博内容，再把事件排队并发送到一个或多个 Telegram 群组/频道。

> 当前环境没有你的微博登录 Cookie 或 Telegram Bot Token，因此没有办法替你完成真实账号联调。项目已进行本地语法检查和模拟数据测试；真实账号是否返回限定可见微博、视频直链和评论回复，需在你的 VPS 上用 `/me`、`/check` 实测。微博移动网页接口不是稳定的公开 API，字段变化或风控都可能让抓取暂时失效。

## 功能范围

- 监控多个微博 UID，读取登录态可见的时间线。
- 对动态尽可能解析长文本、照片、视频直链、转发微博正文与原文链接。
- 给动态添加 `#微博动态`，并尽可能追加 `#微博粉丝可见动态`、`#微博照片`、`#微博视频`、`#微博转发` 标签。
- 对目标账号自己发布的微博，扫描近期评论，并仅保留评论作者 UID 与目标账号 UID 相同的评论；同时扫描部分评论线程，只保留目标账号自己发表的回复，分别添加 `#微博评论` 和 `#微博评论回复`。
- 多个 Telegram 目标；SQLite 事件队列、按目标逐个记录投递状态、失败退避重试、重启恢复、首次基线、手动补发、错误日志。
- 管理命令仅允许 `ADMIN_IDS` 中配置的 Telegram 数字用户 ID 使用。

## 明确的采集边界

1. **粉丝可见内容**：抓取是否成功取决于登录 Cookie 的真实权限和微博接口返回的数据。浏览器能看见的内容不一定会被每个接口完整返回。Bot 不会自动升级账号权限或绕过账号本身没有的权限。
2. **评论**：默认每 5 分钟扫描目标账号最近 20 条时间线微博，每条微博读取最多 20 条顶层评论，并最多检查 12 个含回复的评论线程。只有作者 UID 等于被监控 UID 的评论/回复才会生成事件。这个配置是保守的轮询上限，不保证捕获所有旧微博下的所有评论；可以调整 `.env`，但过高的请求量可能触发限流。
3. **评论排序**：微博接口有时返回热门评论而不是纯时间序，因此即使提高请求量也不能保证零遗漏。建议先在私人测试目标中比对原 Bot 的实时消息。
4. **视频**：可解析到 MP4 直链且大小/格式符合 Telegram 限制时会尝试上传。HLS `.m3u8`、需额外签名的流和过大的视频会发送原微博/视频页面链接。不同账号、不同视频类型的返回结构可能不同。
5. **标签**：`#微博粉丝可见动态` 依赖 API 返回 `visible` 等字段；如果微博不返回可见性元数据，Bot 无法仅凭文本可靠判断标签，消息仍会保留动态链接。
6. **重试语义**：每个事件、每个 Telegram 目标单独保存状态；失败按指数退避重试。若 Telegram 已接收消息但进程在本地记录成功之前崩溃，极小概率会重复投递。

## 1. 创建 Telegram Bot

1. 在 Telegram 联系 `@BotFather`，执行 `/newbot` 并保存 Token。
2. 设置 `.env` 的 `TELEGRAM_BOT_TOKEN` 和 `ADMIN_IDS`。
3. 把 Bot 加进测试群或频道，并确保有发消息权限。
4. `TELEGRAM_CHAT_ID` 可以填默认目标，也可以留空，在 Bot 中使用 `/destadd` 配置多个目标。

## 2. 配置微博 Cookie

登录有权查看目标内容的微博账号，在浏览器中访问 `https://m.weibo.cn/`。从浏览器开发者工具 Network 里选择该站点已经成功返回 JSON 的请求，在 Request Headers 中复制 `Cookie` 的值，填入服务器 `.env` 的 `WEIBO_COOKIES`。

- Cookie 是登录凭据，等同于账号会话；只保存在 VPS 的 `.env`，不要发给 Bot、不要贴到聊天、不要提交到 Git。
- 建议为这个 Bot 使用单独的浏览器配置/会话，并在需要时更新 Cookie。
- Cookie 过期时 `/me` 或 `/check` 会显示登录态相关错误。程序不会把 Cookie 写到日志。
- `.env` 如果包含 `#` 或其他解析敏感字符，请用双引号包住 Cookie 值。

## 3. 在 VPS 上启动

将项目文件放到 VPS，进入目录：

```bash
cp .env.example .env
nano .env
```

填好 `TELEGRAM_BOT_TOKEN`、`ADMIN_IDS`、`WEIBO_COOKIES`，再启动：

```bash
docker compose up -d --build
docker compose logs -f bot
```

数据会保存在 `./data/state.db`。容器重启时数据库不会丢失。建议将项目目录权限限制为 VPS 管理用户，并确保 `.env` 未被 Web 服务器公开。

不用 Docker 时，要求 Python 3.11 或更高版本：

```bash
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
python bot.py
```

## 4. 首次私人测试流程

1. 先私聊 Bot 执行 `/start`。
2. 执行 `/me`，确认微博接口可访问。该命令只检查基本登录状态，不能保证所有限定内容都可读。
3. 执行 `/destadd <你的私人测试群ChatID> 私人测试`，或在 `.env` 设置 `TELEGRAM_CHAT_ID`。
4. 执行 `/test`，确认 Bot 在测试群有发送权限。
5. 执行 `/add <目标微博UID> 账号名称`，然后 `/check <UID>`。
6. 首次检查只建立动态和评论基线，不会把当前已有内容一股脑发送出来。之后新发现的事件才会自动投递。
7. 通过原 Bot 与新 Bot 对照实时更新；若要手动补发初始化时跳过的记录，执行 `/backfill <UID> 5`。
8. 测试通过后用 `/destadd <ChatID或@频道> 名称` 增加正式目标。

## 5. Bot 命令

| 命令 | 作用 |
|---|---|
| `/me` | 检查微博基础登录态 |
| `/add UID [名称]` | 添加微博账号 |
| `/remove UID或名称` | 删除账号及对应本地状态 |
| `/list` | 查看监控账号、基线和错误 |
| `/check [UID或名称]` | 立即检查账号；无参数时检查所有启用账号 |
| `/backfill UID [条数]` | 补发首次基线中最近的记录，范围 1–100 |
| `/latest [UID或名称] [条数] [media]` | 把最新 1–10 条微博只发到 `test` 目标用于测试；加 `media` 只选带图片/视频的；不写数据库，不影响基线和去重 |
| `/disable UID或名称` / `/enable UID或名称` | 暂停/恢复某账号的监控 |
| `/destadd ChatID或@频道 [名称]` | 添加推送目标 |
| `/destremove ChatID或@频道` | 移除推送目标并取消该目标未发送的任务 |
| `/destset ChatID或@频道 public\|test` | 设置目标类型；新目标默认为 `test` |
| `/destlist` | 查看推送目标及类型 |
| `/test` | 仅向 `test` 目标发测试消息 |
| `/pause` / `/resume` | 全局暂停/恢复自动轮询 |
| `/status` | 查看队列、目标数量、轮询间隔和最近错误 |
| `/help` | 显示命令帮助 |

群组数字 ID 通常以 `-100` 开头；公开频道可使用 `@频道用户名`。向私有频道推送时，需要使用数字 Chat ID，并确保 Bot 已被加入且有发帖权限。

### 公开目标与测试目标

- `public` 目标只收到微博内容（动态、评论、评论回复及其媒体）。测试消息、错误提醒、媒体下载/上传失败说明都不会发到这里。
- `test` 目标收到同样的内容，另外还会收到 `/test` 消息、限流后的错误提醒（同一类错误 30 分钟最多一条）和媒体失败说明。
- 管理命令只在管理员的私聊或 `test` 目标中生效；在 `public` 目标和其他群组中会被静默忽略。
- 旧数据库升级后所有已有目标都会变成 `test`，需要对正式目标执行 `/destset ... public`。

## 6. 配置参数

- `POLL_INTERVAL_SECONDS`：微博时间线检查间隔，默认 120 秒，最低 60 秒。
- `COMMENTS_POLL_INTERVAL_SECONDS`：评论/回复检查间隔，默认 300 秒，最低 120 秒。
- `TIMELINE_PAGES`：每次读取时间线的页数，默认 3。
- `COMMENTS_WATCH_POSTS`：检查评论的近期微博数，默认 20。
- `COMMENTS_TOPLEVEL_LIMIT`：每条微博每次扫描最多检查多少条顶层评论，默认 20。
- `REPLY_THREADS_PER_POLL`：每轮最多请求多少个评论回复线程，默认 12。
- `MAX_MEDIA_BYTES`：每条微博下载媒体的总大小预算，默认约 45 MB。
- `MAX_MEDIA_PER_POST`：每条事件最多处理多少个媒体资源，默认 10。
- `DATABASE_PATH`：数据库文件，Docker 默认 `/app/data/state.db`。

不要为了追求“实时”将轮询间隔调得过低。若出现 HTTP 432、登录页跳转、空白响应或频繁失败，应先暂停轮询并确认登录态/微博接口状态，而不是快速重试。

## 7. 当前实现使用的非官方移动网页接口

- `GET https://m.weibo.cn/api/config`：基本登录状态检查。
- `GET https://m.weibo.cn/api/container/getIndex`：读取用户时间线、头像资料和微博卡片。
- `GET https://m.weibo.cn/statuses/extend?id=...`：尝试补全长微博正文。
- `GET https://m.weibo.cn/api/comments/show?id=...`：读取微博评论列表，失败时回退到 `comments/hotflow`。
- `GET https://m.weibo.cn/comments/hotflow` 与 `comments/hotFlowChild`：读取评论和回复线程。

这些不是承诺长期兼容的公开开发者 API；没有按官方 API 计费，但会受到微博服务端规则、账号权限、接口变动和风控影响。项目不执行点赞、评论、转发等写操作。

## 8. 测试和排错

运行单元测试：

```bash
python -m unittest discover -s tests -v
```

查看日志：

```bash
docker compose logs --tail=200 bot
```

常见情况：

- `WEIBO_COOKIES` 缺失：服务会拒绝启动。
- `未登录` / `Cookie 过期`：重新从登录状态的 `m.weibo.cn` 请求复制 Cookie，更新 `.env` 并重启。
- `/me` 成功但目标 `/check` 失败：目标可能对当前账号不可见，或微博当前接口返回异常。
- 视频显示为链接而非上传文件：接口没有返回可直传 MP4，或文件过大/格式不支持。
- 评论/回复漏报：检查 `COMMENTS_WATCH_POSTS`、`COMMENTS_TOPLEVEL_LIMIT` 和 `REPLY_THREADS_PER_POLL`，并考虑微博返回的评论列表未按最新时间排序。
- Telegram 发帖失败：先检查 `/test` 返回的具体错误、Bot 是否在目标群组，以及频道发帖权限。