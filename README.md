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

## Configuration

### Container Level

Use with `docker run`:

```bash
docker run --log-driver=telegram \
    --log-opt token="<bot_token>" \
    --log-opt chat_id="<chat_id>" \
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

| Option               | Required | Default                  | Description                                                                                  |
|----------------------|----------|--------------------------|----------------------------------------------------------------------------------------------|
| url                  | No       | https://api.telegram.org | Telegram API URL                                                                             |
| token                | Yes      |                          | Bot API token                                                                                |
| chat_id              | Yes      |                          | Target chat ID                                                                               |
| template             | No       | {log}                    | Message format template                                                                      |
| filter-regex         | No       |                          | Regex to filter logs                                                                         |
| retries              | No       | 5                        | Max retry attempts (0 = infinite)                                                            |
| timeout              | No       | 10s                      | API request timeout (units: ns, us/µs, ms, s, m, h)                                          |
| no-file              | No       | false                    | Disable log files (disables `docker logs`)                                                   |
| keep-file            | No       | false                    | Keep log files after container stop                                                          |
| mode                 | No       | blocking                 | Log processing mode: `blocking`/`non-blocking`                                               |
| max-buffer-size      | No       | 1m                       | Max buffer size (Example values: 32, 32b, 32B, 32k, 32K, 32kb, 32Kb, 32Mb, 32Gb, 32Tb, 32Pb) |
| batch-enabled        | No       | true                     | Enable batch sending                                                                         |
| batch-flush-interval | No       | 3s                       | Batch flush interval (units: ns, us/µs, ms, s, m, h)                                         |

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

[pkg-img]: https://pkg.go.dev/badge/sklyar/docker-log-driver-telegram

[pkg-url]: https://pkg.go.dev/github.com/sklyar/docker-log-driver-telegram

[version-img]: https://img.shields.io/github/v/release/sklyar/docker-log-driver-telegram

[version-url]: https://github.com/sklyar/docker-log-driver-telegram/releases

[license-img]: https://img.shields.io/github/license/sklyar/docker-log-driver-telegram

[license-url]: https://raw.githubusercontent.com/sklyar/docker-log-driver-telegram/master/LICENSE