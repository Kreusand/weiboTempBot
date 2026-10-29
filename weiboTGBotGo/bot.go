package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var chatIDPattern = regexp.MustCompile(`^-?\d{4,30}$`)
var uidPattern = regexp.MustCompile(`^\d{4,30}$`)

// command is one parsed bot command together with the chat it came from.
type command struct {
	app  *App
	ctx  context.Context
	msg  *TelegramMessage
	args []string
}

type commandHandler func(c *command)

func (c *command) reply(text string) {
	if err := c.app.telegram.SendMessage(c.ctx, strconv.FormatInt(c.msg.Chat.ID, 10), truncateRunes(text, 3900), false); err != nil {
		slog.Warn("could not send a command reply", "error", err)
	}
}

func (a *App) commandTable() map[string]commandHandler {
	return map[string]commandHandler{
		"start": cmdStart, "help": cmdHelp, "me": cmdMe,
		"add": cmdAdd, "srcset": cmdSrcSet, "remove": cmdRemove, "list": cmdList,
		"check": cmdCheck, "backfill": cmdBackfill, "latest": cmdLatest,
		"disable": cmdDisable, "enable": cmdEnable,
		"destadd": cmdDestAdd, "destset": cmdDestSet, "destremove": cmdDestRemove, "destlist": cmdDestList,
		"test": cmdTest, "pause": cmdPause, "resume": cmdResume, "status": cmdStatus,
	}
}

// parseCommand splits "/name@bot arg arg" into its parts. A command addressed to another bot is ignored.
func parseCommand(text, botUsername string) (name string, args []string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", nil, false
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", nil, false
	}
	name = strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	if at := strings.Index(name, "@"); at >= 0 {
		target := name[at+1:]
		name = name[:at]
		if botUsername != "" && !strings.EqualFold(target, botUsername) {
			return "", nil, false
		}
	}
	if name == "" {
		return "", nil, false
	}
	return name, fields[1:], true
}

// isTestChat tells whether a chat is one of the test destinations.
func (a *App) isTestChat(chat TelegramChat) bool {
	for _, destination := range a.store.Destinations(kindTest) {
		id := strings.ToLower(destination.ChatID)
		if id == strconv.FormatInt(chat.ID, 10) || (chat.Username != "" && id == "@"+strings.ToLower(chat.Username)) {
			return true
		}
	}
	return false
}

// handleUpdate runs one command. Admin commands work only in a private chat with an admin or inside a
// test destination, anywhere else they are ignored silently, so command replies, which can contain
// errors and internal state, never appear in a public chat.
func (a *App) handleUpdate(ctx context.Context, update TelegramUpdate) {
	message := update.Message
	if message == nil || message.From == nil {
		return
	}
	name, args, ok := parseCommand(message.Text, a.botUsername)
	if !ok {
		return
	}
	handler, known := a.commandTable()[name]
	if !known {
		return
	}
	c := &command{app: a, ctx: ctx, msg: message, args: args}
	isPrivate := message.Chat.Type == "private"
	if !a.settings.AdminIDs[message.From.ID] {
		if isPrivate {
			c.reply("This command is not available.")
		}
		return
	}
	if !isPrivate && !a.isTestChat(message.Chat) {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("command crashed", "command", name, "panic", recovered)
			c.reply(fmt.Sprintf("The command failed: %v", recovered))
		}
	}()
	handler(c)
}

// runUpdates reads Telegram updates until the context ends.
func (a *App) runUpdates(ctx context.Context) {
	var offset int64
	for ctx.Err() == nil {
		updates, err := a.telegram.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var apiErr *TelegramError
			if errors.As(err, &apiErr) && apiErr.Code == 409 {
				slog.Error("another process is polling this bot token, stop it first", "error", err)
			} else {
				slog.Warn("could not read Telegram updates", "error", err)
			}
			if a.telegram.sleep(ctx, 5*time.Second) != nil {
				return
			}
			continue
		}
		for _, update := range updates {
			offset = update.UpdateID + 1
			a.handleUpdate(ctx, update)
		}
	}
}

// ---- helpers ----

