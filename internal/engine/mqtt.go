package engine

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// mqttDialTimeout caps how long a single TCP connection attempt to the broker
// may take before we treat it as a transient failure and fall through to the
// retry loop. Independent of mqtt.keepalive, which governs an established
// session.
const mqttDialTimeout = 10 * time.Second

// mqttPermanentError marks a broker error that will not succeed on retry —
// authentication failures, protocol mismatches, ACL-rejected subscriptions,
// etc. The worker exits its retry loop when it sees one of these so we don't
// hammer the broker (or the journal) every retry interval forever.
type mqttPermanentError struct {
	err error
}

func (e *mqttPermanentError) Error() string { return e.err.Error() }
func (e *mqttPermanentError) Unwrap() error { return e.err }

func permanentMQTTError(err error) error { return &mqttPermanentError{err: err} }

const (
	pCONNECT    = 1 << 4
	pCONNACK    = 2 << 4
	pPUBLISH    = 3 << 4
	pSUBSCRIBE  = 8 << 4
	pSUBACK     = 9 << 4
	pPINGREQ    = 12 << 4
	pPINGRESP   = 13 << 4
	pDISCONNECT = 14 << 4
)

type mqttWorker struct {
	engine           *Engine
	broker           string
	username         string
	password         string
	clientID         string
	keepAlive        time.Duration
	retryEnabled     bool
	retryInterval    time.Duration
	retryMaxInterval time.Duration
	retryMaxAttempts int
	topics           []EngineConfigMQTTTopic
	conn             net.Conn
	stopCh           chan struct{}
	stopOnce         sync.Once
}

func (e *Engine) startMQTT(cfg EngineConfigMQTT) error {
	worker, err := newMQTTWorker(e, cfg)
	if err != nil {
		return err
	}
	e.mqttWorker = worker
	topicFilters := make([]string, 0, len(worker.topics))
	for _, t := range worker.topics {
		topicFilters = append(topicFilters, t.Topic)
	}
	e.logInfo("mqtt worker starting",
		"broker", worker.broker,
		"client_id", worker.clientID,
		"topics", topicFilters,
		"retry_enabled", worker.retryEnabled,
		"retry_interval", worker.retryInterval,
		"retry_max_interval", worker.retryMaxInterval,
	)
	go worker.run()
	return nil
}

func (e *Engine) stopMQTT() {
	if e == nil || e.mqttWorker == nil {
		return
	}
	e.mqttWorker.stop()
}

func newMQTTWorker(e *Engine, cfg EngineConfigMQTT) (*mqttWorker, error) {
	keepAlive, err := ParseDuration(strings.TrimSpace(cfg.KeepAlive))
	if err != nil {
		return nil, fmt.Errorf("invalid mqtt.keepalive: %w", err)
	}
	if keepAlive <= 0 {
		keepAlive = 60 * time.Second
	}

	retryInterval, err := ParseDuration(strings.TrimSpace(cfg.RetryInterval))
	if err != nil {
		return nil, fmt.Errorf("invalid mqtt.retry_interval: %w", err)
	}
	retryMaxInterval, err := ParseDuration(strings.TrimSpace(cfg.RetryMaxInterval))
	if err != nil {
		return nil, fmt.Errorf("invalid mqtt.retry_max_interval: %w", err)
	}
	if retryMaxInterval < retryInterval {
		return nil, fmt.Errorf("mqtt.retry_max_interval must be >= mqtt.retry_interval")
	}

	if strings.TrimSpace(cfg.ClientID) == "" {
		cfg.ClientID = "nanotdb-mqtt-ingest"
	}

	defaultFormat := strings.ToLower(strings.TrimSpace(cfg.Format))
	if defaultFormat == "" {
		defaultFormat = "json"
	}

	topics := make([]EngineConfigMQTTTopic, 0, len(cfg.Topics))
	for i, topic := range cfg.Topics {
		kind := strings.ToLower(strings.TrimSpace(topic.Type))
		topicFilter := strings.TrimSpace(topic.Topic)
		if kind != "metric" && kind != "event" {
			return nil, fmt.Errorf("invalid mqtt.topic[%d].type: %q", i, topic.Type)
		}
		if topicFilter == "" {
			return nil, fmt.Errorf("mqtt.topic[%d].topic must not be empty", i)
		}
		format := strings.ToLower(strings.TrimSpace(topic.Format))
		if format == "" {
			format = defaultFormat
		}
		if format != "json" && format != "text" {
			return nil, fmt.Errorf("invalid mqtt.topic[%d].format: %q", i, topic.Format)
		}
		topics = append(topics, EngineConfigMQTTTopic{
			Type:   kind,
			Topic:  topicFilter,
			DB:     strings.TrimSpace(topic.DB),
			Name:   strings.TrimSpace(topic.Name),
			Format: format,
		})
	}

	return &mqttWorker{
		engine:           e,
		broker:           strings.TrimSpace(cfg.Broker),
		username:         strings.TrimSpace(cfg.Username),
		password:         strings.TrimSpace(cfg.Password),
		clientID:         strings.TrimSpace(cfg.ClientID),
		keepAlive:        keepAlive,
		retryEnabled:     cfg.RetryEnabled,
		retryInterval:    retryInterval,
		retryMaxInterval: retryMaxInterval,
		retryMaxAttempts: cfg.RetryMaxAttempts,
		topics:           topics,
		stopCh:           make(chan struct{}),
	}, nil
}

