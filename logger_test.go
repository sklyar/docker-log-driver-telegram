package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/daemon/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

var defaultContainerDetails = &ContainerDetails{
	ContainerID:        "9991660fbbe9138909733e9c3038e21335e99c11f5d1be04219ba8a4186d1f96",
	ContainerName:      "/log-generator",
	ContainerImageID:   "sha256:51e60588ff2cd9f45792b23de89bfface0a7fbd711d17c5f5ce900a4f6b16260",
	ContainerImageName: "alpine",
	ContainerCreated:   time.Date(2023, 5, 19, 0, 0, 0, 0, time.UTC),
	DaemonName:         "docker",
}

type mockClient struct {
	mock.Mock
}

func (c *mockClient) SendMessage(_ context.Context, message string) error {
	args := c.Called(message)
	return args.Error(0)
}

// recordingClient stores all sent messages.
type recordingClient struct {
	mu   sync.Mutex
	msgs []string
}

func (c *recordingClient) SendMessage(_ context.Context, message string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.msgs = append(c.msgs, message)
	return nil
}

func (c *recordingClient) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.msgs)
}

// blockingClient waits until the context is done.
type blockingClient struct {
	started chan struct{}
}

func (c *blockingClient) SendMessage(ctx context.Context, _ string) error {
	select {
	case c.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// droppedCount returns the sum of all reported dropped messages.
func droppedCount(logs *observer.ObservedLogs) int64 {
	var n int64
	for _, entry := range logs.FilterMessage("dropped log messages").All() {
		n += entry.ContextMap()["count"].(int64)
	}
	return n
}

func TestTelegramLogger_Log_BatchDisabled(t *testing.T) {
	t.Parallel()

	zapLogger := zap.NewNop()
	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:        "token",
		cfgChatIDKey:       "chat_id",
		cfgBatchEnabledKey: "false",
	}

	client := &mockClient{}
	client.On("SendMessage", "message1").Return(nil)
	client.On("SendMessage", "message2").Return(nil)

	l, err := NewTelegramLogger(zapLogger, &containerDetails)
	require.NoError(t, err)

	l.client = client

	err = l.Log(&logger.Message{Line: []byte("message1")})
	require.NoError(t, err)

	err = l.Log(&logger.Message{Line: []byte("message2")})
	require.NoError(t, err)

	require.NoError(t, l.Close())

	client.AssertExpectations(t)
}

func TestTelegramLogger_Log_Buffer(t *testing.T) {
	t.Parallel()

	zapLogger := zap.NewNop()
	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:              "token",
		cfgChatIDKey:             "chat_id",
		cfgBatchEnabledKey:       "true",
		cfgBatchFlushIntervalKey: "1s",
	}

	// 4 logs with 255 characters each + 1 newline
	logs := generateLogs(5, (defaultLogMessageChars/4)-1)
	joinedLogs := strings.Join(logs[:4], "\n")

	sent := make(chan struct{}, 1)

	client := &mockClient{}
	client.On("SendMessage", joinedLogs).
		Return(nil).
		Run(func(_ mock.Arguments) {
			sent <- struct{}{}
		})
	client.On("SendMessage", logs[4]).
		Return(nil)

	l, err := NewTelegramLogger(zapLogger, &containerDetails)
	require.NoError(t, err)

	l.client = client

	for _, message := range logs {
		err = l.Log(&logger.Message{Line: []byte(message)})
		assert.NoError(t, err)
	}

	waitWithTimeout(t, sent, "timeout waiting for the message to be sent")

	err = l.Close()
	require.NoError(t, err)

	client.AssertExpectations(t)
}

func TestTelegramLogger_Log_Buffer_Drain(t *testing.T) {
	t.Parallel()

	zapLogger := zap.NewNop()
	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:              "token",
		cfgChatIDKey:             "chat_id",
		cfgBatchEnabledKey:       "true",
		cfgBatchFlushIntervalKey: "1m",
	}

	logs := generateLogs(5, 256)

	client := &mockClient{}
	client.On("SendMessage", strings.Join(logs, "\n")).
		Return(nil)

	l, err := NewTelegramLogger(zapLogger, &containerDetails)
	require.NoError(t, err)

	l.client = client

	for _, message := range logs {
		err = l.Log(&logger.Message{Line: []byte(message)})
		assert.NoError(t, err)
	}

	err = l.Close()
	require.NoError(t, err)

	client.AssertExpectations(t)
}

