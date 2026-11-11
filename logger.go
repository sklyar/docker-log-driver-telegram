package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/daemon/logger"
	"github.com/valyala/fasttemplate"
	"go.uber.org/zap"
)

const (
	// driverName is the name of the driver.
	driverName = "telegram"

	// defaultLogMessageChars is the default maximum length of a Telegram message.
	defaultLogMessageChars = 4096

	// defaultCloseTimeout limits how long Close sends the buffered messages.
	// Docker waits 30 seconds for the StopLogging request by default.
	defaultCloseTimeout = 15 * time.Second

	// maxLineBytes limits the size of a line assembled from partial messages.
	// The message text is truncated to one Telegram message anyway, and
	// a line without a limit could use all memory of the plugin.
	maxLineBytes = 64 * 1024

	// truncatedSuffix marks a message that was truncated.
	truncatedSuffix = "… (truncated)"
)

var (
	errUnknownTag   = errors.New("unknown tag")
	errLoggerClosed = errors.New("logger is closed")
)

// client is an interface that represents a Telegram client.
type client interface {
	SendMessage(ctx context.Context, message string) error
}

// TelegramLoggerOption is a function that configures a TelegramLogger.
type TelegramLoggerOption func(*TelegramLogger)

// WithMaxLogMessageChars sets the maximum length of a Telegram message.
func WithMaxLogMessageChars(maxLen int) TelegramLoggerOption {
	return func(l *TelegramLogger) {
		l.maxLogMessageChars = maxLen
	}
}

// WithChatLimiters makes the logger use rate limiters that are shared
// with other loggers.
func WithChatLimiters(limiters *chatLimiters) TelegramLoggerOption {
	return func(l *TelegramLogger) {
		l.limiters = limiters
	}
}

// TelegramLogger is a logger that sends logs to Telegram.
// It implements the logger.Logger interface.
//
// Log puts messages into a buffer, and one goroutine sends them.
// When the buffer is full, Log drops the oldest message.
type TelegramLogger struct {
	client   client
	limiters *chatLimiters

	formatter          *messageFormatter
	cfg                *loggerConfig
	maxLogMessageChars int
	closeTimeout       time.Duration

	partialLogsBuffer *partialLogBuffer

	// mu makes the closed check and the write to buffer one step,
	// so that no message gets into the buffer after Close.
	mu     sync.Mutex
	closed bool
	buffer chan string

	// stop is closed by Close. Then the sending goroutine sends
	// the buffered messages and closes done.
	stop chan struct{}
	done chan struct{}

	// ctx is canceled when the close timeout expires.
	// It stops the message that is being sent.
	ctx    context.Context
	cancel context.CancelFunc

	// dropped counts the messages that were dropped and not reported yet.
	dropped atomic.Int64

	logger *zap.Logger
}

var _ logger.Logger = (*TelegramLogger)(nil)

// NewTelegramLogger creates a new TelegramLogger.
func NewTelegramLogger(
	zapLogger *zap.Logger,
	containerDetails *ContainerDetails,
	opts ...TelegramLoggerOption,
) (*TelegramLogger, error) {
	cfg, err := parseLoggerConfig(containerDetails)
	if err != nil {
		return nil, fmt.Errorf("failed to parse logger config: %w", err)
	}

	formatter, err := newMessageFormatter(containerDetails, cfg.Attrs, cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("failed to create message formatter: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	l := &TelegramLogger{
		formatter:          formatter,
		cfg:                cfg,
		maxLogMessageChars: defaultLogMessageChars,
		closeTimeout:       defaultCloseTimeout,
		partialLogsBuffer:  newPartialLogBuffer(),
		buffer:             make(chan string, cfg.BufferMessages),
		stop:               make(chan struct{}),
		done:               make(chan struct{}),
		ctx:                ctx,
		cancel:             cancel,
		logger:             zapLogger,
	}

	for _, opt := range opts {
		opt(l)
	}

	if l.limiters == nil {
		l.limiters = newChatLimiters()
	}
	limiter := l.limiters.get(cfg.ClientConfig.Token, cfg.ClientConfig.ChatID)

	l.client, err = NewClient(zapLogger, cfg.ClientConfig, limiter)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create Telegram Client: %w", err)
	}

	go l.run()

	return l, nil
}

// Name implements the logger.Logger interface.
func (l *TelegramLogger) Name() string {
	return driverName
}