func (w *mqttWorker) run() {
	// Recover from any panic in the MQTT worker so a bug here cannot take
	// the whole engine process down. Failure is reported via the standard
	// internal event channel; the worker simply exits and the engine keeps
	// running without MQTT ingest.
	defer func() {
		if r := recover(); r != nil {
			w.engine.logInfo("mqtt worker panic", "broker", w.broker, "err", fmt.Sprint(r), "stack", string(debug.Stack()))
			w.closeConn()
			w.engine.emitInternalEvent("nanotdb.mqtt", "nanotdb.mqtt.disconnected", nil, map[string]any{
				"broker": w.broker,
				"reason": fmt.Sprintf("panic: %v", r),
			}, "")
		}
	}()

	backoff := w.retryInterval
	attempts := 0

	for {
		if err := w.connect(); err != nil {
			w.closeConn()
			if w.handlePermanent("connect", err) {
				return
			}
			w.engine.logInfo("mqtt broker connect failed", "broker", w.broker, "err", err)
			if !w.retryEnabled || !w.waitRetry(backoff) {
				return
			}
			attempts++
			if w.retryMaxAttempts > 0 && attempts >= w.retryMaxAttempts {
				w.engine.logInfo("mqtt broker retry limit reached", "attempts", attempts)
				return
			}
			backoff = minDuration(backoff*2, w.retryMaxInterval)
			continue
		}

		if err := w.subscribe(); err != nil {
			w.closeConn()
			if w.handlePermanent("subscribe", err) {
				return
			}
			w.engine.logInfo("mqtt subscribe failed", "err", err)
			if !w.retryEnabled || !w.waitRetry(backoff) {
				return
			}
			attempts++
			if w.retryMaxAttempts > 0 && attempts >= w.retryMaxAttempts {
				w.engine.logInfo("mqtt broker retry limit reached", "attempts", attempts)
				return
			}
			backoff = minDuration(backoff*2, w.retryMaxInterval)
			continue
		}

		w.engine.logInfo("mqtt connected", "broker", w.broker, "client_id", w.clientID)
		w.engine.emitInternalEvent("nanotdb.mqtt", "nanotdb.mqtt.connected", nil, map[string]any{
			"broker": w.broker,
		}, "")
		attempts = 0
		backoff = w.retryInterval

		sessionDone := make(chan struct{})
		ticker := time.NewTicker(w.keepAlive / 2)
		go func() {
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					_ = sendFrame(w.conn, pPINGREQ, nil)
				case <-sessionDone:
					return
				}
			}
		}()

	sessionLoop:
		for {
			packetType, payload, err := readPacket(w.conn)
			if err != nil {
				w.engine.logInfo("mqtt reader stopped", "err", err)
				break
			}

			switch packetType {
			case pPUBLISH:
				topic, body, err := parsePublishPayload(payload)
				if err != nil {
					w.engine.logInfo("mqtt publish parse failed", "err", err)
					continue
				}
				w.handlePublish(topic, body)
			case pPINGRESP:
				continue
			case pDISCONNECT:
				w.engine.logInfo("mqtt broker requested disconnect")
				break sessionLoop
			default:
				continue
			}
		}

		close(sessionDone)
		w.closeConn()
		w.engine.emitInternalEvent("nanotdb.mqtt", "nanotdb.mqtt.disconnected", nil, map[string]any{
			"broker": w.broker,
			"reason": "session_ended",
		}, "")
		if !w.retryEnabled || !w.waitRetry(backoff) {
			return
		}
		attempts++
		if w.retryMaxAttempts > 0 && attempts >= w.retryMaxAttempts {
			w.engine.logInfo("mqtt broker retry limit reached", "attempts", attempts)
			return
		}
		backoff = minDuration(backoff*2, w.retryMaxInterval)
	}
}