func TestTelegramLogger_Log_Buffer_BatchLimit(t *testing.T) {
	t.Parallel()

	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:              "token",
		cfgChatIDKey:             "chat_id",
		cfgBatchEnabledKey:       "true",
		cfgBatchFlushIntervalKey: "1s",
	}

	const maxChars = 10

	// The lengths are chosen so that some batches are exactly maxChars long
	// and some lines do not fit into the current batch.
	lines := []string{"aaaa", "ббб", "cc", "ddddddd", "eeeeeeeeee", "f", "gggggggg", "hh"}

	client := &recordingClient{}

	l, err := NewTelegramLogger(zap.NewNop(), &containerDetails, WithMaxLogMessageChars(maxChars))
	require.NoError(t, err)

	l.client = client

	for _, line := range lines {
		require.NoError(t, l.Log(&logger.Message{Line: []byte(line)}))
	}

	sentLines := func() []string {
		return strings.Split(strings.Join(client.messages(), "\n"), "\n")
	}

	// The last batch is sent by the flush ticker.
	require.Eventually(t, func() bool {
		return len(sentLines()) >= len(lines)
	}, 3*time.Second, 10*time.Millisecond)

	for _, msg := range client.messages() {
		assert.LessOrEqual(t, utf8.RuneCountInString(msg), maxChars, "message %q is too long", msg)
	}
	assert.Equal(t, lines, sentLines())

	require.NoError(t, l.Close())
}

func TestTelegramLogger_Close_SendsBufferedLines(t *testing.T) {
	t.Parallel()

	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:              "token",
		cfgChatIDKey:             "chat_id",
		cfgBatchEnabledKey:       "true",
		cfgBatchFlushIntervalKey: "1m",
	}

	lines := []string{"line1", "line2", "line3", "line4", "line5"}

	client := &recordingClient{}

	l, err := NewTelegramLogger(zap.NewNop(), &containerDetails)
	require.NoError(t, err)

	l.client = client

	// Close right after logging, so some lines are still in the buffer.
	for _, line := range lines {
		require.NoError(t, l.Log(&logger.Message{Line: []byte(line)}))
	}
	require.NoError(t, l.Close())

	assert.Equal(t, strings.Join(lines, "\n"), strings.Join(client.messages(), "\n"))
}

func TestTelegramLogger_Log_Buffer_Overflow(t *testing.T) {
	t.Parallel()

	core, observedLogs := observer.New(zapcore.WarnLevel)
	zapLogger := zap.New(core)
	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:              "token",
		cfgChatIDKey:             "chat_id",
		cfgBatchEnabledKey:       "true",
		cfgBatchFlushIntervalKey: "1m",
		cfgBufferMessagesKey:     "2",
	}

	client := &mockClient{}

	// This blocks the message processing to test buffer overflow behavior.
	sent1 := make(chan struct{})
	client.On("SendMessage", "0000").
		Return(nil).
		Run(func(_ mock.Arguments) {
			// Notify that message processing has started
			sent1 <- struct{}{}

			// Block until explicitly released, simulating slow processing
			<-sent1
		})

	// Set up second message handler
	// When buffer overflows, messages "2" and "3" will be batched together
	// Message "1" will be dropped due to buffer capacity limit
	sent2 := make(chan struct{})
	client.On("SendMessage", "2\n3").
		Return(nil).
		Run(func(_ mock.Arguments) {
			sent2 <- struct{}{}
		})

	l, err := NewTelegramLogger(
		zapLogger,
		&containerDetails,
		WithMaxLogMessageChars(4),
	)
	require.NoError(t, err)

	l.client = client

	// Send first message.
	err = l.Log(&logger.Message{Line: []byte("0000")})
	require.NoError(t, err)

	// Wait for first message to start processing
	// This ensures the message is in the "sending" state
	<-sent1

	// Send three more messages while first is blocked:
	// - Message "1" will be dropped due to buffer overflow
	// - Messages "2" and "3" will be queued and later batched
	for _, message := range []string{"1", "2", "3"} {
		err = l.Log(&logger.Message{Line: []byte(message)})
		require.NoError(t, err)
	}

	// Unblock the first message processing
	// This allows the batched messages to be processed
	sent1 <- struct{}{}

	waitWithTimeout(t, sent2, "timeout waiting for the second message to be sent")

	client.AssertExpectations(t)
	assert.Equal(t, int64(1), droppedCount(observedLogs))
}