func sourceListText(sources []Source) string {
	if len(sources) == 0 {
		return "No monitored accounts. Add one with /add <UID> [public|test] [name]."
	}
	lines := []string{"Monitored accounts:"}
	for _, source := range sources {
		state := "running"
		if !source.Active {
			state = "disabled"
		}
		posts, comments := "post baseline pending", "comment baseline pending"
		if source.Initialized {
			posts = "post baseline ready"
		}
		if source.CommentsInitialized {
			comments = "comment baseline ready"
		}
		lastCheck := source.LastChecked
		if lastCheck == "" {
			lastCheck = "never"
		}
		lines = append(lines, fmt.Sprintf("• %s (%s): %s; %s; %s; %s; last check %s",
			source.Name, source.UID, source.Kind, state, posts, comments, lastCheck))
		if source.LastError != "" {
			lines = append(lines, "  post error: "+truncateRunes(source.LastError, 180))
		}
		if source.LastCommentError != "" {
			lines = append(lines, "  comment error: "+truncateRunes(source.LastCommentError, 180))
		}
	}
	return strings.Join(lines, "\n")
}

// parseLatestArgs reads "/latest [UID or name] [count] [media]" into (key, count, mediaOnly).
func parseLatestArgs(args []string) (key string, count int, mediaOnly bool) {
	count = 1
	var keyParts []string
	for _, arg := range args {
		switch {
		case strings.EqualFold(arg, "media"):
			mediaOnly = true
		case isDigits(arg) && len(arg) <= 2:
			// Weibo UIDs have at least 4 digits, so a short number is the count.
			value, _ := strconv.Atoi(arg)
			count = max(1, min(value, 10))
		default:
			keyParts = append(keyParts, arg)
		}
	}
	return strings.TrimSpace(strings.Join(keyParts, " ")), count, mediaOnly
}

func validChatTarget(target string) bool {
	return strings.HasPrefix(target, "@") || chatIDPattern.MatchString(target)
}

// ---- commands ----

func cmdStart(c *command) {
	c.reply("The Weibo to Telegram bot is running.\nUse /help for the commands.\n" +
		"A new account only records its existing posts and comments as a baseline, nothing old is pushed.")
}

func cmdHelp(c *command) {
	c.reply(`Accounts:
/add <UID> [public|test] [name] - add a Weibo account (test by default, its content only goes to test destinations)
/srcset <UID or name> <public|test> - set the account kind, only public accounts reach public destinations
/remove <UID or name> - delete an account
/list - accounts, state and errors
/check [UID or name] - check one or all accounts now
/backfill <UID or name> [count] - queue the newest baseline items again
/latest [UID or name] [count 1-10] [media] - send the newest posts to test destinations only, for testing; media selects posts with photos or videos; nothing is saved
/disable <UID or name>, /enable <UID or name>

Telegram destinations:
/destadd <ChatID or @channel> [name] - add a destination (test by default)
/destset <ChatID or @channel> <public|test> - set the kind, creating the destination if needed
/destremove <ChatID or @channel>
/destlist
/test - send a test message to all test destinations

Public destinations only receive the content of public accounts. Test accounts, errors, test messages and diagnostics go to test destinations only.
Commands work only in a private chat with an admin or inside a test destination.

Other:
/me - check the Weibo login
/pause, /resume - pause or resume the automatic sync
/status - state and queue
/help

The Weibo cookie is read from the server .env only, there is no command to paste it into Telegram.`)
}

func cmdMe(c *command) {
	info, err := c.app.weibo.CheckLogin(c.ctx)
	if err != nil {
		c.reply("Weibo login check failed: " + err.Error())
		return
	}
	uid := info.UID
	if uid == "" {
		uid = "not reported"
	}
	c.reply(fmt.Sprintf("The Weibo API is reachable. login=%s, account UID=%s.\n"+
		"This only confirms the basic login. Whether followers-only posts are readable needs a /check.", info.Login, uid))
}