func (w *mqttWorker) stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
		w.closeConn()
	})
}

func (w *mqttWorker) closeConn() {
	if w.conn != nil {
		_ = w.conn.Close()
		w.conn = nil
	}
}

func (w *mqttWorker) waitRetry(backoff time.Duration) bool {
	if backoff <= 0 {
		backoff = time.Second
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-w.stopCh:
		return false
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (w *mqttWorker) connect() error {
	conn, err := net.DialTimeout("tcp", w.broker, mqttDialTimeout)
	if err != nil {
		return err
	}
	w.conn = conn

	var payload bytes.Buffer
	writeUTF8String(&payload, "MQTT")
	payload.WriteByte(0x04)
	connectFlags := byte(0x02)
	if w.username != "" {
		connectFlags |= 0x80
	}
	if w.password != "" {
		connectFlags |= 0x40
	}
	payload.WriteByte(connectFlags)
	binary.Write(&payload, binary.BigEndian, uint16(w.keepAlive.Seconds()))
	writeUTF8String(&payload, w.clientID)
	if w.username != "" {
		writeUTF8String(&payload, w.username)
	}
	if w.password != "" {
		writeUTF8String(&payload, w.password)
	}
	if err := sendFrame(w.conn, pCONNECT, payload.Bytes()); err != nil {
		return err
	}

	packetType, data, err := readPacket(w.conn)
	if err != nil {
		return err
	}
	if packetType != pCONNACK {
		return permanentMQTTError(fmt.Errorf("unexpected packet type 0x%02x", packetType))
	}
	if len(data) < 2 {
		return permanentMQTTError(fmt.Errorf("invalid CONNACK payload"))
	}
	// CONNACK return codes (MQTT 3.1.1, §3.2.2.3). Only 0x03 "Server
	// unavailable" is treated as transient — the rest indicate a
	// configuration or protocol problem that retrying cannot resolve.
	switch data[1] {
	case 0x00:
		return nil
	case 0x03:
		return fmt.Errorf("mqtt connect refused: server unavailable (0x03)")
	case 0x01:
		return permanentMQTTError(fmt.Errorf("mqtt connect refused: unacceptable protocol version (0x01)"))
	case 0x02:
		return permanentMQTTError(fmt.Errorf("mqtt connect refused: identifier rejected (0x02)"))
	case 0x04:
		return permanentMQTTError(fmt.Errorf("mqtt connect refused: bad username or password (0x04)"))
	case 0x05:
		return permanentMQTTError(fmt.Errorf("mqtt connect refused: not authorized (0x05)"))
	default:
		return permanentMQTTError(fmt.Errorf("mqtt connect refused code 0x%02x", data[1]))
	}
}

// handlePermanent reports a permanent (non-retriable) MQTT error and emits a
// disconnected internal event with a structured reason. Returns true when the
// caller should exit the worker loop without further retries.
func (w *mqttWorker) handlePermanent(stage string, err error) bool {
	var perm *mqttPermanentError
	if !errors.As(err, &perm) {
		return false
	}
	w.engine.logInfo("mqtt fatal error; not retrying", "stage", stage, "broker", w.broker, "err", err)
	w.engine.emitInternalEvent("nanotdb.mqtt", "nanotdb.mqtt.disconnected", nil, map[string]any{
		"broker": w.broker,
		"reason": "permanent: " + err.Error(),
	}, "")
	return true
}

func (w *mqttWorker) subscribe() error {
	var payload bytes.Buffer
	binary.Write(&payload, binary.BigEndian, uint16(1))
	for _, topic := range w.topics {
		writeUTF8String(&payload, topic.Topic)
		payload.WriteByte(0x00)
	}
	if err := sendFrame(w.conn, pSUBSCRIBE|0x02, payload.Bytes()); err != nil {
		return err
	}

	packetType, data, err := readPacket(w.conn)
	if err != nil {
		return err
	}
	if packetType != pSUBACK {
		return fmt.Errorf("unexpected packet type 0x%02x", packetType)
	}
	if len(data) < 3 {
		return fmt.Errorf("invalid SUBACK payload")
	}
	for i, returnCode := range data[2:] {
		if returnCode == 0x80 {
			return permanentMQTTError(fmt.Errorf("mqtt subscription rejected for topic %q", w.topics[i].Topic))
		}
	}
	topicFilters := make([]string, 0, len(w.topics))
	for _, t := range w.topics {
		topicFilters = append(topicFilters, t.Topic)
	}
	w.engine.logInfo("mqtt subscribed", "broker", w.broker, "topics", topicFilters)
	return nil
}

func (w *mqttWorker) handlePublish(topic string, payload []byte) {
	cfg := w.findTopicConfig(topic)
	if cfg == nil {
		w.engine.logDebug("mqtt publish ignored: no matching topic", "topic", topic)
		return
	}

	db, name := deriveTopicMetadata(*cfg, topic)
	if db == "" {
		w.engine.logInfo("mqtt publish ignored: db not derivable", "topic", topic, "filter", cfg.Topic)
		return
	}
	if name == "" {
		w.engine.logInfo("mqtt publish ignored: name not derivable", "topic", topic, "filter", cfg.Topic)
		return
	}

	switch cfg.Type {
	case "metric":
		value, ts, err := parseMetricPayload(payload, cfg.Format)
		if err != nil {
			w.engine.logInfo("mqtt metric parse failed", "topic", topic, "err", err)
			return
		}
		if err := w.engine.AddSample(db, name, ts, value); err != nil {
			w.engine.logInfo("mqtt metric ingest failed", "database", db, "metric", name, "err", err)
		}
	case "event":
		value, ts, eventPayload, err := parseEventPayload(payload, cfg.Format)
		if err != nil {
			w.engine.logInfo("mqtt event parse failed", "topic", topic, "err", err)
			return
		}
		if err := w.engine.AddEvent(db, name, ts, value, eventPayload); err != nil {
			w.engine.logInfo("mqtt event ingest failed", "database", db, "event", name, "err", err)
		}
	}
}

func (w *mqttWorker) findTopicConfig(topic string) *EngineConfigMQTTTopic {
	for i := range w.topics {
		if matchesTopicFilter(w.topics[i].Topic, topic) {
			return &w.topics[i]
		}
	}
	return nil
}

func deriveTopicMetadata(cfg EngineConfigMQTTTopic, topic string) (string, string) {
	db := strings.TrimSpace(cfg.DB)
	name := strings.TrimSpace(cfg.Name)
	if db != "" && name != "" {
		return db, name
	}
	parts := extractWildcardParts(cfg.Topic, topic)
	if name == "" {
		name = strings.Join(parts, "/")
	}
	if db == "" && len(parts) > 1 {
		db = parts[0]
		if name == strings.Join(parts, "/") {
			name = strings.Join(parts[1:], "/")
		}
	}
	return db, name
}

func extractWildcardParts(filter, topic string) []string {
	filterParts := strings.Split(filter, "/")
	topicParts := strings.Split(topic, "/")
	var parts []string
	for i, frag := range filterParts {
		if frag == "#" {
			if i < len(topicParts) {
				parts = append(parts, strings.Join(topicParts[i:], "/"))
			}
			break
		}
		if frag == "+" {
			if i < len(topicParts) {
				parts = append(parts, topicParts[i])
			}
		}
	}
	return parts
}

func matchesTopicFilter(filter, topic string) bool {
	filterParts := strings.Split(filter, "/")
	topicParts := strings.Split(topic, "/")
	for idx, frag := range filterParts {
		if frag == "#" {
			return true
		}
		if idx >= len(topicParts) {
			return false
		}
		if frag == "+" {
			continue
		}
		if frag != topicParts[idx] {
			return false
		}
	}
	return len(topicParts) == len(filterParts)
}

func parseMetricPayload(payload []byte, format string) (any, Timestamp, error) {
	if format == "json" {
		return parseMetricJSONPayload(payload)
	}
	return parseMetricTextPayload(payload)
}

func parseMetricJSONPayload(payload []byte) (any, Timestamp, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, 0, err
	}
	valueRaw, ok := raw["value"]
	if !ok {
		return nil, 0, fmt.Errorf("missing value")
	}
	value, err := parseNumericValue(valueRaw, "")
	if err != nil {
		return nil, 0, err
	}
	ts := Timestamp(time.Now().UnixNano())
	if tsRaw, ok := raw["ts_ns"]; ok {
		var parsedTs int64
		if err := json.Unmarshal(tsRaw, &parsedTs); err != nil {
			return nil, 0, err
		}
		ts = Timestamp(parsedTs)
	}
	return value, ts, nil
}

func parseMetricTextPayload(payload []byte) (any, Timestamp, error) {
	parts := strings.Fields(string(payload))
	if len(parts) == 0 {
		return nil, 0, fmt.Errorf("empty payload")
	}
	value, err := parseNumericString(parts[0])
	if err != nil {
		return nil, 0, err
	}
	ts := Timestamp(time.Now().UnixNano())
	if len(parts) > 1 {
		tsVal, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil, 0, err
		}
		ts = Timestamp(tsVal)
	}
	return value, ts, nil
}

