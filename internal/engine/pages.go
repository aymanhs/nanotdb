package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
	"time"

	"github.com/klauspost/compress/s2"
)

var ErrOutOfOrderTimestamp = fmt.Errorf("timestamp must be non-decreasing")

const (
	PageMaxRecords = 12000
	PageMaxBytes   = 256 * 1024       // 256 KB estimated uncompressed payload
	PageMaxAge     = 10 * time.Second // max age before forced flush
	HeaderSize     = 18               // StartTime(8) + EndTime(8) + NumRecords(2)
)

var pageEncodeBufferPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, PageMaxBytes))
	},
}

var pageDecodeBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, PageMaxBytes)
		return &buf
	},
}

type PageHeader struct {
	StartTime  Timestamp
	EndTime    Timestamp
	NumRecords uint16
}

func (h *PageHeader) EncodeInto(w *bytes.Buffer) (err error) {
	write := func(v any) {
		if err != nil {
			return
		}
		err = binary.Write(w, binary.LittleEndian, v)
	}
	write(h.StartTime)
	write(h.EndTime)
	write(h.NumRecords)
	return err
}

func (h *PageHeader) Decode(r *bytes.Reader) (err error) {
	read := func(v any) {
		if err != nil {
			return
		}
		err = binary.Read(r, binary.LittleEndian, v)
	}
	read(&h.StartTime)
	read(&h.EndTime)
	read(&h.NumRecords)
	return err
}

// Page holds interleaved samples from multiple metrics.
// Value bytes are stored raw in a pre-allocated buffer; the catalog is responsible for interpreting them.
type Page struct {
	Start        Timestamp
	End          Timestamp
	Metrics      []MetricID
	Times        []Timestamp
	Values       *bytes.Buffer // raw value bytes buffer; catalog knows per-metric width/encoding
	MaxRecords   int
	MaxBytes     int
	MaxAge       time.Duration
	WALSegmentID uint16
	createdAt    time.Time
}

func NewPage(firstTS Timestamp) *Page {
	return NewPageWithLimits(firstTS, PageMaxRecords, PageMaxBytes, PageMaxAge)
}

func NewPageWithLimits(firstTS Timestamp, maxRecords, maxBytes int, maxAge time.Duration) *Page {
	if maxRecords <= 0 {
		maxRecords = PageMaxRecords
	}
	if maxBytes <= 0 {
		maxBytes = PageMaxBytes
	}
	if maxAge <= 0 {
		maxAge = PageMaxAge
	}
	return &Page{
		Start:      firstTS,
		End:        firstTS,
		Metrics:    make([]MetricID, 0, maxRecords),
		Times:      make([]Timestamp, 0, maxRecords),
		Values:     bytes.NewBuffer(make([]byte, 0, maxBytes)),
		MaxRecords: maxRecords,
		MaxBytes:   maxBytes,
		MaxAge:     maxAge,
		createdAt:  time.Now(),
	}
}

func (p *Page) StartTime() Timestamp { return p.Start }
func (p *Page) EndTime() Timestamp   { return p.End }

// AddSample appends a raw value for the given metric and timestamp.
// value must be exactly the bytes the catalog will use to decode this metric.
// Timestamps must be non-decreasing across all metrics in this page.
func (p *Page) AddSample(metricID MetricID, ts Timestamp, value []byte) error {
	if len(p.Times) > 0 && ts < p.Times[len(p.Times)-1] {
		return ErrOutOfOrderTimestamp
	}
	p.Metrics = append(p.Metrics, metricID)
	p.Times = append(p.Times, ts)
	if _, err := p.Values.Write(value); err != nil {
		return err
	}
	p.End = ts
	return nil
}

// IsFull returns true when any flush threshold is exceeded.
func (p *Page) IsFull() bool {
	maxRecords := p.MaxRecords
	if maxRecords <= 0 {
		maxRecords = PageMaxRecords
	}
	maxBytes := p.MaxBytes
	if maxBytes <= 0 {
		maxBytes = PageMaxBytes
	}
	maxAge := p.MaxAge
	if maxAge <= 0 {
		maxAge = PageMaxAge
	}

	if len(p.Times) >= maxRecords {
		return true
	}
	if len(p.Metrics)*2+len(p.Times)*8+p.Values.Len() >= maxBytes {
		return true
	}
	if !p.createdAt.IsZero() && time.Since(p.createdAt) >= maxAge {
		return true
	}
	return false
}

func (p *Page) SetWalSegmentID(segmentID uint16) {
	if p.WALSegmentID == 0 {
		p.WALSegmentID = segmentID
	}
}

