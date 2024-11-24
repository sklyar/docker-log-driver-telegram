package main

import (
	"bytes"
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

	formatter *messageFormatter
	cfg       *loggerConfig

	buffer chan string
	mu     sync.Mutex

	partialLogsBuffer *partialLogBuffer

	wg     sync.WaitGroup
	closed chan struct{}
	logger *zap.Logger
}

var _ = (logger.Logger)(&TelegramLogger{})

// NewTelegramLogger creates a new TelegramLogger.
func NewTelegramLogger(
	logger *zap.Logger,
	containerDetails *ContainerDetails,
) (*TelegramLogger, error) {
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

	bufferCapacity := 10_000
	if cfg.MaxBufferSize <= 0 {
		bufferCapacity = 0
	}
	buffer := make(chan string, bufferCapacity)

	l := &TelegramLogger{
		client:            client,
		formatter:         formatter,
		cfg:               cfg,
		buffer:            buffer,
		partialLogsBuffer: newPartialLogBuffer(),
		closed:            make(chan struct{}),
		logger:            logger,
	}

	l.wg.Add(1)
	runner := l.runImmediate
	if cfg.BatchEnabled {
		runner = l.runBatching
	}
	go runner()

	return l, nil
}

// Name implements the logger.Logger interface.
func (l *TelegramLogger) Name() string {
	return driverName
}

// Log implements the logger.Logger interface.
func (l *TelegramLogger) Log(log *logger.Message) error {
	if l.isClosed() {
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
	// Split the message if it exceeds the maximum number of characters.
	if utf8.RuneCountInString(text) > maxLogMessageChars {
		runes := []rune(text)
		for len(runes) > 0 {
			end := maxLogMessageChars
			if len(runes) < end {
				end = len(runes)
			}
			slog := string(runes[:end])
			runes = runes[end:]
			if err := l.enqueue(slog); err != nil {
				return err
			}
		}
		return nil
	}

	if err := l.enqueue(text); err != nil {
		return err
	}

	return nil
}

func (l *TelegramLogger) enqueue(log string) error {
	if l.cfg.MaxBufferSize <= 0 {
		l.buffer <- log // May block.
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	select {
	case l.buffer <- log:
		return nil
	case <-l.closed:
		return errLoggerClosed
	default:
		// Buffer is full.
		select {
		case <-l.buffer:
			// Drop the oldest message.
		default:
			// Buffer was empty.
		}

		// Try to enqueue the new message again.
		select {
		case l.buffer <- log:
			return nil
		case <-l.closed:
			return errLoggerClosed
		default:
			return errors.New("failed to enqueue message after dropping oldest")
		}
	}
}

func (l *TelegramLogger) runImmediate() {
	defer l.wg.Done()

	drain := func() {
		for log := range l.buffer {
			l.send(log)
		}
	}
	defer drain()

	for {
		select {
		case log, ok := <-l.buffer:
			if !ok {
				return
			}
			l.send(log)
		case <-l.closed:
			return
		}
	}
}

func (l *TelegramLogger) runBatching() {
	defer l.wg.Done()

	ticker := time.NewTicker(l.cfg.BatchFlushInterval)
	defer ticker.Stop()

	var (
		batch          bytes.Buffer
		batchRuneCount int
	)

	const maxBytes = 4 * maxLogMessageChars // Unicode characters are up to 4 bytes
	batch.Grow(maxBytes)

	flush := func() {
		if batch.Len() == 0 {
			return
		}

		if batch.Bytes()[batch.Len()-1] == '\n' {
			batch.Truncate(batch.Len() - 1)
			batchRuneCount--
		}

		if err := l.client.SendMessage(batch.String()); err != nil {
			l.logger.Error("failed to send log message", zap.Error(err))
		}

		batch.Reset()
		batchRuneCount = 0
	}

	add := func(log string) {
		logLength := utf8.RuneCountInString(log) + 1

		batch.WriteString(log)
		batch.WriteByte('\n')
		batchRuneCount += logLength

		if batchRuneCount >= maxLogMessageChars {
			flush()
		}
	}

	drain := func() {
		for log := range l.buffer {
			add(log)
		}
	}
	defer drain()
	defer flush()

	for {
		select {
		case log, ok := <-l.buffer:
			if !ok {
				return
			}
			add(log)
		case <-ticker.C:
			flush()
		case <-l.closed:
			return
		}
	}
}

func (l *TelegramLogger) send(log string) {
	if err := l.client.SendMessage(log); err != nil {
		l.logger.Error("failed to send log message", zap.Error(err))
	}
}

// Close implements the logger.Logger interface.
func (l *TelegramLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.isClosed() {
		return nil
	}
	close(l.closed)
	close(l.buffer)

	l.wg.Wait()

	return nil
}

func (l *TelegramLogger) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
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
