package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type MQTTLogEntry struct {
	Topic   string
	Payload string
	Time    time.Time
}

type LokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"` // [timestamp_ns, log_line]
}

type LokiPushRequest struct {
	Streams []LokiStream `json:"streams"`
}

type Config struct {
	MQTTHostname string
	MQTTPort     string
	MQTTUsername string
	MQTTPassword string
	MQTTTopics   []string
	LokiURL      string
	HostHostname string
	Debug        bool
	MetricsPort  string
}

var (
	nodeRegex      = regexp.MustCompile(`^([^/]+)`)
	batchSize      = 100
	batchTimeout   = 1 * time.Second
	maxRetries     = 3
	retryBaseDelay = 1 * time.Second
	retryMaxDelay  = 10 * time.Second

	// Prometheus metrics
	mqttMessagesReceived = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mqtt_logger_messages_received_total",
			Help: "Total number of MQTT messages received",
		},
		[]string{"topic"},
	)

	mqttConnectionStatus = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "mqtt_logger_connection_status",
			Help: "MQTT connection status (1 = connected, 0 = disconnected)",
		},
	)

	batchSizeGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "mqtt_logger_batch_size",
			Help: "Current number of messages in batch",
		},
	)

	batchesCreated = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "mqtt_logger_batches_created_total",
			Help: "Total number of batches created",
		},
	)

	lokiPushesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mqtt_logger_loki_pushes_total",
			Help: "Total number of Loki push attempts",
		},
		[]string{"status"},
	)

	lokiPushDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mqtt_logger_loki_push_duration_seconds",
			Help:    "Duration of Loki push operations",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
		},
		[]string{"status"},
	)

	lokiRetriesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "mqtt_logger_loki_retries_total",
			Help: "Total number of Loki push retries",
		},
	)

	messagesInBatch = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mqtt_logger_messages_in_batch",
			Help: "Number of messages currently in batch",
		},
		[]string{"topic"},
	)

	mqttConnectionsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "mqtt_logger_connections_total",
			Help: "Total number of MQTT connections established",
		},
	)

	mqttReconnectionsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "mqtt_logger_reconnections_total",
			Help: "Total number of MQTT reconnections",
		},
	)

	mqttConnectionUptime = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "mqtt_logger_connection_uptime_seconds",
			Help: "Current MQTT connection uptime in seconds",
		},
	)

	mqttPingTimeoutsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "mqtt_logger_ping_timeouts_total",
			Help: "Total number of MQTT ping timeouts",
		},
	)
)

func init() {
	prometheus.MustRegister(mqttMessagesReceived)
	prometheus.MustRegister(mqttConnectionStatus)
	prometheus.MustRegister(batchSizeGauge)
	prometheus.MustRegister(batchesCreated)
	prometheus.MustRegister(lokiPushesTotal)
	prometheus.MustRegister(lokiPushDuration)
	prometheus.MustRegister(lokiRetriesTotal)
	prometheus.MustRegister(messagesInBatch)
	prometheus.MustRegister(mqttConnectionsTotal)
	prometheus.MustRegister(mqttReconnectionsTotal)
	prometheus.MustRegister(mqttConnectionUptime)
	prometheus.MustRegister(mqttPingTimeoutsTotal)
}

