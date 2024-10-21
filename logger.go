package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/docker/docker/daemon/logger"
	"github.com/valyala/fasttemplate"
	"go.uber.org/zap"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// driverName is the name of the driver.
	driverName = "telegram"

	// maxLogMessageChars defines the maximum number of characters allowed in a Telegram message (4096 Unicode characters).
	maxLogMessageChars = 4096

	// maxLogMessageBytes defines the maximum number of bytes that can be occupied by a Telegram message,
	// assuming that the most complex characters (such as emoji) may use up to 4 bytes per character.
	maxLogMessageBytes = 4 * maxLogMessageChars // 16,384 bytes
)

var (
	errUnknownTag   = errors.New("unknown tag")
	errLoggerClosed = errors.New("logger is closed")
)

// client is an interface that represents a Telegram client.
type client interface {
	SendMessage(message string) error
}

// TelegramLogger is a logger that sends logs to Telegram.
// It implements the logger.Logger interface.
type TelegramLogger struct {
	client client

	partialLogsBuffer *partialLogBuffer
	formatter         *messageFormatter
	cfg               *loggerConfig

	closed bool
	mu     sync.RWMutex

	logger *zap.Logger
}

var _ = (logger.Logger)(&TelegramLogger{})

// NewTelegramLogger creates a new TelegramLogger.
func NewTelegramLogger(ctx context.Context, logger *zap.Logger, containerDetails *ContainerDetails) (*TelegramLogger, error) {
	cfg, err := parseLoggerConfig(containerDetails)
	if err != nil {
		return nil, fmt.Errorf("failed to parse logger config: %w", err)
	}

	logger.Debug("parsed logger config", zap.Any("config", cfg))
	logger.Debug("parsed container details", zap.Any("details", containerDetails))

	formatter, err := newMessageFormatter(containerDetails, cfg.Attrs, cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("failed to create message formatter: %w", err)
	}

	client, err := NewClient(logger, cfg.ClientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram Client: %w", err)
	}
	if err := client.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping Telegram: %w", err)
	}

	return &TelegramLogger{
		client:            client,
		partialLogsBuffer: newPartialLogBuffer(),
		formatter:         formatter,
		cfg:               cfg,
		closed:            false,
		mu:                sync.RWMutex{},
		logger:            logger,
	}, nil
}

// Name implements the logger.Logger interface.
func (l *TelegramLogger) Name() string {
	return driverName
}

// Log implements the logger.Logger interface.
func (l *TelegramLogger) Log(log *logger.Message) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return errLoggerClosed
	}

	if log.PLogMetaData != nil {
		assembledLog, last := l.partialLogsBuffer.Append(log)
		if !last {
			return nil
		}

		*log = *assembledLog
	}

	if l.cfg.FilterRegex != nil && !l.cfg.FilterRegex.Match(log.Line) {
		l.logger.Debug("message is filtered out by regex", zap.String("regex", l.cfg.FilterRegex.String()))
		return nil
	}

	text := l.formatter.Format(log)
	// Truncate the message if it exceeds the maximum number of characters allowed in a Telegram message.
	// It's temprorary solution, we should implement option to split message into multiple messages.
	if utf8.RuneCountInString(text) > maxLogMessageChars {
		text = string([]rune(text)[:maxLogMessageChars])
	}

	return l.client.SendMessage(text)
}

// Close implements the logger.Logger interface.
func (l *TelegramLogger) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()

	return nil
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

type logBuffer struct {
	logger *zap.Logger
	client client

	flushInterval time.Duration
	batchSize     int

	logs    []string
	mu      sync.Mutex
	flushCh chan struct{}
}

func newLogBuffer(logger *zap.Logger, client client, flushInterval time.Duration, batchSize int) *logBuffer {
	buf := &logBuffer{
		logger:        logger,
		client:        client,
		flushInterval: flushInterval,
		batchSize:     batchSize,
		logs:          make([]string, 0, batchSize),
		flushCh:       make(chan struct{}),
	}

	return buf
}

func (b *logBuffer) Append(log string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.logs = append(b.logs, log)
	if len(b.logs) >= b.batchSize {
		select {
		case b.flushCh <- struct{}{}:
		default: // Do nothing if the channel is full.
		}
	}
}

func (b *logBuffer) StartFlushTimer(ctx context.Context) {
	ticker := time.NewTicker(b.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log, ok := b.flush()
			if !ok {
				continue
			}
			if err := b.client.SendMessage(log); err != nil {
				b.logger.With(zap.String("log", log)).Error("failed to send log message", zap.Error(err))
				continue
			}
		case <-b.flushCh:
			log, ok := b.flush()
			if !ok {
				continue
			}
			if err := b.client.SendMessage(log); err != nil {
				b.logger.With(zap.String("log", log)).Error("failed to send log message", zap.Error(err))
				continue
			}
		case <-ctx.Done():
			return
		}
	}
}

func (b *logBuffer) flush() (string, bool) {
	if len(b.logs) == 0 {
		return "", false
	}
	if len(b.logs) == 1 {
		return b.logs[0], true
	}

	buf := bytes.NewBufferString(b.logs[0])
	buf.Grow(maxLogMessageBytes)

	var lastIdx int
	for i, log := range b.logs[1:] {
		if buf.Len()+len(log)+1 > maxLogMessageBytes { // +1 for newline
			break
		}

		buf.WriteRune('\n')
		buf.WriteString(log)

		lastIdx = i
	}

	b.logs = b.logs[lastIdx+1:]

	return buf.String(), true
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

	plog.Line = append(plog.Line, log.Line...)

	if log.PLogMetaData.Last {
		delete(b.logs, log.PLogMetaData.ID)
		return plog, true
	}

	return nil, false
}
