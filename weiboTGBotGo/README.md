# Weibo to Telegram bot (Go)

SQLite state database (`data/state.db`) using a pure-Go driver; CGO is not required.

## Build

```
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o weibo-tg-bot.exe .                       # Windows
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o weibo-tg-bot .  # Linux server
```

In Git Bash on Windows the second line works as written. Use `GOARCH=arm64` for an ARM server.

## Configure and run

Copy `.env.example` to `.env`, fill in the token, the admin ids and the Weibo cookie. When running the executable from `weiboTGBotGo` while reusing the repository-root Python database, set `DATABASE_PATH=../data/state.db` in the `.env` file read by the Go process. Paths are relative to the current working directory. Then start `./weibo-tg-bot`
(`-env path` selects another file). Real environment variables win over `.env`.

## Deploy

Upload two files: the binary and `.env`.

```
sudo useradd --system --home /opt/weibo-tg-bot weibobot
sudo install -d -o weibobot /opt/weibo-tg-bot
# copy weibo-tg-bot and .env to /opt/weibo-tg-bot, chmod 600 .env, chown to weibobot
sudo cp weibo-tg-bot.service /etc/systemd/system/
sudo systemctl enable --now weibo-tg-bot
journalctl -u weibo-tg-bot -f
```

Back up `data/state.db` to keep the baselines. Set `DATABASE_PATH` to the existing Python SQLite file. Only one process may poll a bot token: stop the Python bot first,
otherwise Telegram answers with HTTP 409.

## Public and test

Accounts and destinations are `public` or `test`, and everything starts as `test`.

- Public destinations only receive the content of public accounts. Test accounts, errors, test messages and
  media diagnostics only go to test destinations.
- Admin commands work only in a private chat with an admin or inside a test destination, elsewhere they are ignored.
- Errors are sent to the test destinations as alerts, at most once per kind every 30 minutes.

First steps in a private chat with the bot:

```
/destset <test chat id> test
/destset <public chat id or @channel> public
/add <UID> public <name>        builds the baseline right away
/latest 3 media                 sends the newest posts to the test destinations only
```

`/help` lists everything.

## Differences from the Python version

- State is stored in SQLite (`DATABASE_PATH`, default `./data/state.db`) and uses the Python database schema. Relative paths are resolved from the process working directory.
- Line breaks of a post are kept, and no spaces are added around mentions.
- New posts are sent oldest first, ordered by Weibo status id. An old post that is pinned later is recorded but not sent.
- Messages to the administrators are English.