// Log implements the logger.Logger interface.
// It does not change msg, because the caller passes msg to other loggers too.
func (l *TelegramLogger) Log(msg *logger.Message) error {
	if msg.PLogMetaData != nil {
		assembledLog, last := l.partialLogsBuffer.Append(msg)
		if !last {
			return nil
		}
		msg = assembledLog
	}

	if l.cfg.FilterRegex != nil && !l.cfg.FilterRegex.Match(msg.Line) {
		l.logger.Debug("message is filtered out by regex", zap.String("regex", l.cfg.FilterRegex.String()))
		return nil
	}

	// Telegram accepts only valid UTF-8 and rejects empty messages.
	text := strings.ToValidUTF8(l.formatter.Format(msg), "�")
	if strings.TrimSpace(text) == "" {
		return nil
	}

	return l.enqueue(truncate(text, l.maxLogMessageChars))
}

func (l *TelegramLogger) enqueue(text string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return errLoggerClosed
	}

	select {
	case l.buffer <- text:
		return nil
	default:
	}

	// The buffer is full. Drop the oldest message to keep the newest ones.
	select {
	case <-l.buffer:
		l.dropped.Add(1)
	default:
	}

	// Only enqueue writes to the buffer, and it holds l.mu,
	// so the buffer has room for the message now.
	l.buffer <- text

	return nil
}

// run sends the buffered messages until Close is called.
func (l *TelegramLogger) run() {
	defer close(l.done)

	var flushTick <-chan time.Time
	if l.cfg.BatchEnabled {
		ticker := time.NewTicker(l.cfg.BatchFlushInterval)
		defer ticker.Stop()
		flushTick = ticker.C
	}

	var b batch

	for {
		select {
		case text := <-l.buffer:
			l.add(&b, text)
		case <-flushTick:
			l.flush(&b)
		case <-l.stop:
			// No new messages get into the buffer after Close,
			// so send the messages that are left and exit.
			l.drain(&b)
			l.flush(&b)
			l.reportDropped()
			return
		}
	}
}

// drain adds the messages that are left in the buffer to the batch.
func (l *TelegramLogger) drain(b *batch) {
	for {
		select {
		case text := <-l.buffer:
			l.add(b, text)
		default:
			return
		}
	}
}

// batch joins messages with newlines into one Telegram message.
type batch struct {
	text  strings.Builder
	len   int // The length of text as messageLen counts it.
	lines int
}

// add adds a message to the batch. It sends the batch first if the message
// does not fit into it, and sends the batch right away if batching is off.
func (l *TelegramLogger) add(b *batch, text string) {
	textLen := messageLen(text)

	if b.lines > 0 && b.len+1+textLen > l.maxLogMessageChars {
		l.flush(b)
	}

	if b.lines > 0 {
		b.text.WriteByte('\n')
		b.len++
	}
	b.text.WriteString(text)
	b.len += textLen
	b.lines++

	// A newline and at least one character do not fit any more.
	if !l.cfg.BatchEnabled || b.len+2 > l.maxLogMessageChars {
		l.flush(b)
	}
}

// flush sends the batch and makes it empty.
func (l *TelegramLogger) flush(b *batch) {
	if b.lines == 0 {
		return
	}

	text, lines := b.text.String(), b.lines
	b.text.Reset()
	b.len = 0
	b.lines = 0

	l.reportDropped()

	if err := l.client.SendMessage(l.ctx, text); err != nil {
		if l.ctx.Err() != nil {
			// The close timeout expired.
			l.dropped.Add(int64(lines))
			return
		}
		l.logger.Error("failed to send log message", zap.Error(err))
	}
}

// reportDropped logs how many messages were dropped since the last report.
func (l *TelegramLogger) reportDropped() {
	if n := l.dropped.Swap(0); n > 0 {
		l.logger.Warn("dropped log messages", zap.Int64("count", n))
	}
}

// Close implements the logger.Logger interface.
// It sends the buffered messages, but waits for them for at most
// closeTimeout. The messages that are not sent by then are dropped.
func (l *TelegramLogger) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()

	close(l.stop)

	timer := time.NewTimer(l.closeTimeout)
	defer timer.Stop()

	select {
	case <-l.done:
	case <-timer.C:
		l.logger.Warn("timeout while sending buffered log messages, dropping the rest",
			zap.Duration("timeout", l.closeTimeout),
		)
	}

	// Stop the request in progress. Then run drops the rest and exits.
	l.cancel()
	<-l.done

	return nil
}