func cmdAdd(c *command) {
	if len(c.args) == 0 {
		c.reply("Usage: /add <UID> [public|test] [name]\nNew accounts are test accounts unless you say public.")
		return
	}
	uid := strings.TrimSpace(c.args[0])
	if !uidPattern.MatchString(uid) {
		c.reply("The UID must be the numeric Weibo user id.")
		return
	}
	rest := c.args[1:]
	kind := kindTest
	if len(rest) > 0 && (strings.EqualFold(rest[0], kindPublic) || strings.EqualFold(rest[0], kindTest)) {
		kind = strings.ToLower(rest[0])
		rest = rest[1:]
	}
	name := strings.TrimSpace(strings.Join(rest, " "))
	if name == "" {
		name = uid
	}
	source, err := c.app.store.AddSource(uid, name, kind)
	if err != nil {
		c.reply(err.Error())
		return
	}
	audience := "test destinations only"
	if kind == kindPublic {
		audience = "all destinations"
	}
	c.reply(fmt.Sprintf("Added Weibo account %s (UID %s) as a %s account, its content goes to %s. "+
		"Building the baseline now: existing posts and comments are recorded but not pushed.", source.Name, uid, kind, audience))
	// Build the baseline right away. Waiting for the next poll would let a post made in the meantime
	// be swallowed into the baseline instead of being pushed.
	result, err := c.app.pollSource(c.ctx, source, true)
	if err != nil {
		c.reply(fmt.Sprintf("Baseline failed: %v\nRegular polling will retry it, or run /check %s.", err, uid))
		return
	}
	c.reply("Baseline ready. " + result)
}

func cmdSrcSet(c *command) {
	if len(c.args) < 2 || !validKind(strings.ToLower(c.args[len(c.args)-1])) {
		c.reply("Usage: /srcset <UID or name> <public|test>")
		return
	}
	key := strings.TrimSpace(strings.Join(c.args[:len(c.args)-1], " "))
	kind := strings.ToLower(c.args[len(c.args)-1])
	found, err := c.app.store.SetSourceKind(key, kind)
	switch {
	case err != nil:
		c.reply(err.Error())
	case !found:
		c.reply("Account not found. Use /list.")
	case kind == kindTest:
		c.reply(key + " is now a test account. Content still waiting for public destinations was cancelled.")
	default:
		c.reply(key + " is now a public account.")
	}
}

func cmdRemove(c *command) {
	if len(c.args) == 0 {
		c.reply("Usage: /remove <UID or name>")
		return
	}
	if c.app.store.RemoveSource(strings.Join(c.args, " ")) {
		c.reply("The account and its local state were deleted.")
	} else {
		c.reply("Account not found.")
	}
}

func cmdList(c *command) {
	c.reply(sourceListText(c.app.store.ListSources(false)))
}

func cmdCheck(c *command) {
	var sources []Source
	if len(c.args) > 0 {
		source, ok := c.app.store.GetSource(strings.Join(c.args, " "))
		if !ok {
			c.reply("Account not found. Use /list.")
			return
		}
		sources = []Source{source}
	} else {
		sources = c.app.store.ListSources(true)
	}
	if len(sources) == 0 {
		c.reply("No active accounts. Add one with /add first.")
		return
	}
	var results []string
	for _, source := range sources {
		result, err := c.app.pollSource(c.ctx, source, true)
		if err != nil {
			result = fmt.Sprintf("%s (%s): check failed: %v", source.Name, source.UID, err)
		}
		results = append(results, result)
	}
	c.reply(strings.Join(results, "\n\n"))
}

func cmdBackfill(c *command) {
	if len(c.args) == 0 {
		c.reply("Usage: /backfill <UID or name> [count]")
		return
	}
	source, ok := c.app.store.GetSource(strings.TrimSpace(c.args[0]))
	if !ok {
		c.reply("Account not found.")
		return
	}
	count := 5
	if len(c.args) > 1 {
		value, err := strconv.Atoi(c.args[1])
		if err != nil {
			c.reply("The count must be an integer from 1 to 100.")
			return
		}
		count = max(1, min(value, 100))
	}
	requeued := c.app.store.BackfillSkipped(source.ID, count)
	sent, failed := c.app.deliverPending(c.ctx, source.ID, max(20, count*5))
	c.reply(fmt.Sprintf("Queued %d baseline items again; this run sent %d and failed %d.", requeued, sent, failed))
}