func parseEventPayload(payload []byte, format string) (any, Timestamp, []byte, error) {
	if format == "json" {
		return parseEventJSONPayload(payload)
	}
	return parseEventTextPayload(payload)
}

func parseEventJSONPayload(payload []byte) (any, Timestamp, []byte, error) {
	var raw interface{}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, 0, nil, err
	}

	obj, ok := raw.(map[string]interface{})
	if !ok {
		return nil, 0, payload, nil
	}

	var value any
	if rawValue, ok := obj["value"]; ok {
		valueParsed, err := parseJSONValue(rawValue, "")
		if err != nil {
			return nil, 0, nil, err
		}
		value = valueParsed
	}

	ts := Timestamp(time.Now().UnixNano())
	if rawTs, ok := obj["ts_ns"]; ok {
		switch v := rawTs.(type) {
		case float64:
			ts = Timestamp(int64(v))
		case int64:
			ts = Timestamp(v)
		case json.Number:
			parsed, err := v.Int64()
			if err != nil {
				return nil, 0, nil, err
			}
			ts = Timestamp(parsed)
		default:
			return nil, 0, nil, fmt.Errorf("invalid ts_ns type")
		}
	}

	payloadBytes := []byte(nil)
	if rawPayload, ok := obj["payload"]; ok {
		normalized, err := json.Marshal(rawPayload)
		if err != nil {
			return nil, 0, nil, err
		}
		payloadBytes = normalized
	}
	return value, ts, payloadBytes, nil
}