func main() {
	config := loadConfig()

	// Start metrics server
	go startMetricsServer(config.MetricsPort)

	log.Printf("Starting mqtt-logger with config:")
	log.Printf("  MQTT Broker: %s:%s", config.MQTTHostname, config.MQTTPort)
	log.Printf("  MQTT Topics: %v", config.MQTTTopics)
	log.Printf("  Loki URL: %s", config.LokiURL)
	log.Printf("  Host Hostname: %s", config.HostHostname)
	log.Printf("  Debug: %v", config.Debug)
	log.Printf("  Metrics Port: %s", config.MetricsPort)

	// Setup MQTT client
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("tcp://%s:%s", config.MQTTHostname, config.MQTTPort))
	// Use unique client ID to avoid conflicts
	opts.SetClientID(fmt.Sprintf("mqtt-logger-%d", time.Now().UnixNano()))
	opts.SetUsername(config.MQTTUsername)
	opts.SetPassword(config.MQTTPassword)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	// More aggressive keepalive to prevent EOF disconnects
	opts.SetKeepAlive(20 * time.Second)
	opts.SetPingTimeout(5 * time.Second)
	// Set clean session to false so broker remembers subscriptions
	opts.SetCleanSession(false)
	opts.SetDefaultPublishHandler(messageHandler(config))

	// Track if this is the initial connection
	var isInitialConnection sync.Once
	var initialConnectionDone sync.Mutex
	var hasConnectedBefore bool
	var connectionStartTime time.Time
	var connectionStartTimeMu sync.Mutex

	// Track connection status for uptime calculation
	var isConnected bool
	var isConnectedMu sync.Mutex

	// Start goroutine to update connection uptime metric
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			connectionStartTimeMu.Lock()
			isConnectedMu.Lock()
			if !connectionStartTime.IsZero() && isConnected {
				uptime := time.Since(connectionStartTime).Seconds()
				mqttConnectionUptime.Set(uptime)
			} else {
				mqttConnectionUptime.Set(0)
			}
			isConnectedMu.Unlock()
			connectionStartTimeMu.Unlock()
		}
	}()

	// Create a function to subscribe to topics (used both initially and on reconnect)
	subscribeToTopics := func(client mqtt.Client, isReconnect bool) {
		if len(config.MQTTTopics) == 0 {
			log.Fatal("No MQTT topics configured. Set MQTT_LOGGER_MQTT_TOPICS environment variable")
		}

		if isReconnect {
			log.Println("Re-subscribing to topics after reconnection...")
			// Small delay to ensure connection is fully established
			time.Sleep(100 * time.Millisecond)
		}

		for _, topic := range config.MQTTTopics {
			log.Printf("Subscribing to topic: %s", topic)
			if token := client.Subscribe(topic, 0, nil); token.Wait() && token.Error() != nil {
				log.Printf("ERROR: Failed to subscribe to topic %s: %v", topic, token.Error())
				// Don't fatal on reconnect failures, just log and continue
				continue
			}
			log.Printf("Successfully subscribed to topic: %s", topic)
			if config.Debug {
				log.Printf("DEBUG: Subscription confirmed for topic: %s", topic)
			}
		}
	}

	// Connection handlers for metrics
	opts.OnConnect = func(client mqtt.Client) {
		isReconnect := false
		isInitialConnection.Do(func() {
			// First connection - do nothing, will be handled below
		})

		// Check if we've connected before
		initialConnectionDone.Lock()
		if hasConnectedBefore {
			isReconnect = true
			mqttReconnectionsTotal.Inc()
		} else {
			hasConnectedBefore = true
		}
		initialConnectionDone.Unlock()

		// Track connection start time
		connectionStartTimeMu.Lock()
		connectionStartTime = time.Now()
		connectionStartTimeMu.Unlock()

		mqttConnectionsTotal.Inc()
		log.Println("MQTT client connected")
		mqttConnectionStatus.Set(1)
		isConnectedMu.Lock()
		isConnected = true
		isConnectedMu.Unlock()
		if config.Debug {
			log.Printf("DEBUG: MQTT connection established (reconnect: %v)", isReconnect)
		}

		// Re-subscribe to topics on reconnect
		subscribeToTopics(client, isReconnect)
	}

	opts.OnConnectionLost = func(client mqtt.Client, err error) {
		log.Printf("MQTT connection lost: %v", err)
		mqttConnectionStatus.Set(0)
		isConnectedMu.Lock()
		isConnected = false
		isConnectedMu.Unlock()

		// Reset connection start time
		connectionStartTimeMu.Lock()
		connectionStartTime = time.Time{}
		connectionStartTimeMu.Unlock()
		mqttConnectionUptime.Set(0)

		// Check if this was a ping timeout
		if err != nil && strings.Contains(err.Error(), "pingresp not received") {
			mqttPingTimeoutsTotal.Inc()
			log.Printf("MQTT ping timeout detected")
		}

		if config.Debug {
			log.Printf("DEBUG: MQTT connection lost, will attempt to reconnect")
		}
	}

	opts.OnReconnecting = func(client mqtt.Client, opts *mqtt.ClientOptions) {
		log.Println("MQTT client reconnecting...")
		if config.Debug {
			log.Printf("DEBUG: Reconnection attempt to %s", opts.Servers[0].String())
		}
	}

	client := mqtt.NewClient(opts)
	log.Printf("Attempting to connect to MQTT broker at %s:%s", config.MQTTHostname, config.MQTTPort)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("Failed to connect to MQTT broker: %v", token.Error())
	}
	log.Println("Connected to MQTT broker")
	mqttConnectionStatus.Set(1)

	// Subscribe to topics initially
	subscribeToTopics(client, false)

	log.Println("MQTT logger is running. Waiting for messages...")

	// Keep running
	select {}
}

func startMetricsServer(port string) {
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	addr := fmt.Sprintf(":%s", port)
	log.Printf("Starting metrics server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Failed to start metrics server: %v", err)
	}
}

