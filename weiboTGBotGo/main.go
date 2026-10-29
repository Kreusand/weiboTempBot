package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func logLevel(name string) slog.Level {
	switch strings.ToUpper(name) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR", "CRITICAL":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func main() {
	envPath := flag.String("env", ".env", "path of the .env file")
	flag.Parse()
	if err := run(*envPath); err != nil {
		fmt.Fprintln(os.Stderr, "startup failed:", err)
		os.Exit(1)
	}
}

func run(envPath string) error {
	settings, err := LoadSettings(environmentWithDotEnv(envPath))
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel(settings.LogLevel)})))

	store, err := OpenStore(settings.StatePath)
	if err != nil {
		return err
	}
	defer store.Close()
	if settings.DefaultChatID != "" {
		if err := store.AddDestination(settings.DefaultChatID, "Default Telegram destination", ""); err != nil {
			return err
		}
	}

	telegram := NewTelegramClient(settings.TelegramToken)
	app := NewApp(settings, store, NewWeiboClient(settings), telegram, NewSender(telegram, settings))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	me, err := telegram.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("the Telegram token was rejected or Telegram is unreachable: %w", err)
	}
	app.botUsername = me.Username
	slog.Info("bot started",
		"username", me.Username,
		"postPollSeconds", int(settings.PollInterval.Seconds()),
		"commentPollSeconds", int(settings.CommentsPollInterval.Seconds()),
		"accounts", len(store.ListSources(false)),
		"destinations", len(store.Destinations("")),
		"statePath", settings.StatePath,
	)

	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		app.pollLoop(ctx)
	}()
	app.runUpdates(ctx)
	stop()
	slog.Info("shutting down")
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		slog.Warn("the sync loop did not stop in time")
	}
	return nil
}