func parseEventTextPayload(payload []byte) (any, Timestamp, []byte, error) {
	body := string(payload)
	separator := strings.Index(body, "|")
	var value any
	var data string
	if separator >= 0 {
		left := body[:separator]
		data = body[separator+1:]
		if left != "" {
			parsed, err := parseNumericString(left)
			if err != nil {
				return nil, 0, nil, err
			}
			value = parsed
		}
	} else {
		data = body
	}
	return value, Timestamp(time.Now().UnixNano()), []byte(data), nil
}

func parseNumericValue(raw json.RawMessage, valueType string) (any, error) {
	var num float64
	if err := json.Unmarshal(raw, &num); err != nil {
		return nil, err
	}
	return parseNumericLiteral(num, valueType)
}

func parseNumericString(raw string) (any, error) {
	if strings.ContainsAny(raw, ".eE") {
		f, err := strconv.ParseFloat(raw, 32)
		if err != nil {
			return nil, err
		}
		return float32(f), nil
	}
	if i, err := strconv.ParseInt(raw, 10, 32); err == nil {
		return int32(i), nil
	}
	f, err := strconv.ParseFloat(raw, 32)
	if err != nil {
		return nil, err
	}
	return float32(f), nil
}

func parseJSONValue(value interface{}, valueType string) (any, error) {
	switch v := value.(type) {
	case float64:
		return parseNumericLiteral(v, valueType)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return nil, err
		}
		return parseNumericLiteral(f, valueType)
	default:
		return nil, fmt.Errorf("unsupported JSON numeric value type %T", value)
	}
}