func TestTelegramLoggerLog_Truncate(t *testing.T) {
	t.Parallel()

	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:        "token",
		cfgChatIDKey:       "chat_id",
		cfgBatchEnabledKey: "false",
	}

	longMessage := strings.Repeat("a", defaultLogMessageChars+1)
	want := longMessage[:defaultLogMessageChars-messageLen(truncatedSuffix)] + truncatedSuffix

	client := &recordingClient{}

	l, err := NewTelegramLogger(zap.NewNop(), &containerDetails)
	require.NoError(t, err)

	l.client = client

	require.NoError(t, l.Log(&logger.Message{Line: []byte(longMessage)}))
	require.NoError(t, l.Close())

	assert.Equal(t, []string{want}, client.messages())
	assert.Equal(t, defaultLogMessageChars, messageLen(want))
}

func TestTelegramLoggerLog_InvalidUTF8AndEmptyLines(t *testing.T) {
	t.Parallel()

	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:        "token",
		cfgChatIDKey:       "chat_id",
		cfgBatchEnabledKey: "false",
	}

	client := &recordingClient{}

	l, err := NewTelegramLogger(zap.NewNop(), &containerDetails)
	require.NoError(t, err)

	l.client = client

	for _, line := range []string{"", "  \t", "\xff\xfeabc"} {
		require.NoError(t, l.Log(&logger.Message{Line: []byte(line)}))
	}
	require.NoError(t, l.Close())

	assert.Equal(t, []string{"\uFFFDabc"}, client.messages())
}

func TestTelegramLogger_Close_Timeout(t *testing.T) {
	t.Parallel()

	core, observedLogs := observer.New(zapcore.WarnLevel)
	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:        "token",
		cfgChatIDKey:       "chat_id",
		cfgBatchEnabledKey: "false",
	}

	client := &blockingClient{started: make(chan struct{}, 1)}

	l, err := NewTelegramLogger(zap.New(core), &containerDetails)
	require.NoError(t, err)

	l.client = client
	l.closeTimeout = 50 * time.Millisecond

	for _, line := range []string{"line1", "line2", "line3"} {
		require.NoError(t, l.Log(&logger.Message{Line: []byte(line)}))
	}
	waitWithTimeout(t, client.started, "timeout waiting for the first send")

	start := time.Now()
	require.NoError(t, l.Close())

	assert.Less(t, time.Since(start), 3*time.Second)
	assert.Equal(t, int64(3), droppedCount(observedLogs))
	assert.Len(t, observedLogs.FilterMessage("dropped log messages").All(), 1, "the drops on close must be reported once")
	assert.ErrorIs(t, l.Log(&logger.Message{Line: []byte("late")}), errLoggerClosed)
}

func TestTelegramLogger_LogDuringClose(t *testing.T) {
	t.Parallel()

	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:              "token",
		cfgChatIDKey:             "chat_id",
		cfgBatchFlushIntervalKey: "1m",
		cfgBufferMessagesKey:     "10",
	}

	for range 20 {
		l, err := NewTelegramLogger(zap.NewNop(), &containerDetails)
		require.NoError(t, err)

		l.client = &recordingClient{}

		var wg sync.WaitGroup
		for i := range 4 {
			wg.Go(func() {
				for j := range 100 {
					err := l.Log(&logger.Message{Line: []byte(fmt.Sprintf("%d-%d", i, j))})
					if err != nil {
						assert.ErrorIs(t, err, errLoggerClosed)
						return
					}
				}
			})
		}

		require.NoError(t, l.Close())
		wg.Wait()
	}
}

