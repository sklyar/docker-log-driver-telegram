package main

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/daemon/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"strings"
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

func (c *mockClient) SendMessage(message string) error {
	args := c.Called(message)
	return args.Error(0)
}

func TestTelegramLogger_Log_NoBuffer(t *testing.T) {
	t.Parallel()

	zapLogger := zap.NewNop()
	containerDetails := *defaultContainerDetails
	containerDetails.Config = map[string]string{
		cfgTokenKey:         "token",
		cfgChatIDKey:        "chat_id",
		cfgBatchEnabledKey:  "false",
		cfgMaxBufferSizeKey: "0",
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
}

func TestTelegramLoggerLog_Truncate(t *testing.T) {
	t.Parallel()

	formatter, err := newMessageFormatter(defaultContainerDetails, nil, "{log}")
	assert.NoError(t, err)

	longMessage := strings.Repeat("a", maxLogMessageChars+1)

	client := &mockClient{}
	client.On("SendMessage", longMessage[:maxLogMessageChars]).Return(nil)

	telegramLogger := &TelegramLogger{
		client:    client,
		logger:    zap.NewNop(),
		formatter: formatter,
		cfg:       &loggerConfig{},
	}

	err = telegramLogger.Log(&logger.Message{Line: []byte(longMessage)})
	assert.NoError(t, err)
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

	formatter, err := newMessageFormatter(defaultContainerDetails, nil, "{log}")
	assert.NoError(t, err)

	client := &mockClient{}
	client.On("SendMessage", string(assembledLog.Line)).Return(nil)

	telegramLogger := &TelegramLogger{
		client:            client,
		partialLogsBuffer: newPartialLogBuffer(),
		formatter:         formatter,
		cfg:               &loggerConfig{},
		logger:            zap.NewNop(),
	}

	assert.NoError(t, telegramLogger.Log(log1))
	assert.NoError(t, telegramLogger.Log(log2))
	assert.NoError(t, telegramLogger.Log(log3))
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
		tc := tc

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