// cmdLatest sends the newest posts of an account to the test destinations only. Nothing is saved,
// so the baseline and the de-duplication of real updates are untouched.
func cmdLatest(c *command) {
	key, count, mediaOnly := parseLatestArgs(c.args)
	var uid, label string
	switch {
	case key != "":
		if source, ok := c.app.store.GetSource(key); ok {
			uid, label = source.UID, source.Name
		} else if uidPattern.MatchString(key) {
			uid, label = key, key
		} else {
			c.reply("Account not found. Use /list, or pass a numeric Weibo UID.")
			return
		}
	default:
		active := c.app.store.ListSources(true)
		if len(active) == 0 {
			c.reply("No active account. Add one with /add, or pass a UID.")
			return
		}
		uid, label = active[0].UID, active[0].Name
	}
	destinations := c.app.store.Destinations(kindTest)
	if len(destinations) == 0 {
		c.reply("No test destination. Add one with /destset <ChatID> test, public destinations never receive this.")
		return
	}

	items, total, pinnedCount, err := c.app.latestItems(c.ctx, uid, &label, count, mediaOnly)
	if err != nil {
		c.reply("Could not read the timeline: " + err.Error())
		return
	}
	if len(items) == 0 {
		scope := ""
		if mediaOnly {
			scope = "with photos or videos "
		}
		c.reply(fmt.Sprintf("No posts %sfound among the %d most recent ones (%d pinned skipped).", scope, total, pinnedCount))
		return
	}
	var failures []string
	// Oldest first, so the newest post ends up at the bottom of the chat.
	for index := len(items) - 1; index >= 0; index-- {
		for _, destination := range destinations {
			if err := c.app.sender.Send(c.ctx, destination.ChatID, label, items[index], true); err != nil {
				failures = append(failures, destination.Name+": "+err.Error())
			}
		}
	}
	summary := fmt.Sprintf("Sent %d post(s) of %s to %d test destination(s). Nothing was queued or saved. Pinned posts skipped: %d.",
		len(items), label, len(destinations), pinnedCount)
	if len(failures) > 0 {
		summary += "\nFailures:\n" + strings.Join(failures, "\n")
	}
	c.reply(summary)
}

// latestItems reads the timeline and picks the newest unpinned posts.
func (a *App) latestItems(ctx context.Context, uid string, label *string, count int, mediaOnly bool) (items []FeedItem, total, pinned int, err error) {
	a.pollMu.Lock()
	defer a.pollMu.Unlock()
	name, statuses, err := a.weibo.GetTimeline(ctx, uid)
	if err != nil {
		return nil, 0, 0, err
	}
	if *label == uid && name != "" {
		*label = name
	}
	var candidates []obj
	for _, status := range statuses {
		if isPinned(status) {
			pinned++
		} else {
			candidates = append(candidates, status)
		}
	}
	for _, status := range sortNewestFirst(candidates) {
		item := a.weibo.ToFeedItem(ctx, uid, status)
		if mediaOnly && len(item.Media) == 0 {
			continue
		}
		items = append(items, item)
		if len(items) >= count {
			break
		}
	}
	return items, len(statuses), pinned, nil
}

func cmdDisable(c *command) { setActive(c, false) }
func cmdEnable(c *command)  { setActive(c, true) }

func setActive(c *command, active bool) {
	verb := "disable"
	if active {
		verb = "enable"
	}
	if len(c.args) == 0 {
		c.reply("Usage: /" + verb + " <UID or name>")
		return
	}
	if !c.app.store.SetSourceActive(strings.Join(c.args, " "), active) {
		c.reply("Account not found.")
		return
	}
	if active {
		c.reply("Enabled.")
	} else {
		c.reply("Disabled.")
	}
}

func cmdDestAdd(c *command) {
	if len(c.args) == 0 {
		c.reply("Usage: /destadd <ChatID or @channel> [name]")
		return
	}
	chatID := strings.TrimSpace(c.args[0])
	name := strings.TrimSpace(strings.Join(c.args[1:], " "))
	if name == "" {
		name = chatID
	}
	if !validChatTarget(chatID) {
		c.reply("The target must be a numeric Chat ID (usually -100...) or a public @channel name.")
		return
	}
	if err := c.app.store.AddDestination(chatID, name, ""); err != nil {
		c.reply(err.Error())
		return
	}
	c.reply(fmt.Sprintf("Added destination %s (%s) as a test destination. Make sure the bot is in the chat and may post.\n"+
		"Use /destset %s public to make it a public destination.", name, chatID, chatID))
}

func cmdDestRemove(c *command) {
	if len(c.args) == 0 {
		c.reply("Usage: /destremove <ChatID or @channel>")
		return
	}
	if c.app.store.RemoveDestination(strings.TrimSpace(c.args[0])) {
		c.reply("Destination removed.")
	} else {
		c.reply("Destination not found.")
	}
}

