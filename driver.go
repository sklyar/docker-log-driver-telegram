package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/containerd/fifo"
	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/plugins/logdriver"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

type telegramLogger interface {
	Name() string
	Log(msg *logger.Message) error
	Close() error
}

type newTelegramLoggerFunc func(*zap.Logger, *ContainerDetails) (telegramLogger, error)

type Driver struct {
	streams          map[string]*logStream
	containerStreams map[string]*logStream
	mu               sync.RWMutex

	fs                fileSystem
	newTelegramLogger newTelegramLoggerFunc
	processLogs       func(stream *logStream)

	zapLogger *zap.Logger
}

func NewDriver(zapLogger *zap.Logger) *Driver {
	driver := &Driver{
		streams:          make(map[string]*logStream),
		containerStreams: make(map[string]*logStream),
		fs:               osFS{},
		newTelegramLogger: func(logger *zap.Logger, details *ContainerDetails) (telegramLogger, error) {
			l, err := NewTelegramLogger(logger, details)
			if err != nil {
				return nil, err
			}
			return l, nil
		},
		zapLogger: zapLogger,
	}

	driver.processLogs = func(stream *logStream) {
		driver.defaultProcessLogs(stream, nil)
	}

	return driver
}

func (d *Driver) StartLogging(streamPath string, containerDetails *ContainerDetails) (stream *logStream, err error) {
	d.mu.RLock()
	if _, ok := d.streams[streamPath]; ok {
		d.mu.RUnlock()
		return nil, errors.New("already logging")
	}
	d.mu.RUnlock()

	name := "container:" + containerDetails.ContainerName
	stream = &logStream{
		streamPath:       streamPath,
		containerDetails: containerDetails,
		logger:           d.zapLogger.Named(name),
		fs:               d.fs,
		stop:             make(chan struct{}),
	}

	defer func(stream *logStream) {
		if err == nil || stream == nil {
			return
		}

		if err := stream.Close(); err != nil {
			d.zapLogger.Error("failed to close stream", zap.Error(err))
		}

		stream = nil
	}(stream)

	noFile, err := parseBool(containerDetails.Config[cfgNoFileKey], false)
	if err != nil {
		return nil, fmt.Errorf("invalid value for %q option: %w", cfgNoFileKey, err)
	}
	stream.keepFile, err = parseBool(containerDetails.Config[cfgKeepFileKey], false)
	if err != nil {
		return nil, fmt.Errorf("invalid value for %q option: %w", cfgKeepFileKey, err)
	}

	if !noFile {
		folder := filepath.Join(d.fs.BasePath(), "/var/log/docker", containerDetails.ContainerID)
		if err := d.fs.MkdirAll(folder, 0755); err != nil {
			return nil, fmt.Errorf("failed to create folder: %w", err)
		}

		stream.jsonLogger, err = newJSONLogger(containerDetails, folder)
		if err != nil {
			return nil, fmt.Errorf("failed to create json logger: %w", err)
		}
	}

	stream.telegramLogger, err = d.newTelegramLogger(d.zapLogger, containerDetails)
	if err != nil {
		return nil, fmt.Errorf("failed to create telegram logger: %w", err)
	}

	stream.logFifo, err = fifo.OpenFifo(context.Background(), streamPath, unix.O_RDWR|unix.O_CREAT|unix.O_NONBLOCK, 0700)
	if err != nil {
		return nil, fmt.Errorf("failed to open fifo: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.streams[streamPath]; exists {
		return nil, errors.New("already logging")
	}
	if _, exists := d.containerStreams[containerDetails.ContainerID]; exists {
		return nil, errors.New("already logging for container")
	}

	d.streams[streamPath] = stream
	d.containerStreams[containerDetails.ContainerID] = stream

	go d.processLogs(stream)

	return stream, nil
}

// defaultProcessLogs reads logs from fifo file and forwards them to the loggers.
// The optional 'processedNotifier' channel is used exclusively for testing purposes to signal
// when a log entry has been processed. It can be left as 'nil' for non-testing use cases.
func (d *Driver) defaultProcessLogs(stream *logStream, processedNotifier chan<- struct{}) {
	defer func() {
		if err := stream.Close(); err != nil {
			d.zapLogger.Error("failed to close stream", zap.Error(err))
		}
	}()

	logs := NewLogs(stream)
	for logs.Next() {
		select {
		case <-stream.stop:
			return
		default:
		}

		entry := logs.Log()

		var partialLog *backend.PartialLogMetaData
		if m := entry.GetPartialLogMetadata(); m != nil {
			partialLog = &backend.PartialLogMetaData{
				Last:    m.GetLast(),
				ID:      m.GetId(),
				Ordinal: int(m.GetOrdinal()),
			}
		}

		log := &logger.Message{
			Line:         entry.GetLine(),
			Source:       entry.GetSource(),
			Timestamp:    time.Unix(0, entry.GetTimeNano()),
			PLogMetaData: partialLog,
		}

		stream.logger.Debug(
			"message",
			zap.String("line", string(log.Line)),
			zap.String("source", log.Source),
			zap.Time("timestamp", log.Timestamp),
		)

		if err := stream.telegramLogger.Log(log); err != nil {
			stream.logger.Error("failed to log to telegram logger", zap.Error(err))
		}

		if stream.jsonLogger != nil {
			if err := stream.jsonLogger.Log(log); err != nil {
				stream.logger.Error("failed to log to json logger", zap.Error(err))
			}
		}

		// Notify that we have processed the log.
		// It`s needs for tests.
		if processedNotifier != nil {
			processedNotifier <- struct{}{}
		}
	}

	if err := logs.Err(); err != nil {
		stream.logger.Error("failed to read logs", zap.Error(err))
	}
}

func (d *Driver) ReadLogs(ctx context.Context, containerID string, readConfig *ReadConfig) (io.ReadCloser, error) {
	d.mu.RLock()
	stream, exists := d.containerStreams[containerID]
	d.mu.RUnlock()
	if !exists {
		return nil, errors.New("not logging")
	}

	if stream.jsonLogger == nil {
		return nil, fmt.Errorf("%q option is set to true, disabling reading capability", cfgNoFileKey)
	}

	jsonLogReader, ok := stream.jsonLogger.(logger.LogReader)
	if !ok {
		return nil, errors.New("logger does not support reading logs")
	}

	r, w := io.Pipe()

	logReader := &logReader{
		stream:    stream,
		config:    readConfig,
		r:         jsonLogReader,
		w:         w,
		zapLogger: d.zapLogger,
	}
	go logReader.ReadLogs(ctx)

	return r, nil
}

func (d *Driver) StopLogging(streamPath string) error {
	d.mu.Lock()

	stream, exists := d.streams[streamPath]
	if !exists {
		d.mu.Unlock()
		return errors.New("not logging")
	}

	delete(d.streams, streamPath)
	delete(d.containerStreams, stream.containerDetails.ContainerID)

	d.mu.Unlock()

	if err := stream.Close(); err != nil {
		return fmt.Errorf("failed to stop stream: %w", err)
	}

	return nil
}

// logReader is a wrapper around json-file logger that reads logs
// from it and writes them to the pipe.
type logReader struct {
	stream *logStream

	config *ReadConfig

	r logger.LogReader
	w *io.PipeWriter

	zapLogger *zap.Logger
}

// ReadLogs reads logs from json-file logger and writes them to the pipe.
func (r *logReader) ReadLogs(ctx context.Context) {
	defer r.w.Close()

	logWatcher := r.r.ReadLogs(*r.config)
	defer logWatcher.ConsumerGone()

	encoder := logdriver.NewLogEntryEncoder(r.w)

	var buf logdriver.LogEntry
	for {
		select {
		case msg, ok := <-logWatcher.Msg:
			if !ok {
				return
			}

			buf.Line = msg.Line
			buf.Partial = msg.PLogMetaData != nil && msg.PLogMetaData.Last
			buf.Source = msg.Source
			buf.TimeNano = msg.Timestamp.UnixNano()

			if err := encoder.Encode(&buf); err != nil {
				r.zapLogger.Error("failed to encode log entry", zap.Error(err))
				return
			}
		case err := <-logWatcher.Err:
			_ = r.w.CloseWithError(err)
			r.zapLogger.Error("log watcher error", zap.Error(err))
			return
		case <-r.stream.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

// logStream is a stream that forwards logs to the loggers.
type logStream struct {
	streamPath       string
	containerDetails *ContainerDetails

	logFifo io.ReadCloser

	telegramLogger telegramLogger
	jsonLogger     logger.Logger

	keepFile bool

	logger *zap.Logger
	fs     fileSystem

	stop       chan struct{}
	closedOnce sync.Once
}

// Close closes the stream.
func (s *logStream) Close() error {
	var err error

	s.closedOnce.Do(func() {
		close(s.stop)

		var errs []error

		if s.logFifo != nil {
			if err := s.logFifo.Close(); err != nil {
				errs = append(errs, fmt.Errorf("failed to close stream: %w", err))
			}
		}

		if s.jsonLogger != nil && !s.keepFile {
			if err := s.jsonLogger.Close(); err != nil {
				errs = append(errs, fmt.Errorf("failed to close json logger: %w", err))
			}
			folder := filepath.Dir(s.containerDetails.LogPath)
			if err := s.fs.RemoveAll(folder); err != nil {
				errs = append(errs, fmt.Errorf("failed to remove log dir: %w", err))
			}
		}

		if s.telegramLogger != nil {
			if err := s.telegramLogger.Close(); err != nil {
				errs = append(errs, fmt.Errorf("failed to close telegram logger: %w", err))
			}
		}

		err = errors.Join(errs...)
	})

	return err
}

// Logs represents an iterator for reading logs using logdriver.LogEntryDecoder.
type Logs struct {
	decoder logdriver.LogEntryDecoder
	stream  *logStream

	current logdriver.LogEntry

	closed  bool
	lastErr error
}

// NewLogs creates a new Logs iterator instance for reading logs from the provided log stream.
func NewLogs(stream *logStream) *Logs {
	return &Logs{
		decoder: logdriver.NewLogEntryDecoder(stream.logFifo),
		stream:  stream,
	}
}

// Next attempts to read the next log entry. It returns true if successful,
// or false if the end of the log is reached or an error occurs.
func (l *Logs) Next() bool {
	if l.closed {
		return false
	}

	var entry logdriver.LogEntry
	if err := l.decoder.Decode(&entry); err != nil {
		l.closed = true
		if !isStreamClosed(err) {
			l.lastErr = fmt.Errorf("failed to decode log entry: %w", err)
		}
		return false
	}

	l.current = entry

	return true
}

// Log returns the current log entry read by the iterator.
func (l *Logs) Log() logdriver.LogEntry {
	return l.current
}

// Err returns the last error encountered by the iterator.
func (l *Logs) Err() error {
	return l.lastErr
}

// Close closes the Logs iterator and the underlying log stream.
func (l *Logs) Close() error {
	l.closed = true
	return l.stream.Close()
}

type osFS struct{}

var _ fileSystem = (*osFS)(nil)

func (osFS) BasePath() string {
	return ""
}

func (osFS) MkdirAll(path string, perm os.FileMode) error {
	path = filepath.Clean(path)
	return os.MkdirAll(path, perm)
}

func (osFS) RemoveAll(path string) error {
	path = filepath.Clean(path)
	return os.RemoveAll(path)
}

func newJSONLogger(containerDetails *ContainerDetails, folder string) (logger.Logger, error) {
	details := *containerDetails
	details.LogPath = filepath.Join(folder, "json.log")
	return jsonfilelog.New(details)
}

func isStreamClosed(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, os.ErrClosed) ||
		strings.Contains(err.Error(), "file already closed")
}

type fileSystem interface {
	BasePath() string
	MkdirAll(path string, perm os.FileMode) error
	RemoveAll(path string) error
}
