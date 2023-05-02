package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func (m *fsMock) MkdirAll(path string, perm os.FileMode) error {
	if !strings.HasPrefix(path, m.basePath) {
		path = filepath.Join(m.basePath, path)
	}
	return os.MkdirAll(path, perm)
}

func (m *fsMock) RemoveAll(path string) error {
	if !strings.HasPrefix(path, m.basePath) {
		path = filepath.Join(m.basePath, path)
	}
	return os.RemoveAll(path)
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
	w, err := fifo.OpenFifo(context.Background(), fifoPath, unix.O_RDWR|unix.O_CREAT|unix.O_NONBLOCK, 0700)
	require.NoError(t, err)

	encoder := logdriver.NewLogEntryEncoder(w)
	for i := 0; i < count; i++ {
		entry := &logdriver.LogEntry{
			Source:   "stdout",
			TimeNano: time.Now().UnixNano(),
			Line:     []byte(strconv.Itoa(i)),
		}
		err = encoder.Encode(entry)
		require.NoError(t, err)

		msg := &logger.Message{
			Line:      entry.Line,
			Source:    entry.Source,
			Timestamp: time.Unix(0, entry.TimeNano),
		}
		loggerMock.On("Log", msg).Return(nil)
	}
}

func setupDriver(t *testing.T) *Driver {
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
	for i := 0; i < 10; i++ {
		select {
		case <-processedNotifier:
		case <-time.After(1 * time.Second):
			t.Fatal("timed out waiting for logs to be processed")
		}
	}

	loggerMock.AssertExpectations(t)
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

	loggerMock.On("Close").Return(nil)

	fifoPath = createTempFifo(t, driver.fs.BasePath())
	_, err = driver.StartLogging(fifoPath, containerDetails)
	assert.ErrorContains(t, err, "already logging for container")

	loggerMock.AssertExpectations(t)
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
		r, err := driver.ReadLogs(ctx, containerDetails.ContainerID, readConfig)
		assert.NoError(t, err)
		assert.NotNil(t, r)
	})

	t.Run("Not logging", func(t *testing.T) {
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

	select {
	case <-processedNotifier:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for logs to be processed")
	}

	err = driver.StopLogging(fifoPath)
	assert.NoError(t, err)

	err = driver.StopLogging(fifoPath)
	assert.ErrorContains(t, err, "not logging")
}

func createTempFifo(t *testing.T, tempDir string) string {
	t.Helper()

	nano := time.Now().UnixNano()
	fifoPath := filepath.Join(tempDir, strconv.FormatInt(nano, 10)+"fifo")
	if err := syscall.Mkfifo(fifoPath, 0666); err != nil {
		t.Fatalf("failed to create fifo file: %v", err)
	}

	return fifoPath
}
