package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/fifo"
	"github.com/docker/docker/api/types/plugins/logdriver"
	"github.com/docker/docker/daemon/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

type fsMock struct {
	basePath string
}

var _ fileSystem = (*fsMock)(nil)

func (m *fsMock) BasePath() string {
	return m.basePath
}

// checkPath returns an error if path is not inside the base path,
// so that tests find wrong paths instead of fixing them.
func (m *fsMock) checkPath(path string) error {
	if !strings.HasPrefix(path, m.basePath+string(filepath.Separator)) {
		return fmt.Errorf("path %q is not inside %q", path, m.basePath)
	}
	return nil
}

func (m *fsMock) MkdirAll(path string, perm os.FileMode) error {
	if err := m.checkPath(path); err != nil {
		return err
	}
	return os.MkdirAll(path, perm)
}

func (m *fsMock) RemoveAll(path string) error {
	if err := m.checkPath(path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// logFolder returns the folder of the json log file of the container.
func logFolder(driver *Driver, containerID string) string {
	return filepath.Join(driver.fs.BasePath(), "var", "log", "docker", containerID)
}

type telegramLoggerMock struct {
	mock.Mock
}

var _ telegramLogger = (*telegramLoggerMock)(nil)

func (t *telegramLoggerMock) Name() string {
	args := t.Called()
	return args.String(0)
}

func (t *telegramLoggerMock) Log(msg *logger.Message) error {
	args := t.Called(msg)
	return args.Error(0)
}

func (t *telegramLoggerMock) Close() error {
	args := t.Called()
	return args.Error(0)
}

func produceLogs(t *testing.T, loggerMock *telegramLoggerMock, count int, fifoPath string) {
	t.Helper()

	w, err := fifo.OpenFifo(context.Background(), fifoPath, unix.O_RDWR|unix.O_CREAT|unix.O_NONBLOCK, 0o700)
	require.NoError(t, err)

	encoder := logdriver.NewLogEntryEncoder(w)
	for i := range count {
		entry := &logdriver.LogEntry{
			Source:   "stdout",
			TimeNano: time.Now().UnixNano(),
			Line:     []byte(strconv.Itoa(i)),
		}

		// Expect the message before it is written: the driver can read it
		// from the fifo right away.
		msg := &logger.Message{
			Line:      entry.Line,
			Source:    entry.Source,
			Timestamp: time.Unix(0, entry.TimeNano),
		}
		loggerMock.On("Log", msg).Return(nil)

		err = encoder.Encode(entry)
		require.NoError(t, err)
	}
}

func setupDriver(t *testing.T) *Driver {
	t.Helper()

	driver := NewDriver(zap.NewNop())
	tempDir := t.TempDir()
	driver.fs = &fsMock{basePath: tempDir}
	return driver
}

func TestDriverStartLogging(t *testing.T) {
	t.Parallel()

	loggerMock := &telegramLoggerMock{}

	driver := setupDriver(t)
	driver.newTelegramLogger = func(*zap.Logger, *ContainerDetails) (telegramLogger, error) {
		return loggerMock, nil
	}
	processedNotifier := make(chan struct{})
	driver.processLogs = func(stream *logStream) {
		driver.defaultProcessLogs(stream, processedNotifier)
	}

	fifoPath := createTempFifo(t, driver.fs.BasePath())
	containerDetails := &ContainerDetails{
		ContainerID: "testcontainer",
		Config:      map[string]string{},
	}

	stream, err := driver.StartLogging(fifoPath, containerDetails)
	assert.NoError(t, err)

	assert.NotNil(t, stream.jsonLogger)
	assert.False(t, stream.keepFile)

	produceLogs(t, loggerMock, 10, fifoPath)
	for range 10 {
		select {
		case <-processedNotifier:
		case <-time.After(1 * time.Second):
			t.Fatal("timed out waiting for logs to be processed")
		}
	}

	loggerMock.AssertExpectations(t)
}

func TestDriverStartLogging_FailedToInitTelegramLogger(t *testing.T) {
	t.Parallel()

	driver := setupDriver(t)
	driver.newTelegramLogger = func(logger *zap.Logger, details *ContainerDetails) (telegramLogger, error) {
		l, err := NewTelegramLogger(logger, details)
		if err != nil {
			return nil, assert.AnError
		}

		return l, nil
	}

	fifoPath := createTempFifo(t, driver.fs.BasePath())
	containerDetails := &ContainerDetails{
		ContainerID: "testcontainer",
		Config:      map[string]string{},
	}

	_, err := driver.StartLogging(fifoPath, containerDetails)
	assert.ErrorIs(t, err, assert.AnError)
}

func TestDriverStartLogging_AlreadyLogging(t *testing.T) {
	t.Parallel()

	driver := setupDriver(t)
	driver.processLogs = func(_ *logStream) {}
	driver.newTelegramLogger = func(*zap.Logger, *ContainerDetails) (telegramLogger, error) {
		return &telegramLoggerMock{}, nil
	}

	fifoPath := createTempFifo(t, driver.fs.BasePath())
	containerDetails := &ContainerDetails{
		ContainerID: "testcontainer",
		Config:      map[string]string{},
	}

	_, err := driver.StartLogging(fifoPath, containerDetails)
	assert.NoError(t, err)

	_, err = driver.StartLogging(fifoPath, containerDetails)
	assert.ErrorContains(t, err, "already logging")
}

func TestDriverStartLogging_BadOptions(t *testing.T) {
	t.Parallel()

	driver := setupDriver(t)
	driver.processLogs = func(_ *logStream) {}
	driver.newTelegramLogger = func(*zap.Logger, *ContainerDetails) (telegramLogger, error) {
		return &telegramLoggerMock{}, nil
	}

	fifoPath := createTempFifo(t, driver.fs.BasePath())
	containerDetails := &ContainerDetails{ContainerID: "testcontainer"}

	containerDetails.Config = map[string]string{cfgNoFileKey: "true1"}
	_, err := driver.StartLogging(fifoPath, containerDetails)
	assert.ErrorContains(t, err, "invalid value")

	containerDetails.Config = map[string]string{cfgKeepFileKey: "true1"}
	_, err = driver.StartLogging(fifoPath, containerDetails)
	assert.ErrorContains(t, err, "invalid value")
}

func TestDriverStartLogging_NoFile(t *testing.T) {
	t.Parallel()

	driver := setupDriver(t)
	driver.processLogs = func(_ *logStream) {}
	driver.newTelegramLogger = func(*zap.Logger, *ContainerDetails) (telegramLogger, error) {
		return &telegramLoggerMock{}, nil
	}

	fifoPath := createTempFifo(t, driver.fs.BasePath())
	containerDetails := &ContainerDetails{
		ContainerID: "testcontainer",
		Config:      map[string]string{cfgNoFileKey: "true"},
	}

	stream, err := driver.StartLogging(fifoPath, containerDetails)
	assert.NoError(t, err)

	assert.Nil(t, stream.jsonLogger)
}

func TestDriverStartLogging_AlreadyLoggingForContainer(t *testing.T) {
	t.Parallel()

	loggerMock := &telegramLoggerMock{}

	driver := setupDriver(t)
	driver.processLogs = func(_ *logStream) {}
	driver.newTelegramLogger = func(*zap.Logger, *ContainerDetails) (telegramLogger, error) {
		return loggerMock, nil
	}

	containerDetails := &ContainerDetails{
		ContainerID: "testcontainer",
		Config:      map[string]string{},
	}

	fifoPath := createTempFifo(t, driver.fs.BasePath())
	_, err := driver.StartLogging(fifoPath, containerDetails)
	assert.NoError(t, err)

	fifoPath = createTempFifo(t, driver.fs.BasePath())
	_, err = driver.StartLogging(fifoPath, containerDetails)
	assert.ErrorContains(t, err, "already logging for container")

	// The failed start must not touch the log folder of the working stream.
	assert.DirExists(t, logFolder(driver, containerDetails.ContainerID))
}

func TestDriverReadLogs(t *testing.T) {
	t.Parallel()

	loggerMock := &telegramLoggerMock{}

	driver := setupDriver(t)
	driver.newTelegramLogger = func(*zap.Logger, *ContainerDetails) (telegramLogger, error) {
		return loggerMock, nil
	}

	fifoPath := createTempFifo(t, driver.fs.BasePath())
	containerDetails := &ContainerDetails{
		ContainerID: "testcontainer",
		Config:      map[string]string{},
	}

	_, err := driver.StartLogging(fifoPath, containerDetails)
	assert.NoError(t, err)

	readConfig := &ReadConfig{
		Since:  time.Now(),
		Until:  time.Now(),
		Tail:   10,
		Follow: false,
	}

	ctx := context.Background()

	t.Run("Success", func(t *testing.T) {
		t.Parallel()

		r, err := driver.ReadLogs(ctx, containerDetails.ContainerID, readConfig)
		assert.NoError(t, err)
		assert.NotNil(t, r)
	})

	t.Run("Not logging", func(t *testing.T) {
		t.Parallel()

		_, err := driver.ReadLogs(ctx, "nonexistent-container", readConfig)
		assert.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "not logging"))
	})
}

