package main

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

const (
	cfgURLKey     = "url"
	cfgTokenKey   = "token"
	cfgChatIDKey  = "chat_id"
	cfgRetriesKey = "retries"
	cfgTimeoutKey = "timeout"

	cfgNoFileKey   = "no-file"
	cfgKeepFileKey = "keep-file"

	cfgTemplateKey    = "template"
	cfgFilterRegexKey = "filter-regex"

	cfgBatchingEnabledKey     = "batching-enabled"
	cfgBatchingMaxWaitTimeKey = "batching-max-wait-time"
)

type loggerConfig struct {
	ClientConfig ClientConfig

	Attrs map[string]string

	Template    string
	FilterRegex *regexp.Regexp

	BatchEnabled bool
	BatchMaxWait time.Duration
}

var defaultLoggerConfig = loggerConfig{
	Template:     "{log}",
	BatchEnabled: true,
	BatchMaxWait: 3 * time.Second,
}

var defaultClientConfig = ClientConfig{
	APIURL:  "https://api.telegram.org",
	Retries: 5,
	Timeout: 10 * time.Second,
}

func parseLoggerConfig(containerDetails *ContainerDetails) (*loggerConfig, error) {
	clientConfig, err := parseClientConfig(containerDetails)
	if err != nil {
		return nil, fmt.Errorf("failed to parse client config: %w", err)
	}
	attrs, err := containerDetails.ExtraAttributes(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to parse extra attributes: %w", err)
	}

	cfg := defaultLoggerConfig
	cfg.ClientConfig = clientConfig
	cfg.Attrs = attrs

	if template, ok := containerDetails.Config[cfgTemplateKey]; ok {
		cfg.Template = template
	}

	if filterRegex, ok := containerDetails.Config[cfgFilterRegexKey]; ok {
		cfg.FilterRegex, err = regexp.Compile(filterRegex)
		if err != nil {
			return nil, fmt.Errorf("failed to parse %q option: %w", cfgFilterRegexKey, err)
		}
	}

	if batchingEnabled, ok := containerDetails.Config[cfgBatchingEnabledKey]; ok {
		cfg.BatchEnabled, err = parseBool(batchingEnabled, true)
		if err != nil {
			return nil, fmt.Errorf("failed to parse %q option: %w", cfgBatchingEnabledKey, err)
		}
	}

	if maxWaitTime, ok := containerDetails.Config[cfgBatchingMaxWaitTimeKey]; ok {
		cfg.BatchMaxWait, err = time.ParseDuration(maxWaitTime)
		if err != nil {
			return nil, fmt.Errorf("failed to parse %q option: %w", cfgBatchingMaxWaitTimeKey, err)
		}
		if cfg.BatchMaxWait < 1*time.Second {
			return nil, fmt.Errorf("invalid %q option: %s", cfgBatchingMaxWaitTimeKey, cfg.BatchMaxWait)
		}
	}

	if err := cfg.Validate(containerDetails.Config); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c loggerConfig) Validate(opts map[string]string) error {
	if err := validateDriverOptions(opts); err != nil {
		return err
	}
	return c.ClientConfig.Validate()
}

func validateDriverOptions(opts map[string]string) error {
	for opt := range opts {
		switch opt {
		case cfgURLKey,
			cfgTokenKey,
			cfgChatIDKey,
			cfgRetriesKey,
			cfgTimeoutKey,
			cfgTemplateKey,
			cfgFilterRegexKey,
			cfgBatchingEnabledKey,
			cfgBatchingMaxWaitTimeKey:
		case "max-file", "max-size", "compress", "labels", "labels-regex", "env", "env-regex", "tag":
		case cfgNoFileKey, cfgKeepFileKey:
		default:
			return fmt.Errorf("unknown log opt '%s' for telegram log driver", opt)
		}
	}

	return nil
}

func parseClientConfig(containerDetails *ContainerDetails) (ClientConfig, error) {
	clientConfig := ClientConfig{
		APIURL:  defaultClientConfig.APIURL,
		Token:   containerDetails.Config[cfgTokenKey],
		ChatID:  containerDetails.Config[cfgChatIDKey],
		Retries: defaultClientConfig.Retries,
		Timeout: defaultClientConfig.Timeout,
	}

	if url, ok := containerDetails.Config[cfgURLKey]; ok {
		clientConfig.APIURL = url
	}

	if retries, ok := containerDetails.Config[cfgRetriesKey]; ok {
		var err error
		clientConfig.Retries, err = strconv.Atoi(retries)
		if err != nil {
			return clientConfig, fmt.Errorf("failed to parse %q option: %w", cfgRetriesKey, err)
		}
		if clientConfig.Retries < 0 {
			return clientConfig, fmt.Errorf("invalid %q option: %d", cfgRetriesKey, clientConfig.Retries)
		}
	}

	if timeout, ok := containerDetails.Config[cfgTimeoutKey]; ok {
		var err error
		clientConfig.Timeout, err = time.ParseDuration(timeout)
		if err != nil {
			return clientConfig, fmt.Errorf("failed to parse %q option: %w", cfgTimeoutKey, err)
		}
	}

	return clientConfig, nil
}

func parseBool(value string, defaultValue bool) (bool, error) {
	if value == "" {
		return defaultValue, nil
	}

	return strconv.ParseBool(value)
}
