package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"golang.org/x/time/rate"
)

const testToken = "123:SECRET"

// testServer is a fake Telegram API. It returns the responses in order
// and then repeats the last one.
type testServer struct {
	*httptest.Server

	calls atomic.Int32

	mu    sync.Mutex
	paths []string
}

func newTestServer(t *testing.T, responses ...string) *testServer {
	t.Helper()

	s := &testServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(s.calls.Add(1))

		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.mu.Unlock()

		assert.NoError(t, r.ParseForm())
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "456", r.Form.Get("chat_id"))
		assert.Equal(t, "text", r.Form.Get("text"))

		w.Header().Set("Content-Type", "application/json")
		_, err := fmt.Fprintln(w, responses[min(n, len(responses))-1])
		assert.NoError(t, err)
	}))
	t.Cleanup(s.Close)

	return s
}

// requestPaths returns the paths of all requests.
func (s *testServer) requestPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.paths...)
}

// newTestClient creates a client without a rate limit. The client does not
// sleep between attempts, but records the delays.
func newTestClient(t *testing.T, apiURL string, retries int) (*Client, *[]time.Duration) {
	t.Helper()

	cfg := ClientConfig{
		APIURL:  apiURL,
		Token:   testToken,
		ChatID:  "456",
		Retries: retries,
		Timeout: time.Second,
	}
	c, err := NewClient(zaptest.NewLogger(t), cfg, rate.NewLimiter(rate.Inf, 1))
	require.NoError(t, err)

	var delays []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}

	return c, &delays
}

const (
	respOK          = `{"ok":true}`
	respBadRequest  = `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`
	respServerError = `{"ok":false,"error_code":500,"description":"Internal Server Error"}`
	respRateLimited = `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`
)

func TestSendMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		responses  []string
		retries    int
		wantErr    error
		wantCalls  int
		wantDelays []time.Duration
	}{
		{
			name:      "ok",
			responses: []string{respOK},
			retries:   3,
			wantCalls: 1,
		},
		{
			name:      "telegram error is not retried",
			responses: []string{respBadRequest},
			retries:   3,
			wantErr:   errAPIError,
			wantCalls: 1,
		},
		{
			name:       "bad response is retried",
			responses:  []string{`bad response`, respOK},
			retries:    3,
			wantCalls:  2,
			wantDelays: []time.Duration{time.Second},
		},
		{
			name:       "server error is retried with a growing delay",
			responses:  []string{respServerError, respServerError, respServerError, respOK},
			retries:    3,
			wantCalls:  4,
			wantDelays: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		},
		{
			name:       "rate limit waits for retry_after",
			responses:  []string{respRateLimited, respOK},
			retries:    3,
			wantCalls:  2,
			wantDelays: []time.Duration{7 * time.Second},
		},
		{
			name:       "max attempts exceeded",
			responses:  []string{respRateLimited},
			retries:    2,
			wantErr:    errRateLimited,
			wantCalls:  3,
			wantDelays: []time.Duration{7 * time.Second, 7 * time.Second},
		},
		{
			name:      "no retries",
			responses: []string{respServerError},
			retries:   0,
			wantErr:   errAPIError,
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newTestServer(t, tt.responses...)
			c, delays := newTestClient(t, srv.URL, tt.retries)

			err := c.SendMessage(context.Background(), "text")
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.wantCalls, int(srv.calls.Load()))
			assert.Equal(t, tt.wantDelays, *delays)
			assert.Equal(t, "/bot"+testToken+"/sendMessage", srv.requestPaths()[0])
		})
	}
}

func TestSendMessage_KeepsURLPath(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, respOK)
	c, _ := newTestClient(t, srv.URL+"/telegram/", 0)

	require.NoError(t, c.SendMessage(context.Background(), "text"))
	assert.Equal(t, []string{"/telegram/bot" + testToken + "/sendMessage"}, srv.requestPaths())
}

func TestSendMessage_TopicID(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "456", r.Form.Get("chat_id"))
		assert.Equal(t, "789", r.Form.Get("message_thread_id"))

		w.Header().Set("Content-Type", "application/json")
		_, err := fmt.Fprintln(w, respOK)
		assert.NoError(t, err)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv.URL, 0)
	c.cfg.TopicID = "789"

	require.NoError(t, c.SendMessage(context.Background(), "text"))
}

func TestSendMessage_NetworkError(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)

		conn, _, err := w.(http.Hijacker).Hijack()
		assert.NoError(t, err)
		_ = conn.Close()
	}))
	defer srv.Close()

	c, delays := newTestClient(t, srv.URL, 2)

	err := c.SendMessage(context.Background(), "text")
	require.Error(t, err)
	assert.ErrorContains(t, err, "max attempts exceeded")
	assert.NotContains(t, err.Error(), testToken, "the error must not contain the bot token")
	assert.Equal(t, 3, int(calls.Load()))
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second}, *delays)
}

func TestSendMessage_StopsWhenContextIsDone(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		cancel()

		w.Header().Set("Content-Type", "application/json")
		_, err := fmt.Fprintln(w, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":60}}`)
		assert.NoError(t, err)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv.URL, 5)
	c.sleep = sleepContext

	start := time.Now()
	err := c.SendMessage(ctx, "text")

	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, 1, int(calls.Load()))
}

func TestNewClient_InvalidConfig(t *testing.T) {
	t.Parallel()

	valid := ClientConfig{
		APIURL:  "https://api.telegram.org",
		Token:   testToken,
		ChatID:  "456",
		Retries: 1,
		Timeout: time.Second,
	}

	tests := []struct {
		name    string
		change  func(*ClientConfig)
		wantErr string
	}{
		{"no scheme", func(c *ClientConfig) { c.APIURL = "api.telegram.org" }, "the scheme must be http or https"},
		{"unsupported scheme", func(c *ClientConfig) { c.APIURL = "ftp://api.telegram.org" }, "the scheme must be http or https"},
		{"no host", func(c *ClientConfig) { c.APIURL = "https:///path" }, "the host is required"},
		{"no token", func(c *ClientConfig) { c.Token = "" }, "token is required"},
		{"no chat", func(c *ClientConfig) { c.ChatID = "" }, "chat ID is required"},
		{"zero timeout", func(c *ClientConfig) { c.Timeout = 0 }, "timeout must be positive"},
		{"negative retries", func(c *ClientConfig) { c.Retries = -1 }, "retries must not be negative"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := valid
			tt.change(&cfg)

			_, err := NewClient(zaptest.NewLogger(t), cfg, nil)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestChatLimiters(t *testing.T) {
	t.Parallel()

	limiters := newChatLimiters()

	assert.Same(t, limiters.get("bot", "chat1"), limiters.get("bot", "chat1"))
	assert.NotSame(t, limiters.get("bot", "chat1"), limiters.get("bot", "chat2"))
	assert.NotSame(t, limiters.get("bot1", "chat"), limiters.get("bot2", "chat"))
}