func TestDriverStopLogging(t *testing.T) {
	t.Parallel()

	loggerMock := &telegramLoggerMock{}

	driver := setupDriver(t)
	driver.newTelegramLogger = func(*zap.Logger, *ContainerDetails) (telegramLogger, error) {
		return loggerMock, nil
	}

	fifoPath := createTempFifo(t, driver.fs.BasePath())
	containerDetails := &ContainerDetails{
		ContainerID: "testcontainer",
		Config:      map[string]string{},
	}

	processedNotifier := make(chan struct{})
	driver.processLogs = func(stream *logStream) {
		driver.defaultProcessLogs(stream, processedNotifier)
	}

	_, err := driver.StartLogging(fifoPath, containerDetails)
	assert.NoError(t, err)

	produceLogs(t, loggerMock, 2, fifoPath)
	loggerMock.On("Close").Return(nil)

	for range 2 {
		select {
		case <-processedNotifier:
		case <-time.After(1 * time.Second):
			t.Fatal("timed out waiting for logs to be processed")
		}
	}

	assert.DirExists(t, logFolder(driver, containerDetails.ContainerID))

	err = driver.StopLogging(fifoPath)
	assert.NoError(t, err)
	assert.NoDirExists(t, logFolder(driver, containerDetails.ContainerID))

	// The second stop does nothing.
	err = driver.StopLogging(fifoPath)
	assert.NoError(t, err)
}