func TestTelegramLoggerLog_PartialLog(t *testing.T) {
	t.Parallel()

	log1 := &logger.Message{
		Line:         []byte("1"),
		PLogMetaData: &backend.PartialLogMetaData{ID: "group_id"},
	}
	log2 := &logger.Message{
		Line:         []byte("2"),
		PLogMetaData: &backend.PartialLogMetaData{ID: "group_id"},
	}
	log3 := &logger.Message{
		Line:         []byte("3"),
		PLogMetaData: &backend.PartialLogMetaData{ID: "group_id", Last: true},
	}
	assembledLog := &logger.Message{
		Line:         []byte("123"),
		PLogMetaData: &backend.PartialLogMetaData{ID: "group_id"},
	}
	_ = assembledLog

	formatter, err := newMessageFormatter(defaultContainerDetails, nil, "{log}")
	assert.NoError(t, err)

	sent := make(chan struct{})

	client := &mockClient{}
	client.On("SendMessage", string(assembledLog.Line)).
		Return(nil).
		Run(func(_ mock.Arguments) {
			sent <- struct{}{}
		})

	zapLogger := zap.NewNop()
	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:        "token",
		cfgChatIDKey:       "chat_id",
		cfgBatchEnabledKey: "false",
	}

	l, err := NewTelegramLogger(zapLogger, &containerDetails)
	require.NoError(t, err)

	l.client = client
	l.formatter = formatter

	assert.NoError(t, l.Log(log1))
	assert.NoError(t, l.Log(log2))
	assert.NoError(t, l.Log(log3))

	waitWithTimeout(t, sent, "timeout waiting for the message to be sent")

	client.AssertExpectations(t)

	// The driver passes the same message to the json-file logger,
	// so Log must not change it.
	assert.Equal(t, "3", string(log3.Line))
	assert.Equal(t, &backend.PartialLogMetaData{ID: "group_id", Last: true}, log3.PLogMetaData)
}

