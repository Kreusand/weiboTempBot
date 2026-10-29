package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	telegramBase  = "https://api.telegram.org"
	textTimeout   = 30 * time.Second
	uploadTimeout = 180 * time.Second
	maxRetryWait  = 30 * time.Second
	maxRateRetry  = 2
)

// TelegramError is an error answered by the Bot API.
type TelegramError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *TelegramError) Error() string {
	return fmt.Sprintf("Telegram error %d: %s", e.Code, e.Description)
}

type TelegramUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type TelegramChat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Username string `json:"username"`
}

type TelegramMessage struct {
	MessageID int           `json:"message_id"`
	From      *TelegramUser `json:"from"`
	Chat      TelegramChat  `json:"chat"`
	Text      string        `json:"text"`
}

type TelegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *TelegramMessage `json:"message"`
}

// uploadFile is raw file content sent as a multipart part.
type uploadFile struct {
	filename string
	data     []byte
}

// groupMember is one photo or video of an album.
type groupMember struct {
	kind    string
	caption string
	file    uploadFile
}

// TelegramClient talks to the Bot API with the standard library only.
type TelegramClient struct {
	token   string
	baseURL string
	http    *http.Client
	sleep   func(context.Context, time.Duration) error
}

func NewTelegramClient(token string) *TelegramClient {
	return &TelegramClient{token: token, baseURL: telegramBase, http: &http.Client{}, sleep: sleepContext}
}

func sleepContext(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// redact removes the bot token from an error. Go puts the full request URL, which contains the
// token, into network errors, and that must never reach a log or a chat.
func (c *TelegramClient) redact(err error) error {
	if err == nil || c.token == "" {
		return err
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = fmt.Errorf("%s Telegram request: %w", urlErr.Op, urlErr.Err)
	}
	if strings.Contains(err.Error(), c.token) {
		return errors.New(strings.ReplaceAll(err.Error(), c.token, "***"))
	}
	return err
}

// call sends one request. Files switch the body to multipart. A 429 answer is retried after the
// wait Telegram asks for.
func (c *TelegramClient) call(ctx context.Context, method string, fields map[string]string, files map[string]uploadFile, timeout time.Duration) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		result, err := c.callOnce(ctx, method, fields, files, timeout)
		var apiErr *TelegramError
		if err == nil || !errors.As(err, &apiErr) || apiErr.Code != http.StatusTooManyRequests || attempt >= maxRateRetry {
			return result, err
		}
		wait := time.Duration(apiErr.RetryAfter) * time.Second
		if wait <= 0 || wait > maxRetryWait {
			return nil, err
		}
		if sleepErr := c.sleep(ctx, wait); sleepErr != nil {
			return nil, sleepErr
		}
	}
}

func (c *TelegramClient) callOnce(ctx context.Context, method string, fields map[string]string, files map[string]uploadFile, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body bytes.Buffer
	contentType := ""
	if len(files) == 0 {
		contentType = "application/json"
		if err := json.NewEncoder(&body).Encode(fields); err != nil {
			return nil, err
		}
	} else {
		writer := multipart.NewWriter(&body)
		for name, value := range fields {
			if err := writer.WriteField(name, value); err != nil {
				return nil, err
			}
		}
		for name, file := range files {
			part, err := writer.CreateFormFile(name, file.filename)
			if err != nil {
				return nil, err
			}
			if _, err := part.Write(file.data); err != nil {
				return nil, err
			}
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		contentType = writer.FormDataContentType()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/"+method, &body)
	if err != nil {
		return nil, c.redact(err)
	}
	request.Header.Set("Content-Type", contentType)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, c.redact(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, c.redact(err)
	}
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("Telegram answered HTTP %d with an unreadable body", response.StatusCode)
	}
	if !envelope.OK {
		code := envelope.ErrorCode
		if code == 0 {
			code = response.StatusCode
		}
		return nil, &TelegramError{Code: code, Description: envelope.Description, RetryAfter: envelope.Parameters.RetryAfter}
	}
	return envelope.Result, nil
}

func (c *TelegramClient) GetMe(ctx context.Context) (TelegramUser, error) {
	raw, err := c.call(ctx, "getMe", nil, nil, textTimeout)
	if err != nil {
		return TelegramUser{}, err
	}
	var user TelegramUser
	return user, json.Unmarshal(raw, &user)
}

// GetUpdates long-polls for new messages.
func (c *TelegramClient) GetUpdates(ctx context.Context, offset int64, pollSeconds int) ([]TelegramUpdate, error) {
	raw, err := c.call(ctx, "getUpdates", map[string]string{
		"offset":          strconv.FormatInt(offset, 10),
		"timeout":         strconv.Itoa(pollSeconds),
		"allowed_updates": `["message"]`,
	}, nil, time.Duration(pollSeconds+20)*time.Second)
	if err != nil {
		return nil, err
	}
	var updates []TelegramUpdate
	return updates, json.Unmarshal(raw, &updates)
}

func (c *TelegramClient) SendMessage(ctx context.Context, chatID, text string, disablePreview bool) error {
	fields := map[string]string{"chat_id": chatID, "text": text}
	if disablePreview {
		fields["disable_web_page_preview"] = "true"
	}
	_, err := c.call(ctx, "sendMessage", fields, nil, textTimeout)
	return err
}

func (c *TelegramClient) GetChat(ctx context.Context, chatID string) (TelegramChat, error) {
	raw, err := c.call(ctx, "getChat", map[string]string{"chat_id": chatID}, nil, textTimeout)
	if err != nil {
		return TelegramChat{}, err
	}
	var chat TelegramChat
	return chat, json.Unmarshal(raw, &chat)
}

// SendSingle uploads one photo or video.
func (c *TelegramClient) SendSingle(ctx context.Context, chatID, kind, caption string, file uploadFile) error {
	method, field := "sendPhoto", "photo"
	fields := map[string]string{"chat_id": chatID}
	if kind == mediaVideo {
		method, field = "sendVideo", "video"
		fields["supports_streaming"] = "true"
	}
	if caption != "" {
		fields["caption"] = caption
	}
	_, err := c.call(ctx, method, fields, map[string]uploadFile{field: file}, uploadTimeout)
	return err
}

// SendAlbum uploads 2 to 10 photos or videos as one album. Every member is referenced from the
// "media" description by an attach:// name, otherwise Telegram receives an album without files.
func (c *TelegramClient) SendAlbum(ctx context.Context, chatID string, members []groupMember) error {
	type albumEntry struct {
		Type    string `json:"type"`
		Media   string `json:"media"`
		Caption string `json:"caption,omitempty"`
	}
	entries := make([]albumEntry, 0, len(members))
	files := map[string]uploadFile{}
	for index, member := range members {
		name := "file" + strconv.Itoa(index)
		files[name] = member.file
		entries = append(entries, albumEntry{Type: member.kind, Media: "attach://" + name, Caption: member.caption})
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, "sendMediaGroup", map[string]string{"chat_id": chatID, "media": string(encoded)}, files, uploadTimeout)
	return err
}