func parseNumericLiteral(v float64, valueType string) (any, error) {
	if strings.EqualFold(valueType, "int32") {
		if v != math.Trunc(v) || v < math.MinInt32 || v > math.MaxInt32 {
			return nil, fmt.Errorf("value not representable as int32")
		}
		return int32(v), nil
	}
	if strings.EqualFold(valueType, "float32") {
		return float32(v), nil
	}
	if v == math.Trunc(v) && v >= math.MinInt32 && v <= math.MaxInt32 {
		return int32(v), nil
	}
	return float32(v), nil
}

func sendFrame(w io.Writer, packetType byte, payload []byte) error {
	var frame bytes.Buffer
	frame.WriteByte(packetType)
	frame.Write(encodeRemainingLength(len(payload)))
	frame.Write(payload)
	_, err := w.Write(frame.Bytes())
	return err
}

func readPacket(r io.Reader) (packetType byte, payload []byte, err error) {
	header := make([]byte, 1)
	if _, err = io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}
	packetType = header[0] & 0xF0
	length, err := parseVariableLength(r)
	if err != nil {
		return 0, nil, err
	}
	payload = make([]byte, length)
	_, err = io.ReadFull(r, payload)
	return packetType, payload, err
}

func parsePublishPayload(data []byte) (topic string, payload []byte, err error) {
	if len(data) < 2 {
		return "", nil, fmt.Errorf("publish frame too short")
	}
	topicLen := binary.BigEndian.Uint16(data[0:2])
	remain := data[2:]
	if len(remain) < int(topicLen) {
		return "", nil, fmt.Errorf("publish topic length exceeds remaining payload")
	}
	topic = string(remain[:topicLen])
	payload = remain[topicLen:]
	return topic, payload, nil
}

func writeUTF8String(w io.Writer, value string) {
	binary.Write(w, binary.BigEndian, uint16(len(value)))
	w.Write([]byte(value))
}

func encodeRemainingLength(length int) []byte {
	var encoded []byte
	for {
		digit := byte(length % 128)
		length /= 128
		if length > 0 {
			digit |= 0x80
		}
		encoded = append(encoded, digit)
		if length == 0 {
			break
		}
	}
	return encoded
}

func parseVariableLength(r io.Reader) (int, error) {
	multiplier := 1
	value := 0
	b := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, b); err != nil {
			return 0, err
		}
		value += int(b[0]&127) * multiplier
		if b[0]&128 == 0 {
			break
		}
		multiplier *= 128
		if multiplier > 128*128*128 {
			return 0, fmt.Errorf("malformed remaining length")
		}
	}
	return value, nil
}