func TestMessageFormatter(t *testing.T) {
	t.Parallel()

	attrs := map[string]string{
		"custom_attr": "custom_value",
	}

	tests := []struct {
		name        string
		template    string
		expectedErr error
	}{
		{
			name:        "valid template",
			template:    "{log} {timestamp} {container_id} {container_full_id} {container_name} {image_id} {image_full_id} {image_name} {daemon_name} {custom_attr}",
			expectedErr: nil,
		},
		{
			name:        "invalid template",
			template:    "{log} {timestamp} {{container_id} {container_full_id} {container_name} {image_id} {image_full_id} {image_name} {daemon_name} {custom_attr}",
			expectedErr: errUnknownTag,
		},
		{
			name:        "unknown tag",
			template:    "{log} {timestamp} {unknown_tag} {container_id} {container_full_id} {container_name} {image_id} {image_full_id} {image_name} {daemon_name} {custom_attr}",
			expectedErr: errUnknownTag,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := newMessageFormatter(defaultContainerDetails, attrs, tc.template)
			if tc.expectedErr != nil {
				assert.ErrorIs(t, err, tc.expectedErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestMessageFormatterFormat(t *testing.T) {
	t.Parallel()

	attrs := map[string]string{
		"custom_attr": "custom_value",
	}
	template := "{log} {timestamp} {container_id} {container_full_id} {container_name} {image_id} {image_full_id} {image_name} {daemon_name} {custom_attr}"

	formatter, err := newMessageFormatter(defaultContainerDetails, attrs, template)
	assert.NoError(t, err)

	now := time.Now()
	msg := &logger.Message{
		Line:      []byte("Test log message"),
		Timestamp: now,
	}

	formattedMessage := formatter.Format(msg)
	assert.Contains(t, formattedMessage, string(msg.Line))
	assert.Contains(t, formattedMessage, now.UTC().Format(time.RFC3339))
	assert.Contains(t, formattedMessage, defaultContainerDetails.ID())
	assert.Contains(t, formattedMessage, defaultContainerDetails.ContainerID)
	assert.Contains(t, formattedMessage, defaultContainerDetails.Name())
	assert.Contains(t, formattedMessage, defaultContainerDetails.ImageID())
	assert.Contains(t, formattedMessage, defaultContainerDetails.ContainerImageID)
	assert.Contains(t, formattedMessage, defaultContainerDetails.ImageName())
	assert.Contains(t, formattedMessage, defaultContainerDetails.DaemonName)
	assert.Contains(t, formattedMessage, attrs["custom_attr"])
}

func TestPartialLogBuffer(t *testing.T) {
	t.Parallel()

	b := newPartialLogBuffer()

	assembledLog := &logger.Message{
		Line:         []byte("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua."),
		Timestamp:    time.Now(),
		PLogMetaData: &backend.PartialLogMetaData{ID: "group_id"},
	}

	log, last := b.Append(
		&logger.Message{
			Line:         []byte("Lorem ipsum dolor sit amet, "),
			Timestamp:    assembledLog.Timestamp,
			PLogMetaData: &backend.PartialLogMetaData{ID: "group_id"},
		},
	)
	require.False(t, last)
	require.Nil(t, log)

	log, last = b.Append(
		&logger.Message{
			Line:         []byte("consectetur adipiscing elit, "),
			Timestamp:    time.Now(),
			PLogMetaData: &backend.PartialLogMetaData{ID: "group_id"},
		},
	)
	require.False(t, last)
	require.Nil(t, log)

	log, last = b.Append(
		&logger.Message{
			Line:         []byte("sed do eiusmod tempor incididunt ut labore et dolore magna aliqua."),
			Timestamp:    time.Now(),
			PLogMetaData: &backend.PartialLogMetaData{ID: "group_id", Last: true},
		},
	)
	require.True(t, last)
	require.NotNil(t, log)

	assert.Equal(t, string(assembledLog.Line), string(log.Line))
	assert.Equal(t, assembledLog.Timestamp, log.Timestamp)

	// check delete log after assembled chunks
	log, last = b.Append(
		&logger.Message{
			Line:         []byte("must be first"),
			Timestamp:    time.Now(),
			PLogMetaData: &backend.PartialLogMetaData{ID: "group_id"},
		},
	)
	require.False(t, last)
	require.Nil(t, log)
}

func TestPartialLogBuffer_LimitsLineSize(t *testing.T) {
	t.Parallel()

	b := newPartialLogBuffer()
	chunk := []byte(strings.Repeat("a", 16*1024))

	for range 10 {
		log, last := b.Append(&logger.Message{Line: chunk, PLogMetaData: &backend.PartialLogMetaData{ID: "id"}})
		require.False(t, last)
		require.Nil(t, log)
	}

	log, last := b.Append(&logger.Message{Line: chunk, PLogMetaData: &backend.PartialLogMetaData{ID: "id", Last: true}})
	require.True(t, last)
	assert.Len(t, log.Line, maxLineBytes)
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		text   string
		maxLen int
		want   string
	}{
		{"short", "abc", 20, "abc"},
		{"exact", strings.Repeat("a", 20), 20, strings.Repeat("a", 20)},
		{"long", strings.Repeat("a", 21), 20, strings.Repeat("a", 7) + truncatedSuffix},
		{"multibyte", strings.Repeat("ж", 21), 20, strings.Repeat("ж", 7) + truncatedSuffix},
		{"emoji counts as two", strings.Repeat("😀", 11), 20, strings.Repeat("😀", 3) + truncatedSuffix},
		{"no room for the suffix", "abcdef", 4, "abcd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := truncate(tt.text, tt.maxLen)
			assert.Equal(t, tt.want, got)
			assert.LessOrEqual(t, messageLen(got), tt.maxLen)
		})
	}
}

func generateLogs(count, size int) []string {
	logs := make([]string, count)
	for i := range count {
		logs[i] = strings.Repeat("a", size)
	}
	return logs
}

// waitWithTimeout waits for done for at most 3 seconds.
func waitWithTimeout(t *testing.T, done chan struct{}, args ...any) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal(args...)
	}
}
