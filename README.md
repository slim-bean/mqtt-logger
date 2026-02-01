# mqtt-logger

A lightweight Go-based MQTT to Loki logger that replaces Promtail. Subscribes to MQTT topics and forwards messages to Grafana Loki with proper labeling and batching.

## Features

- Direct MQTT subscription (no external dependencies)
- Automatic batching for efficient Loki writes
- Configurable topics via environment variables
- Automatic reconnection handling
- Extracts `mqtt_node` label from topic structure
- Small Docker image (~20MB)

## Environment Variables

- `MQTT_LOGGER_MQTT_HOSTNAME` (required) - MQTT broker hostname
- `MQTT_LOGGER_MQTT_PORT` (default: `1883`) - MQTT broker port
- `MQTT_LOGGER_MQTT_USERNAME` (required) - MQTT username
- `MQTT_LOGGER_MQTT_PASSWORD` (required) - MQTT password
- `MQTT_LOGGER_MQTT_TOPICS` (default: `+/debug homeassistant/#`) - Space-separated list of MQTT topics to subscribe to
- `MQTT_LOGGER_LOKI_CLIENT_URL` (required) - Loki push API URL (e.g., `http://loki:3100/loki/api/v1/push`)
- `MQTT_LOGGER_HOST_HOSTNAME` (required) - Hostname label for Loki logs
- `TZ` (optional) - Timezone (default: `America/Chicago`)

## Building

```bash
docker build -f Dockerfile -t mqtt-logger:latest .
```

## Running

See `docker-compose.yml` for example configuration.

## How It Works

1. Connects to MQTT broker and subscribes to configured topics
2. Receives messages and batches them (100 messages or 5 second timeout)
3. Groups messages by `mqtt_topic` and `mqtt_node` labels
4. Extracts `mqtt_node` from the first segment of the topic (e.g., `node1/sensor/temp` → `node1`)
5. Sends batched logs to Loki with labels: `job`, `host_hostname`, `mqtt_topic`, `mqtt_node`

## Migration from Promtail

This replaces the previous Promtail-based implementation:
- No longer requires `mosquitto-clients` or Promtail
- Single Go binary handles everything
- Smaller Docker image
- Better error handling and reconnection logic
