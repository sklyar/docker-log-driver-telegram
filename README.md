# Telegram Log Driver

[![pkg-img]][pkg-url]
[![version-img]][version-url]
[![license-img]][license-url]

Docker logging driver that forwards container logs to Telegram chats via a bot.

## Quick Start

```bash
# Install plugin
docker plugin install sklyarx/docker-log-driver-telegram:latest --alias telegram --grant-all-permissions

# Run container with the driver
docker run --log-driver=telegram \
  --log-opt token="<bot_token>" \
  --log-opt chat_id="<chat_id>" \
  your_image
```

## Installation

### Plugin Management

```bash
# Install
docker plugin install sklyarx/docker-log-driver-telegram:latest --alias telegram --grant-all-permissions

# Upgrade
docker plugin disable telegram --force
docker plugin upgrade telegram sklyarx/docker-log-driver-telegram:latest --grant-all-permissions
docker plugin enable telegram
systemctl restart docker

# Uninstall
docker plugin disable telegram --force
docker plugin rm telegram
```

### Plugin Environment

```bash
docker plugin disable telegram --force
docker plugin set telegram ENV=production LOG_LEVEL=info
docker plugin enable telegram
```

| Variable  | Default     | Description                                                             |
|-----------|-------------|-------------------------------------------------------------------------|
| ENV       | development | Plugin runtime environment: `production` or `development`               |
| LOG_LEVEL | info        | Plugin log level: `debug`, `info`, `warn`, `error`, `fatal`, or `panic` |

## Configuration

### Container Level

Use with `docker run`:

```bash
docker run --log-driver=telegram \
    --log-opt token="<bot_token>" \
    --log-opt chat_id="<chat_id>" \
    --log-opt topic_id="<topic_id>" \
    --log-opt template="{container_name}: {log}" \
    your_image
```

Use with `docker-compose.yml`:

```yaml
version: '3.8'
services:
  app:
    image: your/image
    logging:
      driver: telegram
      options:
        token: "<bot_token>"
        chat_id: "<chat_id>"
        template: "{container_name}: {log}"
```

### Daemon Level (Default for All Containers)

Edit `/etc/docker/daemon.json`:

```json
{
  "log-driver": "telegram",
  "log-opts": {
    "token": "<bot_token>",
    "chat_id": "<chat_id>"
  }
}
```

Restart Docker after changes: `systemctl restart docker`

## Options

| Option               | Required | Default                  | Description                                                                     |
|----------------------|----------|--------------------------|---------------------------------------------------------------------------------|
| url                  | No       | https://api.telegram.org | Telegram API URL (`http` or `https`); a path such as `/telegram` is kept        |
| token                | Yes      |                          | Bot API token                                                                   |
| chat_id              | Yes      |                          | Target chat ID                                                                  |
| topic_id             | No       |                          | Target forum topic (message thread) ID, must be positive                        |
| template             | No       | {log}                    | Message format template                                                         |
| filter-regex         | No       |                          | Regex to filter logs                                                            |
| retries              | No       | 5                        | Retry attempts after the initial request (0 = no retries)                       |
| timeout              | No       | 10s                      | Timeout of one API request, must be positive (units: ns, us/µs, ms, s, m, h)    |
| no-file              | No       | false                    | Disable log files (disables `docker logs`)                                      |
| keep-file            | No       | false                    | Keep log files after container stop                                             |
| buffer-messages      | No       | 10000                    | Number of messages that wait to be sent; when it is full, the oldest is dropped |
| batch-enabled        | No       | true                     | Enable batch sending                                                            |
| batch-flush-interval | No       | 3s                       | Batch flush interval, at least 1s (units: ns, us/µs, ms, s, m, h)               |

`filter-regex` sends only log messages that match the regex. For a very long line, the regex is matched against the
first 64 KiB.

`topic_id` sends messages to a specific topic in a Telegram forum supergroup. It must be a positive integer.

The Docker options `mode` and `max-buffer-size` are also accepted. Docker uses them itself: with `mode=non-blocking`,
Docker keeps up to `max-buffer-size` of logs in its own buffer before it passes them to the driver.

### Delivery

- A message longer than the Telegram limit of 4096 characters is truncated and ends with `… (truncated)`.
- With batching, the driver joins log lines into one message up to the Telegram limit.
- Telegram allows about 20 messages per minute to one group. All containers that use the same bot and chat share this
  limit.
- Failed requests are retried: network errors, bad responses and server errors after a growing delay (1s, 2s, 4s, …,
  up to 30s), and rate limit errors after the delay that Telegram returns.
- When the buffer is full, the driver drops the oldest messages and writes the number of dropped messages to the
  plugin log.
- When a container stops, the driver first reads the logs that Docker has already passed to it (for at most 5
  seconds), then sends the buffered messages for at most 15 seconds and drops the rest.

### Template Tags

To customize the log message format using the `template` option, you can use the following tags:

| Tag                 | Description        |
|---------------------|--------------------|
| {log}               | Log message        |
| {timestamp}         | Log timestamp      |
| {container_id}      | Short container ID |
| {container_full_id} | Full container ID  |
| {container_name}    | Container name     |
| {image_id}          | Short image ID     |
| {image_full_id}     | Full image ID      |
| {image_name}        | Image name         |
| {daemon_name}       | Docker daemon name |

Extra attributes populated by Docker from `labels`, `labels-regex`, `env`, and `env-regex` can also be used as template
tags.

[pkg-img]: https://pkg.go.dev/badge/github.com/sklyar/docker-log-driver-telegram

[pkg-url]: https://pkg.go.dev/github.com/sklyar/docker-log-driver-telegram

[version-img]: https://img.shields.io/github/v/release/sklyar/docker-log-driver-telegram

[version-url]: https://github.com/sklyar/docker-log-driver-telegram/releases

[license-img]: https://img.shields.io/github/license/sklyar/docker-log-driver-telegram

[license-url]: https://raw.githubusercontent.com/sklyar/docker-log-driver-telegram/main/LICENSE
