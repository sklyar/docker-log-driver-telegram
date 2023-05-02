package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"
)

func TestSendMessage(t *testing.T) {
	t.Parallel()

	defaultConfig := ClientConfig{
		Token:   "123",
		ChatID:  "456",
		Retries: 0,
		Timeout: 10 * time.Millisecond,
	}

	newServer := func(response string) *httptest.Server {
		t.Helper()

		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			err := r.ParseForm()
			assert.NoError(t, err)

			assert.Equal(t, "POST", r.Method)
			assert.Equal(t, "/bot123/sendMessage", r.URL.Path)
			assert.Equal(t, "456", r.Form.Get("chat_id"))
			assert.Equal(t, "text", r.Form.Get("text"))

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, response)
		}))
	}

	t.Run("ok", func(t *testing.T) {
		t.Parallel()

		srv := newServer(`{"ok":true}`)
		defer srv.Close()

		logger := zaptest.NewLogger(t)
		cfg := defaultConfig
		cfg.APIURL = srv.URL

		c, err := NewClient(logger, cfg)
		assert.NoError(t, err)

		err = c.SendMessage("text")
		assert.NoError(t, err)
	})

	t.Run("bad response", func(t *testing.T) {
		t.Parallel()

		srv := newServer(`bad response`)
		defer srv.Close()

		logger := zaptest.NewLogger(t)
		cfg := defaultConfig
		cfg.APIURL = srv.URL

		c, err := NewClient(logger, cfg)
		assert.NoError(t, err)

		err = c.SendMessage("text")
		assert.ErrorIs(t, err, errBadResponse)
	})

	t.Run("telegram error", func(t *testing.T) {
		t.Parallel()

		srv := newServer(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
		defer srv.Close()

		logger := zaptest.NewLogger(t)
		cfg := defaultConfig
		cfg.APIURL = srv.URL

		c, err := NewClient(logger, cfg)
		assert.NoError(t, err)

		err = c.SendMessage("text")
		assert.ErrorIs(t, err, errAPIError)
	})

	t.Run("retries", func(t *testing.T) {
		t.Parallel()

		attempts := 0
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++

			if attempts <= 2 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprintln(w, `
					{
						"ok": false,
						"error_code": 429,
						"description": "Too Many Requests: retry after 1",
						"parameters": {"retry_after": 1}
					}
				`)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"ok": true}`)
		}))
		defer ts.Close()

		logger := zaptest.NewLogger(t)
		cfg := ClientConfig{
			APIURL:  ts.URL,
			Token:   "123",
			ChatID:  "456",
			Retries: 3,
			Timeout: 10 * time.Millisecond,
		}

		c, err := NewClient(logger, cfg)
		assert.NoError(t, err)

		var sleepTime time.Duration
		c.sleep = func(d time.Duration) {
			sleepTime += d
		}

		err = c.SendMessage("text")
		assert.NoError(t, err)

		assert.Equal(t, 3, attempts)
		assert.Equal(t, 2*time.Second, sleepTime)
	})

	t.Run("max attempts exceeded", func(t *testing.T) {
		t.Parallel()

		calls := 0
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintln(w, `
				{
					"ok": false,
					"error_code": 429,
					"description": "Too Many Requests: retry after 1",
					"parameters": {"retry_after": 1}
				}
			`)
		}))
		defer ts.Close()

		logger := zaptest.NewLogger(t)
		cfg := ClientConfig{
			APIURL:  ts.URL,
			Token:   "123",
			ChatID:  "456",
			Retries: 3,
			Timeout: 10 * time.Millisecond,
		}

		c, err := NewClient(logger, cfg)
		assert.NoError(t, err)

		var sleepTime time.Duration
		c.sleep = func(d time.Duration) {
			sleepTime += d
		}

		err = c.SendMessage("text")
		assert.ErrorContains(t, err, "max attempts exceeded")
		assert.Equal(t, 4, cfg.Retries+1)
		assert.Equal(t, 3*time.Second, sleepTime)
	})

}

func TestPing(t *testing.T) {
	t.Parallel()

	defaultConfig := ClientConfig{
		Token:   "123",
		ChatID:  "456",
		Retries: 0,
		Timeout: 10 * time.Millisecond,
	}

	newServer := func(response string) *httptest.Server {
		t.Helper()

		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			err := r.ParseForm()
			assert.NoError(t, err)

			assert.Equal(t, "POST", r.Method, "Expected method 'POST', got '%s'", r.Method)
			assert.Equal(t, "/bot123/getChat", r.URL.Path, "Expected path '/bot123/getChat', got '%s'", r.URL.Path)
			assert.Equal(t, defaultConfig.ChatID, r.Form.Get("chat_id"))

			fmt.Fprintln(w, response)
		}))
	}

	t.Run("ok", func(t *testing.T) {
		t.Parallel()

		srv := newServer(`{"ok":true}`)
		defer srv.Close()

		logger := zaptest.NewLogger(t)
		cfg := defaultConfig
		cfg.APIURL = srv.URL

		c, err := NewClient(logger, cfg)
		assert.NoError(t, err)

		err = c.Ping()
		assert.NoError(t, err)
	})

	t.Run("bad response", func(t *testing.T) {
		t.Parallel()

		srv := newServer(`bad response`)
		defer srv.Close()

		logger := zaptest.NewLogger(t)
		cfg := defaultConfig
		cfg.APIURL = srv.URL

		c, err := NewClient(logger, cfg)
		assert.NoError(t, err)

		err = c.Ping()
		assert.ErrorIs(t, err, errBadResponse)
	})

	t.Run("telegram error", func(t *testing.T) {
		t.Parallel()

		srv := newServer(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
		defer srv.Close()

		logger := zaptest.NewLogger(t)
		cfg := defaultConfig
		cfg.APIURL = srv.URL

		c, err := NewClient(logger, cfg)
		assert.NoError(t, err)

		err = c.Ping()
		assert.ErrorIs(t, err, errAPIError)
	})

	t.Run("max attempts exceeded", func(t *testing.T) {
		t.Parallel()

		attempts := 0
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintln(w, `
				{
					"ok": false,
					"error_code": 429,
					"description": "Too Many Requests: retry after 1",
					"parameters": {"retry_after": 1}
				}
			`)
		}))
		defer ts.Close()

		logger := zaptest.NewLogger(t)
		cfg := ClientConfig{
			APIURL:  ts.URL,
			Token:   "123",
			ChatID:  "456",
			Retries: 3,
			Timeout: 10 * time.Millisecond,
		}

		c, err := NewClient(logger, cfg)
		assert.NoError(t, err)

		err = c.Ping()
		assert.ErrorContains(t, err, "max attempts exceeded")
		assert.Equal(t, 4, attempts)
	})
}
