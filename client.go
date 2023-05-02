package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/ratelimit"
	"go.uber.org/zap"
)

const (
	// In case of an error, the Client will retry sending the message.
	retryInterval = 30 * time.Second

	// Telegram API limits.
	msgsPerMinute = 20
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

	// Timeout is the timeout for the HTTP Client.
	// Valid time units are "ns", "us" (or "µs"), "ms", "s", "m", "h".
	Timeout time.Duration
}

func (c ClientConfig) Validate() error {
	var errs []error

	if c.APIURL == "" {
		errs = append(errs, errors.New("API URL is required"))
	}
	if c.Token == "" {
		errs = append(errs, errors.New("token is required"))
	}
	if c.ChatID == "" {
		errs = append(errs, errors.New("chat ID is required"))
	}

	return errors.Join(errs...)
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
	baseURL    *url.URL

	cfg ClientConfig

	limiter ratelimit.Limiter
	logger  *zap.Logger

	// sleep is used to mock time.Sleep in tests.
	sleep func(time.Duration)
}

// NewClient creates a new Telegram client.
func NewClient(logger *zap.Logger, cfg ClientConfig, limiterOpts ...ratelimit.Option) (*Client, error) {
	baseURL, err := url.Parse(cfg.APIURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse API URL: %w", err)
	}

	limiterOpts = append(limiterOpts, ratelimit.Per(time.Minute), ratelimit.WithoutSlack)
	limiter := ratelimit.New(msgsPerMinute, limiterOpts...)

	return &Client{
		httpClient: &http.Client{
			Timeout: time.Second * 10,
		},
		baseURL: baseURL,
		cfg:     cfg,
		limiter: limiter,
		logger:  logger,
		sleep:   time.Sleep,
	}, nil
}

// SendMessage sends a message to a Telegram chat.
func (c *Client) SendMessage(text string) error {
	c.limiter.Take()

	u := c.baseURL
	u.Path = fmt.Sprintf("/bot%s/sendMessage", c.cfg.Token)

	// Values for form data.
	values := url.Values{
		"chat_id": {c.cfg.ChatID},
		"text":    {text},
	}

	var lastErr error
	for attempt := 0; attempt < c.cfg.Retries+1; attempt++ {
		resp, err := c.sendMessage(u.String(), values)
		if err != nil {
			if errors.Is(err, errBadResponse) {
				lastErr = err
				continue
			}
			return fmt.Errorf("failed to send message: %w", err)
		}

		if resp.Ok {
			return nil
		}

		switch resp.ErrorCode {
		case http.StatusTooManyRequests:
			lastErr = fmt.Errorf("%s: %w", resp.Description, errRateLimited)

			retryAfter := retryInterval
			if resp.Parameters != nil {
				retryAfter = time.Duration(resp.Parameters.RetryAfter) * time.Second
			}

			c.logger.Debug("Telegram API rate limit exceeded",
				zap.Duration("retry_after", retryAfter),
			)

			if attempt < c.cfg.Retries {
				c.sleep(retryAfter)
			}
		default:
			return fmt.Errorf("%s: %w", resp.Description, errAPIError)
		}
	}

	return fmt.Errorf("max attempts exceeded: %w", lastErr)
}

func (c *Client) Ping() error {
	u := c.baseURL
	u.Path = fmt.Sprintf("/bot%s/getChat", c.cfg.Token)

	values := url.Values{
		"chat_id": {c.cfg.ChatID},
	}

	var lastErr error
	for attempt := 0; attempt < c.cfg.Retries+1; attempt++ {
		resp, err := c.sendMessage(u.String(), values)
		if err != nil {
			if errors.Is(err, errBadResponse) {
				lastErr = err
				continue
			}
			return fmt.Errorf("failed to send message: %w", err)
		}
		if resp.Ok {
			return nil
		}

		lastErr = fmt.Errorf("%s: %w", resp.Description, errAPIError)
	}

	return fmt.Errorf("max attempts exceeded: %w", lastErr)
}

func (c *Client) sendMessage(url string, values url.Values) (*clientResponse, error) {
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	var body clientResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", errors.Join(err, errBadResponse))
	}

	return &body, nil
}