// messageLen returns the length of s as Telegram may count it.
// Characters outside the Basic Multilingual Plane, such as most emoji,
// count as two. This is safe whether Telegram counts characters or
// UTF-16 code units.
func messageLen(s string) int {
	n := 0
	for _, r := range s {
		n += runeLen(r)
	}
	return n
}

func runeLen(r rune) int {
	if r > 0xFFFF {
		return 2
	}
	return 1
}

// truncate shortens text to at most maxLen as messageLen counts it,
// and marks that the text was truncated.
func truncate(text string, maxLen int) string {
	if messageLen(text) <= maxLen {
		return text
	}

	suffix := truncatedSuffix
	limit := maxLen - messageLen(suffix)
	if limit <= 0 {
		suffix = ""
		limit = maxLen
	}

	n := 0
	for i, r := range text {
		if n+runeLen(r) > limit {
			return text[:i] + suffix
		}
		n += runeLen(r)
	}

	return text
}

// messageFormatter is a helper struct that formats log messages.
type messageFormatter struct {
	template *fasttemplate.Template

	containerDetails *ContainerDetails
	attrs            map[string]string
}

// newMessageFormatter creates a new messageFormatter.
func newMessageFormatter(containerDetails *ContainerDetails, attrs map[string]string, template string) (*messageFormatter, error) {
	t, err := fasttemplate.NewTemplate(template, "{", "}")
	if err != nil {
		return nil, err
	}

	formatter := &messageFormatter{
		template:         t,
		containerDetails: containerDetails,
		attrs:            attrs,
	}

	if err := formatter.validateTemplate(); err != nil {
		return nil, err
	}

	return formatter, nil
}

// Format formats the given message.
func (f *messageFormatter) Format(msg *logger.Message) string {
	return f.template.ExecuteFuncString(f.tagFunc(msg))
}

// validateTemplate validates the template.
func (f *messageFormatter) validateTemplate() error {
	msg := &logger.Message{
		Line:      []byte("validate"),
		Timestamp: time.Now(),
	}
	_, err := f.template.ExecuteFuncStringWithErr(f.tagFunc(msg))
	return err
}

// tagFunc is a fasttemplate.TagFunc that replaces tags with values.
func (f *messageFormatter) tagFunc(msg *logger.Message) fasttemplate.TagFunc {
	return func(w io.Writer, tag string) (int, error) {
		switch tag {
		case "log":
			return w.Write(msg.Line)
		case "timestamp":
			return w.Write([]byte(msg.Timestamp.UTC().Format(time.RFC3339)))
		case "container_id":
			return w.Write([]byte(f.containerDetails.ID()))
		case "container_full_id":
			return w.Write([]byte(f.containerDetails.ContainerID))
		case "container_name":
			return w.Write([]byte(f.containerDetails.Name()))
		case "image_id":
			return w.Write([]byte(f.containerDetails.ImageID()))
		case "image_full_id":
			return w.Write([]byte(f.containerDetails.ContainerImageID))
		case "image_name":
			return w.Write([]byte(f.containerDetails.ImageName()))
		case "daemon_name":
			return w.Write([]byte(f.containerDetails.DaemonName))
		}

		if value, ok := f.attrs[tag]; ok {
			return w.Write([]byte(value))
		}

		return 0, fmt.Errorf("%w: %s", errUnknownTag, tag)
	}
}

type partialLogBuffer struct {
	logs map[string]*logger.Message
	mu   sync.Mutex
}

func newPartialLogBuffer() *partialLogBuffer {
	return &partialLogBuffer{
		logs: map[string]*logger.Message{},
	}
}

// Append adds a part of a long line. When it gets the last part, it returns
// the assembled line and true. The assembled line keeps at most maxLineBytes.
func (b *partialLogBuffer) Append(log *logger.Message) (*logger.Message, bool) {
	if log.PLogMetaData == nil {
		panic("log must be partial")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	plog, exists := b.logs[log.PLogMetaData.ID]
	if !exists {
		plog = new(logger.Message)
		*plog = *log

		b.logs[plog.PLogMetaData.ID] = plog

		plog.Line = make([]byte, 0, 16*1024) // 16KB. Arbitrary size
		plog.PLogMetaData = nil
	}

	if room := maxLineBytes - len(plog.Line); room > 0 {
		plog.Line = append(plog.Line, log.Line[:min(room, len(log.Line))]...)
	}

	if log.PLogMetaData.Last {
		delete(b.logs, log.PLogMetaData.ID)
		return plog, true
	}

	return nil, false
}