// cmdDestSet sets the kind of a destination and creates the destination when it does not exist yet.
func cmdDestSet(c *command) {
	if len(c.args) != 2 || !validKind(strings.ToLower(c.args[1])) {
		c.reply("Usage: /destset <ChatID or @channel> <public|test>")
		return
	}
	chatID := strings.TrimSpace(c.args[0])
	kind := strings.ToLower(c.args[1])
	found, err := c.app.store.SetDestinationKind(chatID, kind)
	if err != nil {
		c.reply(err.Error())
		return
	}
	if found {
		c.reply(fmt.Sprintf("%s is now a %s destination.", chatID, kind))
		return
	}
	if !validChatTarget(chatID) {
		c.reply("The target must be a numeric Chat ID (usually -100...) or a public @channel name.")
		return
	}
	// Unknown destination: make sure the bot can reach the chat before registering it.
	chat, err := c.app.telegram.GetChat(c.ctx, chatID)
	if err != nil {
		c.reply(fmt.Sprintf("Could not access %s: %v\nAdd the bot to that chat first, then retry.", chatID, err))
		return
	}
	name := chat.Title
	if name == "" && chat.Username != "" {
		name = "@" + chat.Username
	}
	if name == "" {
		name = chatID
	}
	if err := c.app.store.AddDestination(chatID, name, kind); err != nil {
		c.reply(err.Error())
		return
	}
	c.reply(fmt.Sprintf("Added %s (%s) as a %s destination.", name, chatID, kind))
}

func cmdDestList(c *command) {
	destinations := c.app.store.Destinations("")
	if len(destinations) == 0 {
		c.reply("No destinations yet. Add one with /destset <ChatID> test, or set TELEGRAM_CHAT_ID.")
		return
	}
	lines := []string{"Telegram destinations:"}
	for _, destination := range destinations {
		lines = append(lines, fmt.Sprintf("• %s: %s (%s)", destination.Name, destination.ChatID, destination.Kind))
	}
	c.reply(strings.Join(lines, "\n"))
}

func cmdTest(c *command) {
	destinations := c.app.store.Destinations(kindTest)
	if len(destinations) == 0 {
		c.reply("No test destination. Add one with /destset <ChatID> test, public destinations never receive test messages.")
		return
	}
	var results []string
	for _, destination := range destinations {
		text := fmt.Sprintf("Weibo sync bot test message\nDestination: %s\nTime: %s", destination.Name, time.Now().UTC().Format(time.RFC3339))
		if err := c.app.telegram.SendMessage(c.ctx, destination.ChatID, text, false); err != nil {
			results = append(results, fmt.Sprintf("✗ %s (%s): %v", destination.Name, destination.ChatID, err))
		} else {
			results = append(results, fmt.Sprintf("✓ %s (%s)", destination.Name, destination.ChatID))
		}
	}
	c.reply(strings.Join(results, "\n"))
}

func cmdPause(c *command) {
	c.app.store.SetPaused(true)
	c.reply("Automatic sync paused.")
}

func cmdResume(c *command) {
	c.app.store.SetPaused(false)
	c.reply("Automatic sync resumed.")
}

func cmdStatus(c *command) {
	stats := c.app.store.Stats()
	paused := "no"
	if c.app.store.IsPaused() {
		paused = "yes"
	}
	lines := []string{
		"Status:",
		"Automatic sync paused: " + paused,
		fmt.Sprintf("Accounts: %d active of %d", stats.ActiveSource, stats.Sources),
		fmt.Sprintf("Telegram destinations: %d", stats.Destinations),
		fmt.Sprintf("Deliveries waiting: %d; delivered in total: %d", stats.Pending, stats.Sent),
		fmt.Sprintf("Post poll interval: %d s", int(c.app.settings.PollInterval.Seconds())),
		fmt.Sprintf("Comment poll interval: %d s", int(c.app.settings.CommentsPollInterval.Seconds())),
	}
	if len(stats.Errors) > 0 {
		lines = append(lines, "Recent errors:")
		for _, source := range stats.Errors {
			if source.LastError != "" {
				lines = append(lines, fmt.Sprintf("• %s posts: %s", source.Name, truncateRunes(source.LastError, 150)))
			}
			if source.LastCommentError != "" {
				lines = append(lines, fmt.Sprintf("• %s comments: %s", source.Name, truncateRunes(source.LastCommentError, 150)))
			}
		}
	}
	c.reply(strings.Join(lines, "\n"))
}