func clonePage(p *Page) *Page {
	if p == nil {
		return nil
	}
	clone := &Page{
		Start:        p.Start,
		End:          p.End,
		Metrics:      append([]MetricID(nil), p.Metrics...),
		Times:        append([]Timestamp(nil), p.Times...),
		Values:       bytes.NewBuffer(append([]byte(nil), p.Values.Bytes()...)),
		MaxRecords:   p.MaxRecords,
		MaxBytes:     p.MaxBytes,
		MaxAge:       p.MaxAge,
		WALSegmentID: p.WALSegmentID,
		createdAt:    p.createdAt,
	}
	return clone
}

func (p *Page) EncodeInto(bb *bytes.Buffer) error {
	n := len(p.Times)
	h := PageHeader{
		StartTime:  p.Start,
		EndTime:    p.End,
		NumRecords: uint16(n),
	}
	if err := h.EncodeInto(bb); err != nil {
		return err
	}

	payload := pageEncodeBufferPool.Get().(*bytes.Buffer)
	payload.Reset()
	defer pageEncodeBufferPool.Put(payload)

	// Metrics: n × uint16
	for _, mid := range p.Metrics {
		var buf [2]byte
		binary.LittleEndian.PutUint16(buf[:], uint16(mid))
		if _, err := payload.Write(buf[:]); err != nil {
			return err
		}
	}

	// Times: n × uint64
	for _, ts := range p.Times {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(ts))
		if _, err := payload.Write(buf[:]); err != nil {
			return err
		}
	}

	// Values: raw blob, catalog-owned encoding
	if _, err := payload.Write(p.Values.Bytes()); err != nil {
		return err
	}

	compressed := s2.Encode(nil, payload.Bytes())

	var varintBuf [binary.MaxVarintLen64]byte
	nv := binary.PutUvarint(varintBuf[:], uint64(len(compressed)))
	if _, err := bb.Write(varintBuf[:nv]); err != nil {
		return err
	}
	if _, err := bb.Write(compressed); err != nil {
		return err
	}

	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc32.ChecksumIEEE(compressed))
	if _, err := bb.Write(crcBuf[:]); err != nil {
		return err
	}

	return nil
}

func (p *Page) DecodeFrom(r *bytes.Reader) error {
	var h PageHeader
	if err := h.Decode(r); err != nil {
		return err
	}

	compressedLen, err := binary.ReadUvarint(r)
	if err != nil {
		return err
	}

	compressed := make([]byte, compressedLen)
	if _, err := io.ReadFull(r, compressed); err != nil {
		return err
	}

	var crcBytes [4]byte
	if _, err := io.ReadFull(r, crcBytes[:]); err != nil {
		return err
	}
	return p.DecodeCompressedFrame(h, compressed, binary.LittleEndian.Uint32(crcBytes[:]))
}

func (p *Page) DecodeCompressedFrame(h PageHeader, compressed []byte, expectedCRC uint32) error {
	if actualCRC := crc32.ChecksumIEEE(compressed); actualCRC != expectedCRC {
		return fmt.Errorf("checksum mismatch: expected=%08x actual=%08x", expectedCRC, actualCRC)
	}

	decodedBuf := pageDecodeBufferPool.Get().(*[]byte)
	defer pageDecodeBufferPool.Put(decodedBuf)

	expectedPayloadLen := int(h.NumRecords) * (2 + 8 + 4)
	if cap(*decodedBuf) < expectedPayloadLen {
		*decodedBuf = make([]byte, 0, expectedPayloadLen)
	}

	payload, err := s2.Decode((*decodedBuf)[:0], compressed)
	if err != nil {
		return err
	}
	if len(payload) != expectedPayloadLen {
		return fmt.Errorf("decoded payload length mismatch: got=%d want=%d", len(payload), expectedPayloadLen)
	}

	n := int(h.NumRecords)
	p.Start = h.StartTime
	p.End = h.EndTime

	if cap(p.Metrics) >= n {
		p.Metrics = p.Metrics[:n]
	} else {
		p.Metrics = make([]MetricID, n)
	}
	if cap(p.Times) >= n {
		p.Times = p.Times[:n]
	} else {
		p.Times = make([]Timestamp, n)
	}

	if p.Values == nil {
		p.Values = bytes.NewBuffer(make([]byte, 0, PageMaxBytes))
	} else {
		p.Values.Reset()
	}

	pos := 0
	for i := range n {
		if pos+2 > len(payload) {
			return fmt.Errorf("payload too short for metric[%d]", i)
		}
		p.Metrics[i] = MetricID(binary.LittleEndian.Uint16(payload[pos:]))
		pos += 2
	}
	for i := range n {
		if pos+8 > len(payload) {
			return fmt.Errorf("payload too short for time[%d]", i)
		}
		p.Times[i] = Timestamp(binary.LittleEndian.Uint64(payload[pos:]))
		pos += 8
	}
	if _, err := p.Values.Write(payload[pos:]); err != nil {
		return err
	}

	return nil
}