// countingLogger counts the messages. It is slow on purpose,
// so that the messages wait in the fifo.
type countingLogger struct {
	count atomic.Int64
	delay time.Duration
}

func (l *countingLogger) Name() string { return "counting" }
func (l *countingLogger) Close() error { return nil }

func (l *countingLogger) Log(*logger.Message) error {
	time.Sleep(l.delay)
	l.count.Add(1)
	return nil
}

func TestDriverStopLogging_ReadsTheRestOfFifo(t *testing.T) {
	t.Parallel()

	telegram := &countingLogger{delay: 50 * time.Microsecond}

	driver := setupDriver(t)
	driver.newTelegramLogger = func(*zap.Logger, *ContainerDetails) (telegramLogger, error) {
		return telegram, nil
	}

	fifoPath := createTempFifo(t, driver.fs.BasePath())
	_, err := driver.StartLogging(fifoPath, &ContainerDetails{
		ContainerID: "testcontainer",
		Config:      map[string]string{cfgNoFileKey: "true"},
	})
	require.NoError(t, err)

	w, err := fifo.OpenFifo(context.Background(), fifoPath, unix.O_WRONLY|unix.O_NONBLOCK, 0)
	require.NoError(t, err)
	defer func() {
		_ = w.Close()
	}()

	// Docker writes the last logs and calls StopLogging right after that.
	const count = 300
	encoder := logdriver.NewLogEntryEncoder(w)
	for i := range count {
		require.NoError(t, encoder.Encode(&logdriver.LogEntry{
			Source:   "stdout",
			TimeNano: time.Now().UnixNano(),
			Line:     []byte(strconv.Itoa(i)),
		}))
	}

	require.NoError(t, driver.StopLogging(fifoPath))
	assert.Equal(t, int64(count), telegram.count.Load())
}

func TestDriverStopLogging_KeepFile(t *testing.T) {
	t.Parallel()

	loggerMock := &telegramLoggerMock{}
	loggerMock.On("Close").Return(nil)

	driver := setupDriver(t)
	driver.processLogs = func(_ *logStream) {}
	driver.newTelegramLogger = func(*zap.Logger, *ContainerDetails) (telegramLogger, error) {
		return loggerMock, nil
	}

	fifoPath := createTempFifo(t, driver.fs.BasePath())
	containerDetails := &ContainerDetails{
		ContainerID: "testcontainer",
		Config:      map[string]string{cfgKeepFileKey: "true"},
	}

	_, err := driver.StartLogging(fifoPath, containerDetails)
	require.NoError(t, err)

	require.NoError(t, driver.StopLogging(fifoPath))
	assert.FileExists(t, filepath.Join(logFolder(driver, containerDetails.ContainerID), "json.log"))
	loggerMock.AssertExpectations(t)
}

func TestDriverClose(t *testing.T) {
	t.Parallel()

	loggerMock := &telegramLoggerMock{}
	loggerMock.On("Close").Return(nil)

	driver := setupDriver(t)
	driver.processLogs = func(_ *logStream) {}
	driver.newTelegramLogger = func(*zap.Logger, *ContainerDetails) (telegramLogger, error) {
		return loggerMock, nil
	}

	for _, id := range []string{"container1", "container2"} {
		_, err := driver.StartLogging(createTempFifo(t, driver.fs.BasePath()), &ContainerDetails{
			ContainerID: id,
			Config:      map[string]string{},
		})
		require.NoError(t, err)
	}

	driver.Close()

	loggerMock.AssertNumberOfCalls(t, "Close", 2)
	assert.NoDirExists(t, logFolder(driver, "container1"))
	assert.NoDirExists(t, logFolder(driver, "container2"))
	assert.Empty(t, driver.streams)
	assert.Empty(t, driver.containerStreams)
}

func createTempFifo(t *testing.T, tempDir string) string {
	t.Helper()

	nano := time.Now().UnixNano()
	fifoPath := filepath.Join(tempDir, strconv.FormatInt(nano, 10)+"fifo")
	if err := syscall.Mkfifo(fifoPath, 0o666); err != nil {
		t.Fatalf("failed to create fifo file: %v", err)
	}

	return fifoPath
}
