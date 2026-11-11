package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

const (
	// msgsPerMinute is the Telegram limit for messages to one group chat.
	msgsPerMinute = 20

	// minRetryDelay and maxRetryDelay limit the delay between attempts
	// when Telegram does not say how long to wait.
	minRetryDelay = time.Second
	maxRetryDelay = 30 * time.Second

	// maxResponseSize limits the size of a Telegram API response.
	maxResponseSize = 1 << 20
)

var (
	errBadResponse = errors.New("bad response")
	errAPIError    = errors.New("API error")
	errRateLimited = errors.New("rate limited")
)

type ClientConfig struct {
	// APIURL is the Telegram API URL. Optional.
	APIURL string

	// Token is the Telegram Bot API token.
	Token string

	// ChatID is the Telegram chat ID.
	ChatID string

	// Retries is the number of retries to call the Telegram API.
	Retries int

	// Timeout is the timeout for one request to the Telegram API.
	// Valid time units are "ns", "us" (or "µs"), "ms", "s", "m", "h".
	Timeout time.Duration
}

func (c ClientConfig) Validate() error {
	var errs []error

	if c.APIURL == "" {
		errs = append(errs, errors.New("API URL is required"))
	} else if err := validateAPIURL(c.APIURL); err != nil {
		errs = append(errs, err)
	}
	if c.Token == "" {
		errs = append(errs, errors.New("token is required"))
	}
	if c.ChatID == "" {
		errs = append(errs, errors.New("chat ID is required"))
	}
	if c.Retries < 0 {
		errs = append(errs, errors.New("retries must not be negative"))
	}
	if c.Timeout <= 0 {
		errs = append(errs, errors.New("timeout must be positive"))
	}

	return errors.Join(errs...)
}

func validateAPIURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid API URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid API URL %q: the scheme must be http or https", rawURL)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid API URL %q: the host is required", rawURL)
	}
	return nil
}

// clientResponse is the Telegram API response.
type clientResponse struct {
	Ok          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`

	Parameters *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// Client is a Telegram client.
// It is used to send messages to a Telegram chat.
type Client struct {
	httpClient *http.Client

	// endpoint is the sendMessage URL. It contains the bot token,
	// so it must not get into errors or logs.
	endpoint string

	cfg ClientConfig

	limiter *rate.Limiter
	logger  *zap.Logger

	// sleep waits for d or until ctx is done. Tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

// NewClient creates a new Telegram client. The limiter limits the message
// rate. If it is nil, the client uses its own limiter.
func NewClient(logger *zap.Logger, cfg ClientConfig, limiter *rate.Limiter) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	baseURL, err := url.Parse(cfg.APIURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse API URL: %w", err)
	}

	if limiter == nil {
		limiter = newChatLimiter()
	}

	return &Client{
		httpClient: &http.Client{},
		endpoint:   baseURL.JoinPath(url.PathEscape("bot"+cfg.Token), "sendMessage").String(),
		cfg:        cfg,
		limiter:    limiter,
		logger:     logger,
		sleep:      sleepContext,
	}, nil
}

// SendMessage sends a message to the Telegram chat.
//
// It retries network errors, bad responses and server errors with a growing
// delay, and rate limit errors after the delay that Telegram asks for.
// It stops when ctx is done.
func (c *Client) SendMessage(ctx context.Context, text string) error {
	values := url.Values{
		"chat_id": {c.cfg.ChatID},
		"text":    {text},
	}

	var (
		lastErr error
		delay   time.Duration
	)
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, delay); err != nil {
				return errors.Join(err, lastErr)
			}
		}

		if err := c.limiter.Wait(ctx); err != nil {
			return errors.Join(fmt.Errorf("failed to wait for the rate limiter: %w", err), lastErr)
		}

		err := c.send(ctx, values)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), err)
		}

		var retryErr *retryError
		if !errors.As(err, &retryErr) {
			return err
		}
		lastErr = retryErr.err

		delay = retryErr.delay
		if delay == 0 {
			delay = retryDelay(attempt)
		}

		c.logger.Debug("Telegram API request failed",
			zap.Int("attempt", attempt+1),
			zap.Duration("retry_after", delay),
			zap.Error(lastErr),
		)
	}

	return fmt.Errorf("max attempts exceeded: %w", lastErr)
}

// retryError is an error after which the request can be repeated.
type retryError struct {
	err error

	// delay is how long to wait before the next attempt.
	// Zero means that the client chooses the delay.
	delay time.Duration
}

func (e *retryError) Error() string { return e.err.Error() }
func (e *retryError) Unwrap() error { return e.err }

// retryDelay returns the delay before the next attempt: 1s, 2s, 4s and so on,
// up to maxRetryDelay.
func retryDelay(attempt int) time.Duration {
	if attempt >= 5 {
		return maxRetryDelay
	}
	return min(minRetryDelay<<attempt, maxRetryDelay)
}

// send makes one request to the Telegram API.
func (c *Client) send(ctx context.Context, values url.Values) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", redactURL(err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &retryError{err: fmt.Errorf("failed to send request: %w", redactURL(err))}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	var body clientResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize)).Decode(&body); err != nil {
		return &retryError{err: fmt.Errorf("failed to decode response with HTTP status %d: %w",
			resp.StatusCode, errors.Join(err, errBadResponse))}
	}

	switch {
	case body.Ok:
		return nil
	case body.ErrorCode == http.StatusTooManyRequests:
		var delay time.Duration
		if body.Parameters != nil {
			delay = time.Duration(body.Parameters.RetryAfter) * time.Second
		}
		return &retryError{err: fmt.Errorf("%s: %w", body.Description, errRateLimited), delay: delay}
	case body.ErrorCode >= http.StatusInternalServerError:
		return &retryError{err: fmt.Errorf("%s: %w", body.Description, errAPIError)}
	default:
		return fmt.Errorf("%s: %w", body.Description, errAPIError)
	}
}

// redactURL removes the request URL from err, because the URL contains
// the bot token.
func redactURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// chatLimiters gives one rate limiter for each bot and chat. Telegram limits
// the message rate per chat, so all containers that send messages to the same
// chat must share one limiter.
type chatLimiters struct {
	mu       sync.Mutex
	limiters map[chatKey]*rate.Limiter
}

type chatKey struct {
	token  string
	chatID string
}

func newChatLimiters() *chatLimiters {
	return &chatLimiters{limiters: make(map[chatKey]*rate.Limiter)}
}

// get returns the limiter for the bot and chat.
func (c *chatLimiters) get(token, chatID string) *rate.Limiter {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := chatKey{token: token, chatID: chatID}
	limiter, ok := c.limiters[key]
	if !ok {
		limiter = newChatLimiter()
		c.limiters[key] = limiter
	}
	return limiter
}

func newChatLimiter() *rate.Limiter {
	return rate.NewLimiter(rate.Every(time.Minute/msgsPerMinute), 1)
}
