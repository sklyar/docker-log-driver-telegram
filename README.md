# Telegram Log Driver

This Docker plugin allows you to collect logs from your containers and send them to a specified Telegram chat using a Telegram Bot.

## Installation

Run the following command to install the plugin:

```bash
docker plugin install sklyarx/docker-log-driver-telegram:latest --alias telegram --grant-all-permissions
```

To check installed plugins, use the `docker plugin ls` command. Plugins that have started successfully are listed as enabled:

```bash
$ docker plugin ls
ID                  NAME                DESCRIPTION           ENABLED
ac720b8fcfdb        telegram            Telegram Logging Driver   true
```

## Upgrading

The upgrade process involves disabling the existing plugin, upgrading, then re-enabling and restarting Docker:

```bash
docker plugin disable telegram --force
docker plugin upgrade telegram sklyarx/docker-log-driver-telegram:latest --grant-all-permissions
docker plugin enable telegram
systemctl restart docker
```

## Uninstalling

To cleanly uninstall the plugin, disable and remove it:

```bash
docker plugin disable telegram --force
docker plugin rm telegram
```

## Change the logging driver for a container

The `docker run` command can be configured to use a different logging driver than the Docker daemon’s default with the `--log-driver` flag. Any options that the logging driver supports can be set using the `--log-opt <NAME>=<VALUE>` flag. `--log-opt` can be passed multiple times for each option to be set.

The following command will start a container and send logs to a Telegram Bot, using a specified API token and chat ID:

```bash
docker run --log-driver=telegram \
    --log-opt token="<your_bot_token>" \
    --log-opt chat_id="<your_chat_id>" \
    your_image
```

## Change the default logging driver

If you want the Telegram logging driver to be the default for all containers, change Docker’s `daemon.json` file (located in `/etc/docker` on Linux) and set the value of `log-driver` to `telegram`:

```json
{
    "debug" : true,
    "log-driver": "telegram",
    "log-opts": {
        "token": "<your_bot_token>",
        "chat_id": "<your_chat_id>"
    }
}
```

**Note**: `log-opt` configuration options in `daemon.json` must be provided as strings. Boolean and numeric values must therefore be enclosed in quotes (").

After changing `daemon.json`, restart the Docker daemon for the changes to take effect. All newly created containers from that host will then send logs to the Telegram Bot via the driver.

## Supported log-opt options

To specify additional logging driver options, you can use the `–log-opt NAME=VALUE` flag.

| Option       | Required? | Default Value            | Description                                                                                                                                                                                                                                          |
|--------------|-----------|--------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| url          | No        | https://api.telegram.org | Telegram API URL.                                                                                                                                                                                                                                    |
| token        | Yes       |                          | Telegram Bot API token.                                                                                                                                                                                                                              |
| chat_id      | Yes       |                          | Telegram Chat ID to send logs to.                                                                                                                                                                                                                    |
| retries      | No        | 5                        | The maximum number of retries. Setting it to 0 will retry indefinitely.                                                                                                                                                                              |
| timeout      | No        | 10s                      | The timeout to use when sending logs to the Telegram API. Valid time units are “ns”, “us” (or “µs”), “ms”, “s”, “m”, “h”.                                                                                                                            |
| no-file      | No        | false                    | Disable creation of log files on disk. This means you won’t be able to use `docker logs` on the container anymore. You can use this if you don’t need to use `docker logs` and you run with limited disk spacespace. (By default, files are created) |
| keep-file    | No        | false                    | Keep the JSON log files once the container is stopped. By default, files are removed, which means you won’t be able to use `docker logs` once the container is stopped.                                                                              |
| template     | No        | {log}                    | The Go template to format the log message. By default, the log will be sent as is. Example: `--log-opt template="{{.ID}}: {{.Name}} - {{.Message}}"`.                                                                                                |
| filter-regex | No        |                          | A regular expression to filter logs. Only logs that match the regex will be sent to the Telegram Bot. Example: `--log-opt filter-regex="ERROR`.                                                                                                      |WARN"`.

## Example Usage

### Using `docker run`

You can use the `--log-driver` flag to set the logging driver for a specific container. Use the `--log-opt` flag to set logging options. Here's an example:

```bash
docker run --log-driver=telegram \
    --log-opt token="<bot_token>" \
    --log-opt chat_id="<chat_id>" \
    your/image
```

### Using `docker-compose`

In your `docker-compose.yml` file, you can configure the logging driver and options for each service. Here's an example:

```yaml
version: '3.8'

services:
  my_service:
    image: your/image
    logging:
      driver: telegram
      options:
        token: "<bot_token>"
        chat_id: "<chat_id>"
```

In both examples, replace `<bot_token>` with your Telegram Bot token and `<chat_id>` with the chat ID where the logs will be sent.