func loadConfig() *Config {
	config := &Config{
		MQTTHostname: getEnv("MQTT_LOGGER_MQTT_HOSTNAME", ""),
		MQTTPort:     getEnv("MQTT_LOGGER_MQTT_PORT", "1883"),
		MQTTUsername: getEnv("MQTT_LOGGER_MQTT_USERNAME", ""),
		MQTTPassword: getEnv("MQTT_LOGGER_MQTT_PASSWORD", ""),
		LokiURL:      getEnv("MQTT_LOGGER_LOKI_CLIENT_URL", ""),
		HostHostname: getEnv("MQTT_LOGGER_HOST_HOSTNAME", ""),
		Debug:        getEnv("MQTT_LOGGER_DEBUG", "false") == "true",
		MetricsPort:  getEnv("MQTT_LOGGER_METRICS_PORT", "9090"),
	}

	// Parse topics from environment variable (space-separated)
	topicsEnv := getEnv("MQTT_LOGGER_MQTT_TOPICS", "")
	if topicsEnv != "" {
		config.MQTTTopics = strings.Fields(topicsEnv)
	}

	// Validate required config
	if config.MQTTHostname == "" {
		log.Fatal("MQTT_LOGGER_MQTT_HOSTNAME is required")
	}
	if config.LokiURL == "" {
		log.Fatal("MQTT_LOGGER_LOKI_CLIENT_URL is required")
	}

	return config
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func messageHandler(config *Config) mqtt.MessageHandler {
	// Batch buffer with mutex for thread safety
	type batchState struct {
		messages []MQTTLogEntry
		timer    *time.Timer
		mu       sync.Mutex
	}

	state := &batchState{
		messages: make([]MQTTLogEntry, 0, batchSize),
		timer:    time.NewTimer(batchTimeout),
	}

	// Flush function
	flush := func() {
		state.mu.Lock()
		if len(state.messages) == 0 {
			// Reset timer even when batch is empty so it continues to fire
			state.timer.Reset(batchTimeout)
			state.mu.Unlock()
			if config.Debug {
				log.Println("DEBUG: Flush called but batch is empty")
			}
			return
		}
		batch := make([]MQTTLogEntry, len(state.messages))
		copy(batch, state.messages)
		batchLength := len(state.messages)
		state.messages = state.messages[:0]
		state.timer.Reset(batchTimeout)
		state.mu.Unlock()

		if config.Debug {
			log.Printf("DEBUG: Flushing batch of %d messages", batchLength)
		}
		flushBatch(config, batch)
	}

	// Start timer goroutine
	go func() {
		for range state.timer.C {
			if config.Debug {
				log.Println("DEBUG: Batch timeout reached, triggering flush")
			}
			flush()
		}
	}()

	return func(client mqtt.Client, msg mqtt.Message) {
		topic := msg.Topic()
		payload := string(msg.Payload())

		if config.Debug {
			log.Printf("DEBUG: Received MQTT message on topic '%s', payload length: %d", topic, len(payload))
		}

		// Increment metrics
		mqttMessagesReceived.WithLabelValues(topic).Inc()

		// Create log entry from MQTT message
		entry := MQTTLogEntry{
			Topic:   topic,
			Payload: payload,
			Time:    time.Now(),
		}

		state.mu.Lock()
		state.messages = append(state.messages, entry)
		currentBatchSize := len(state.messages)
		shouldFlush := currentBatchSize >= batchSize
		state.mu.Unlock()

		// Update metrics
		batchSizeGauge.Set(float64(currentBatchSize))
		messagesInBatch.WithLabelValues(topic).Set(float64(currentBatchSize))

		if config.Debug {
			log.Printf("DEBUG: Added message to batch. Current batch size: %d/%d", currentBatchSize, batchSize)
		}

		if shouldFlush {
			if config.Debug {
				log.Printf("DEBUG: Batch size limit reached (%d), triggering flush", batchSize)
			}
			flush()
		}
	}
}

func flushBatch(config *Config, batch []MQTTLogEntry) {
	if len(batch) == 0 {
		return
	}

	batchesCreated.Inc()

	if config.Debug {
		log.Printf("DEBUG: Processing batch of %d messages", len(batch))
	}

	// Group messages by labels (mqtt_topic, mqtt_node)
	streams := make(map[string]*LokiStream)

	for _, entry := range batch {
		// Extract mqtt_node from topic
		mqttNode := extractNode(entry.Topic)

		if config.Debug {
			log.Printf("DEBUG: Processing message - topic: %s, node: %s, payload length: %d", entry.Topic, mqttNode, len(entry.Payload))
		}

		// Create label key for grouping
		labelKey := fmt.Sprintf("%s|%s", entry.Topic, mqttNode)

		stream, exists := streams[labelKey]
		if !exists {
			stream = &LokiStream{
				Stream: map[string]string{
					"job":        "mqtt_loki",
					"mqtt_topic": entry.Topic,
					"mqtt_node":  mqttNode,
				},
				Values: make([][]string, 0),
			}
			streams[labelKey] = stream
			if config.Debug {
				log.Printf("DEBUG: Created new stream for topic: %s, node: %s", entry.Topic, mqttNode)
			}
		}

		// Use entry timestamp
		timestamp := entry.Time.UnixNano()

		// Add log entry
		stream.Values = append(stream.Values, []string{
			fmt.Sprintf("%d", timestamp),
			entry.Payload,
		})
	}

	// Convert to Loki format
	lokiStreams := make([]LokiStream, 0, len(streams))
	for _, stream := range streams {
		lokiStreams = append(lokiStreams, *stream)
	}

	if config.Debug {
		log.Printf("DEBUG: Created %d Loki streams from batch", len(lokiStreams))
	}

	pushRequest := LokiPushRequest{
		Streams: lokiStreams,
	}

	// Send to Loki
	startTime := time.Now()
	err := sendToLoki(config, config.LokiURL, pushRequest)
	duration := time.Since(startTime).Seconds()

	if err != nil {
		log.Printf("Failed to send batch of %d messages to Loki: %v", len(batch), err)
		lokiPushesTotal.WithLabelValues("error").Inc()
		lokiPushDuration.WithLabelValues("error").Observe(duration)
	} else {
		log.Printf("Successfully sent batch of %d messages to Loki in %.3fs", len(batch), duration)
		lokiPushesTotal.WithLabelValues("success").Inc()
		lokiPushDuration.WithLabelValues("success").Observe(duration)
	}

	// Reset batch size gauge after flush
	batchSizeGauge.Set(0)
}

func extractNode(topic string) string {
	matches := nodeRegex.FindStringSubmatch(topic)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func sendToLoki(config *Config, url string, request LokiPushRequest) error {
	if config.Debug {
		log.Printf("DEBUG: Preparing to send %d streams to Loki at %s", len(request.Streams), url)
	}

	// Marshal to JSON
	jsonData, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	if config.Debug {
		log.Printf("DEBUG: Marshaled JSON size: %d bytes", len(jsonData))
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			lokiRetriesTotal.Inc()
			// Exponential backoff with jitter
			delay := retryBaseDelay * time.Duration(1<<uint(attempt-1))
			if delay > retryMaxDelay {
				delay = retryMaxDelay
			}
			log.Printf("Retrying Loki push (attempt %d/%d) after %v", attempt+1, maxRetries+1, delay)
			if config.Debug {
				log.Printf("DEBUG: Retry attempt %d, previous error: %v", attempt+1, lastErr)
			}
			time.Sleep(delay)
		}

		if config.Debug && attempt == 0 {
			log.Printf("DEBUG: Sending request to Loki (attempt 1)")
		}

		// Create HTTP request (need to recreate reader for each attempt)
		req, err := http.NewRequest("POST", url, bytes.NewReader(jsonData))
		if err != nil {
			lastErr = fmt.Errorf("failed to create request: %w", err)
			if config.Debug {
				log.Printf("DEBUG: Failed to create HTTP request: %v", err)
			}
			continue
		}

		req.Header.Set("Content-Type", "application/json")

		// Send request
		client := &http.Client{
			Timeout: 30 * time.Second,
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to send request: %w", err)
			if config.Debug {
				log.Printf("DEBUG: HTTP request failed: %v", err)
			}
			// Retry on network errors
			continue
		}

		statusCode := resp.StatusCode

		// Read response body for error messages
		bodyBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		bodyStr := ""
		if readErr == nil && len(bodyBytes) > 0 {
			bodyStr = string(bodyBytes)
		}

		if config.Debug {
			log.Printf("DEBUG: Loki responded with status code: %d", statusCode)
			if bodyStr != "" {
				log.Printf("DEBUG: Loki response body: %s", bodyStr)
			}
		}

		// Success
		if statusCode == http.StatusNoContent || statusCode == http.StatusOK {
			return nil
		}

		// Build error message with response body
		errorMsg := fmt.Sprintf("status code %d", statusCode)
		if bodyStr != "" {
			errorMsg = fmt.Sprintf("status code %d: %s", statusCode, bodyStr)
		}

		// Retry on 429 (Too Many Requests) - rate limiting
		if statusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("rate limited: %s", errorMsg)
			log.Printf("ERROR: Loki rate limited: %s", errorMsg)
			if config.Debug {
				log.Printf("DEBUG: Rate limited by Loki, will retry")
			}
			continue
		}

		// Retry on 5xx errors (server errors)
		if statusCode >= 500 && statusCode < 600 {
			lastErr = fmt.Errorf("server error: %s", errorMsg)
			log.Printf("ERROR: Loki server error: %s", errorMsg)
			if config.Debug {
				log.Printf("DEBUG: Loki server error, will retry")
			}
			continue
		}

		// Don't retry on other 4xx errors (client errors)
		log.Printf("ERROR: Loki client error: %s", errorMsg)
		return fmt.Errorf("client error: %s", errorMsg)
	}

	return fmt.Errorf("max retries exceeded: %w", lastErr)
}
