# MQTT Ingest

NanoTDB can ingest metrics and events directly from an MQTT broker using
engine-level configuration in `engine.toml`.

The MQTT feature is opt-in. Configure it under `[mqtt]` and declare one or more
`[[mqtt.topic]]` subscriptions.

## Configuration

```toml
[mqtt]
enabled = true
broker = "127.0.0.1:1883"
username = ""
password = ""
client_id = "nanotdb-mqtt-ingest"
format = "json"
keepalive = "60s"

[[mqtt.topic]]
type = "metric"
topic = "sensors/+/temperature"
# Optional per-topic overrides.
# db = "sensors"
# name = "temperature"
# format = "text"

[[mqtt.topic]]
type = "event"
topic = "alerts/#"
# Optional per-topic overrides.
# db = "alerts"
# name = "critical"
# format = "json"
```

### Config fields

- `enabled` — enable MQTT ingest.
- `broker` — MQTT broker address, e.g. `127.0.0.1:1883`.
- `username` / `password` — optional MQTT auth credentials.
- `client_id` — MQTT client identifier used on connect.
- `format` — default payload format when a topic entry omits `format`.
  Valid values: `json`, `text`.
- `keepalive` — MQTT keepalive interval. The engine sends keepalive pings
  to maintain the broker connection.

### Topic subscriptions

Each `[[mqtt.topic]]` entry declares a topic filter and ingest mode.

- `type = "metric"` — payloads are parsed as metric samples.
- `type = "event"` — payloads are parsed as events.
- `topic` — an MQTT subscription filter. Supports `+` and `#` wildcards.
- `db` — optional target database. If omitted, the worker derives the
  database name from wildcard matches.
- `name` — optional metric/event name. If omitted, the worker derives the
  name from wildcard matches.
- `format` — optional override for this topic's payload format.

## Payload formats

NanoTDB supports two payload styles:

- `json`
- `text`

The chosen format may be set globally in `[mqtt].format` and overridden per
subscription with `[[mqtt.topic]].format`.

### Metric JSON payload

For metric ingestion, the JSON payload must include `value`.

```json
{
  "value": 23.4,
  "ts_ns": 1680000000000000000
}
```

- `value` may be numeric and is mapped to the metric value.
- `ts_ns` is optional and is treated as a nanosecond timestamp.
  If omitted, the current wall-clock time is used.

### Metric text payload

Text mode accepts a numeric value followed optionally by a timestamp.

```
23.4 1680000000000000000
```

- First token is parsed as a number.
- Second token, if present, is parsed as `ts_ns`.
- If the timestamp is omitted, the current time is used.

### Event JSON payload

Event ingestion accepts any JSON object, with optional fields used for
metadata.

```json
{
  "value": 1,
  "ts_ns": 1680000000000000000,
  "payload": {
    "source": "sensor",
    "message": "temperature threshold exceeded"
  }
}
```

- `value` is optional and will be stored as the event value.
- `ts_ns` is optional and is treated as a nanosecond timestamp.
- `payload` may be any JSON value and is preserved as the event payload.

If the JSON body is not an object, the raw payload is passed through as-is.

### Event text payload

Plain text event payload also supports a compact `VALUE|payload` convention.

```
1|door opened
```

- `VALUE` is optional and parsed as the event value.
- `payload` is the text after `|`.
- If the separator `|` is absent, the entire message becomes the event
  payload.

## Topic matching and derived metadata

If `db` or `name` are omitted from a topic entry, NanoTDB derives them from
wildcard matches in the subscription filter.

Example:

```toml
[[mqtt.topic]]
type = "metric"
topic = "sensors/+/temperature"
```

A published topic like `sensors/room1/temperature` may derive:

- `db = "room1"`
- `name = "temperature"`

When a fixed `db` or `name` is provided, that value is used instead of the
derived value.

## Example publishes

### Publish a metric sample

```bash
mosquitto_pub -h 127.0.0.1 -p 1883 -t "sensors/room1/temperature" -m '{"value": 22.1, "ts_ns": 1680000000000000000}'
```

### Publish an event in JSON

```bash
mosquitto_pub -h 127.0.0.1 -p 1883 -t "alerts/critical" -m '{"value": 1, "ts_ns": 1680000000000000000, "payload": {"state": "open"}}'
```

### Publish an event in text

```bash
mosquitto_pub -h 127.0.0.1 -p 1883 -t "alerts/door" -m '1|door opened'
```

## Keepalive behavior

The MQTT worker uses the configured `keepalive` interval to keep the
connection alive with periodic ping requests. Set `keepalive` in `engine.toml`
so the broker does not close idle sessions.

Example:

```toml
[mqtt]
enabled = true
broker = "127.0.0.1:1883"
keepalive = "60s"
```

## Retry and reconnect behavior

When `mqtt.enabled` is true, the engine will automatically reconnect to the
broker if the connection fails or is dropped.

- `retry_enabled` — enable automatic reconnects.
- `retry_interval` — base retry delay after a failed connect or subscribe.
- `retry_max_interval` — maximum retry delay when backoff is applied.
- `retry_max_attempts` — maximum number of retries before giving up.
  Set to `0` for unlimited retries.

Defaults are:

```toml
retry_enabled = true
retry_interval = "5s"
retry_max_interval = "1m"
retry_max_attempts = 0
```

If the broker is temporarily unavailable, the MQTT worker will reconnect with
exponential backoff until the connection succeeds or the retry limit is hit.

## Notes

- The MQTT client built into the engine is a lightweight ingest worker, not a
  full MQTT broker.
- Only subscribed topics are processed; other broker traffic is ignored.
- Wildcard subscriptions follow MQTT semantics for `+` and `#`.
