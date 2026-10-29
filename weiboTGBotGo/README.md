# Weibo to Telegram bot (Go)

One static binary, standard library only, no CGO. State lives in one JSON file.

## Build

```
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o weibo-tg-bot.exe .                       # Windows
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o weibo-tg-bot .  # Linux server
```

In Git Bash on Windows the second line works as written. Use `GOARCH=arm64` for an ARM server.

## Configure and run

Copy `.env.example` to `.env`, fill in the token, the admin ids and the Weibo cookie, then start `./weibo-tg-bot`
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

Back up `data/state.json` to keep the baselines. Only one process may poll a bot token: stop the Python bot first,
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

- State is a JSON file (`STATE_PATH`), the old `state.db` is not read. Re-add accounts and destinations, `/add` builds the baseline.
- Line breaks of a post are kept, and no spaces are added around mentions.
- New posts are sent oldest first, ordered by Weibo status id. An old post that is pinned later is recorded but not sent.
- Messages to the administrators are English.
