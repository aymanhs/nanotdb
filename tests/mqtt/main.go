package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"
)

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

func main() {
	broker := flag.String("broker", "127.0.0.1:1883", "MQTT broker host:port")
	username := flag.String("username", "", "MQTT username")
	password := flag.String("password", "", "MQTT password")
	clientID := flag.String("client-id", "nanotdb-client", "MQTT client identifier")
	topic := flag.String("topic", "nanotdb/#", "MQTT topic to subscribe to")
	keepAlive := flag.Int("keepalive", 60, "MQTT keepalive interval in seconds")
	verbose := flag.Bool("verbose", false, "enable verbose logging")
	flag.Parse()

	conn, err := net.Dial("tcp", *broker)
	if err != nil {
		log.Fatalf("failed to dial broker %q: %v", *broker, err)
	}
	defer conn.Close()

	client := &MQTTClient{
		Conn:      conn,
		ClientID:  *clientID,
		Username:  *username,
		Password:  *password,
		KeepAlive: uint16(*keepAlive),
		Verbose:   *verbose,
	}

	if err := client.Connect(); err != nil {
		log.Fatalf("failed to connect: %v", err)
	}

	topics := parseTopics(*topic)
	if err := client.Subscribe(topics); err != nil {
		log.Fatalf("failed to subscribe: %v", err)
	}

	client.Run()
}

type MQTTClient struct {
	Conn      net.Conn
	ClientID  string
	Username  string
	Password  string
	KeepAlive uint16
	Verbose   bool
}

func parseTopics(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func (c *MQTTClient) Connect() error {
	payload, err := c.buildConnectPayload()
	if err != nil {
		return err
	}
	if err := sendFrame(c.Conn, pCONNECT, payload); err != nil {
		return fmt.Errorf("send CONNECT: %w", err)
	}

	packetType, data, err := readPacket(c.Conn)
	if err != nil {
		return fmt.Errorf("read CONNACK: %w", err)
	}
	if packetType != pCONNACK {
		return fmt.Errorf("unexpected packet type 0x%02x", packetType)
	}
	if len(data) < 2 {
		return fmt.Errorf("invalid CONNACK payload")
	}
	if data[1] != 0x00 {
		return fmt.Errorf("connection refused, code 0x%02x", data[1])
	}
	if c.Verbose {
		log.Printf("connected to broker %s", c.Conn.RemoteAddr())
	}
	return nil
}

func (c *MQTTClient) Subscribe(topics []string) error {
	if len(topics) == 0 {
		return fmt.Errorf("no topic specified")
	}

	var payload bytes.Buffer
	binary.Write(&payload, binary.BigEndian, uint16(1))
	for _, topic := range topics {
		if topic == "" {
			continue
		}
		writeUTF8String(&payload, topic)
		payload.WriteByte(0x00)
	}

	if err := sendFrame(c.Conn, pSUBSCRIBE|0x02, payload.Bytes()); err != nil {
		return fmt.Errorf("send SUBSCRIBE: %w", err)
	}

	packetType, data, err := readPacket(c.Conn)
	if err != nil {
		return fmt.Errorf("read SUBACK: %w", err)
	}
	if packetType != pSUBACK {
		return fmt.Errorf("unexpected packet type 0x%02x", packetType)
	}
	if len(data) < 3 {
		return fmt.Errorf("invalid SUBACK payload")
	}
	for i, returnCode := range data[2:] {
		if returnCode == 0x80 {
			return fmt.Errorf("subscription rejected for topic %q", topics[i])
		}
	}

	if c.Verbose {
		log.Printf("subscribed to %s", strings.Join(topics, ", "))
	}
	return nil
}

func (c *MQTTClient) Run() {
	ticker := time.NewTicker(time.Duration(c.KeepAlive/2) * time.Second)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
			if err := sendFrame(c.Conn, pPINGREQ, nil); err != nil {
				log.Printf("keepalive: failed to send PINGREQ: %v", err)
				return
			}
			if c.Verbose {
				log.Printf("sent PINGREQ")
			}
		}
	}()

	for {
		packetType, data, err := readPacket(c.Conn)
		if err != nil {
			log.Fatalf("read packet: %v", err)
		}

		switch packetType {
		case pPUBLISH:
			topic, payload, err := parsePublishPayload(data)
			if err != nil {
				log.Printf("parse PUBLISH: %v", err)
				continue
			}
			fmt.Printf("[Nanotdb Ingest] topic=%s payload=%s\n", topic, string(payload))
		case pPINGRESP:
			if c.Verbose {
				log.Println("received PINGRESP")
			}
		case pDISCONNECT:
			log.Println("broker requested disconnect")
			return
		default:
			if c.Verbose {
				log.Printf("ignored packet type 0x%02x", packetType)
			}
		}
	}
}

func (c *MQTTClient) Disconnect() {
	if err := sendFrame(c.Conn, pDISCONNECT, nil); err != nil {
		log.Printf("failed to send DISCONNECT: %v", err)
	}
	c.Conn.Close()
}

func (c *MQTTClient) buildConnectPayload() ([]byte, error) {
	var buf bytes.Buffer
	writeUTF8String(&buf, "MQTT")
	buf.WriteByte(0x04)
	connectFlags := byte(0x02)
	if c.Username != "" {
		connectFlags |= 0x80
	}
	if c.Password != "" {
		connectFlags |= 0x40
	}
	buf.WriteByte(connectFlags)
	binary.Write(&buf, binary.BigEndian, c.KeepAlive)
	writeUTF8String(&buf, c.ClientID)
	if c.Username != "" {
		writeUTF8String(&buf, c.Username)
	}
	if c.Password != "" {
		writeUTF8String(&buf, c.Password)
	}
	return buf.Bytes(), nil
}

func sendFrame(w io.Writer, packetType byte, payload []byte) error {
	var frame bytes.Buffer
	frame.WriteByte(packetType)

	lengthBytes := encodeRemainingLength(len(payload))
	frame.Write(lengthBytes)
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
