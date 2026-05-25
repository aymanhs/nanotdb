package engine

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	metricFileV2HeaderLen           = 64
	metricFileV2FrameHeaderLen      = 48
	metricFileV2TimeIndexEntryLen   = 40
	metricFileV2MetricIndexEntryLen = 44
	metricFileV2FooterLen           = 16

	metricFileV2Version = 2
)

var (
	metricFileV2TimeFrameMagic   = [4]byte{'T', 'P', 'G', '2'}
	metricFileV2MetricFrameMagic = [4]byte{'M', 'P', 'G', '2'}
)

const (
	metricFileV2TimeEncodingRawInt64 uint8 = 1
	metricFileV2TimeEncodingDeltaInt64
)

type metricFileV2Header struct {
	Flags             uint32
	PartitionKind     uint8
	FileMinTS         Timestamp
	FileMaxTS         Timestamp
	TimeFrameCount    uint32
	MetricFrameCount  uint32
	TimeIndexOffset   uint64
	MetricIndexOffset uint64
}

type metricTimeFrameHeaderV2 struct {
	CodecID      uint16
	TimeFrameID  uint16
	TimeEncoding uint8
	StartTS      Timestamp
	EndTS        Timestamp
	PointCount   uint32
	PayloadLen   uint32
	DecodedLen   uint32
}

type metricMetricFrameHeaderV2 struct {
	CodecID     uint16
	MetricID    MetricID
	ValueType   byte
	StartTS     Timestamp
	EndTS       Timestamp
	TimeFrameID uint16
	TimeOffset  uint32
	PayloadLen  uint32
	DecodedLen  uint32
}

type metricTimeFrameIndexEntryV2 struct {
	TimeFrameID uint16
	FrameOffset uint64
	StartTS     Timestamp
	EndTS       Timestamp
	PointCount  uint32
	DecodedLen  uint32
	PayloadLen  uint32
}

type metricMetricFrameIndexEntryV2 struct {
	MetricID    MetricID
	ValueType   byte
	FrameOffset uint64
	StartTS     Timestamp
	EndTS       Timestamp
	TimeFrameID uint16
	TimeOffset  uint32
	DecodedLen  uint32
	PayloadLen  uint32
}

type metricFileV2Footer struct{}

type metricTimeFramePlanV2 struct {
	TimeFrameID uint16
	Times       []Timestamp
	StartTS     Timestamp
	EndTS       Timestamp
	PointCount  uint32
	DecodedLen  uint32
	PayloadLen  uint32
	FrameOffset uint64
}

type metricMetricFramePlanV2 struct {
	MetricID    MetricID
	ValueType   byte
	TimeFrameID uint16
	TimeOffset  uint32
	Times       []Timestamp
	Int32       []int32
	Float32     []float32
	StartTS     Timestamp
	EndTS       Timestamp
	DecodedLen  uint32
	PayloadLen  uint32
	FrameOffset uint64
}

type metricFrameEncodeWorkspaceV2 struct {
	payloadRaw bytes.Buffer
	frame      bytes.Buffer
}

type metricTimeFrameCacheKeyV2 struct {
	Path        string
	FileSize    int64
	FileModUnix int64
	TimeFrameID uint16
}

type metricTimeFrameCacheEntryV2 struct {
	key   metricTimeFrameCacheKeyV2
	times []Timestamp
	bytes int64
	link  *list.Element
}

type metricTimeFrameCacheV2 struct {
	mu         sync.Mutex
	entries    map[metricTimeFrameCacheKeyV2]*metricTimeFrameCacheEntryV2
	order      *list.List
	bytes      int64
	maxEntries int
	hits       uint64
	misses     uint64
	evictions  uint64
}

const (
	metricTimeFrameCacheMaxEntriesV2 = 256
	metricTimeFrameCacheMaxBytesV2   = 32 << 20
)

var sharedMetricTimeFrameCacheV2 = metricTimeFrameCacheV2{
	entries:    make(map[metricTimeFrameCacheKeyV2]*metricTimeFrameCacheEntryV2),
	order:      list.New(),
	maxEntries: metricTimeFrameCacheMaxEntriesV2,
}

type metricTimeFrameCacheStatsV2 struct {
	Entries    int
	Bytes      int64
	MaxEntries int
	Hits       uint64
	Misses     uint64
	Evictions  uint64
}

func configureMetricTimeFrameCacheSlotsV2(maxEntries int) {
	sharedMetricTimeFrameCacheV2.configure(maxEntries)
}

func metricTimeFrameCacheStatsSnapshotV2() metricTimeFrameCacheStatsV2 {
	return sharedMetricTimeFrameCacheV2.snapshot()
}

func (h metricFileV2Header) EncodeBinary() [metricFileV2HeaderLen]byte {
	var out [metricFileV2HeaderLen]byte
	copy(out[0:4], metricFileMagic[:])
	binary.LittleEndian.PutUint16(out[4:6], metricFileV2Version)
	binary.LittleEndian.PutUint16(out[6:8], metricFileV2HeaderLen)
	binary.LittleEndian.PutUint32(out[8:12], h.Flags)
	out[12] = h.PartitionKind
	binary.LittleEndian.PutUint64(out[16:24], uint64(h.FileMinTS))
	binary.LittleEndian.PutUint64(out[24:32], uint64(h.FileMaxTS))
	binary.LittleEndian.PutUint32(out[32:36], h.TimeFrameCount)
	binary.LittleEndian.PutUint32(out[36:40], h.MetricFrameCount)
	binary.LittleEndian.PutUint64(out[40:48], h.TimeIndexOffset)
	binary.LittleEndian.PutUint64(out[48:56], h.MetricIndexOffset)
	binary.LittleEndian.PutUint32(out[60:64], crc32.ChecksumIEEE(out[:60]))
	return out
}

func decodeMetricFileV2Header(blob []byte) (metricFileV2Header, error) {
	if len(blob) != metricFileV2HeaderLen {
		return metricFileV2Header{}, fmt.Errorf("metric v2 header length mismatch: got=%d want=%d", len(blob), metricFileV2HeaderLen)
	}
	if string(blob[0:4]) != string(metricFileMagic[:]) {
		return metricFileV2Header{}, fmt.Errorf("metric v2 header magic mismatch")
	}
	if got := binary.LittleEndian.Uint16(blob[4:6]); got != metricFileV2Version {
		return metricFileV2Header{}, fmt.Errorf("metric v2 header version mismatch: got=%d want=%d", got, metricFileV2Version)
	}
	if got := binary.LittleEndian.Uint16(blob[6:8]); got != metricFileV2HeaderLen {
		return metricFileV2Header{}, fmt.Errorf("metric v2 header_len mismatch: got=%d want=%d", got, metricFileV2HeaderLen)
	}
	if got, want := binary.LittleEndian.Uint32(blob[60:64]), crc32.ChecksumIEEE(blob[:60]); got != want {
		return metricFileV2Header{}, fmt.Errorf("metric v2 header checksum mismatch: got=%08x want=%08x", got, want)
	}
	if binary.LittleEndian.Uint32(blob[56:60]) != 0 {
		return metricFileV2Header{}, fmt.Errorf("metric v2 header reserved field must be zero")
	}
	return metricFileV2Header{
		Flags:             binary.LittleEndian.Uint32(blob[8:12]),
		PartitionKind:     blob[12],
		FileMinTS:         Timestamp(binary.LittleEndian.Uint64(blob[16:24])),
		FileMaxTS:         Timestamp(binary.LittleEndian.Uint64(blob[24:32])),
		TimeFrameCount:    binary.LittleEndian.Uint32(blob[32:36]),
		MetricFrameCount:  binary.LittleEndian.Uint32(blob[36:40]),
		TimeIndexOffset:   binary.LittleEndian.Uint64(blob[40:48]),
		MetricIndexOffset: binary.LittleEndian.Uint64(blob[48:56]),
	}, nil
}

func (h metricTimeFrameHeaderV2) EncodeBinary() [metricFileV2FrameHeaderLen]byte {
	var out [metricFileV2FrameHeaderLen]byte
	copy(out[0:4], metricFileV2TimeFrameMagic[:])
	binary.LittleEndian.PutUint16(out[4:6], metricFileV2FrameHeaderLen)
	binary.LittleEndian.PutUint16(out[6:8], h.CodecID)
	binary.LittleEndian.PutUint16(out[8:10], h.TimeFrameID)
	out[10] = h.TimeEncoding
	binary.LittleEndian.PutUint64(out[12:20], uint64(h.StartTS))
	binary.LittleEndian.PutUint64(out[20:28], uint64(h.EndTS))
	binary.LittleEndian.PutUint32(out[28:32], h.PointCount)
	binary.LittleEndian.PutUint32(out[32:36], h.PayloadLen)
	binary.LittleEndian.PutUint32(out[36:40], h.DecodedLen)
	binary.LittleEndian.PutUint32(out[44:48], crc32.ChecksumIEEE(out[:44]))
	return out
}

func decodeMetricTimeFrameHeaderV2(blob []byte) (metricTimeFrameHeaderV2, error) {
	if len(blob) != metricFileV2FrameHeaderLen {
		return metricTimeFrameHeaderV2{}, fmt.Errorf("metric v2 time frame header length mismatch: got=%d want=%d", len(blob), metricFileV2FrameHeaderLen)
	}
	if string(blob[0:4]) != string(metricFileV2TimeFrameMagic[:]) {
		return metricTimeFrameHeaderV2{}, fmt.Errorf("metric v2 time frame magic mismatch")
	}
	if got := binary.LittleEndian.Uint16(blob[4:6]); got != metricFileV2FrameHeaderLen {
		return metricTimeFrameHeaderV2{}, fmt.Errorf("metric v2 time frame header_len mismatch: got=%d want=%d", got, metricFileV2FrameHeaderLen)
	}
	if binary.LittleEndian.Uint32(blob[40:44]) != 0 {
		return metricTimeFrameHeaderV2{}, fmt.Errorf("metric v2 time frame reserved field must be zero")
	}
	if got, want := binary.LittleEndian.Uint32(blob[44:48]), crc32.ChecksumIEEE(blob[:44]); got != want {
		return metricTimeFrameHeaderV2{}, fmt.Errorf("metric v2 time frame checksum mismatch: got=%08x want=%08x", got, want)
	}
	return metricTimeFrameHeaderV2{
		CodecID:      binary.LittleEndian.Uint16(blob[6:8]),
		TimeFrameID:  binary.LittleEndian.Uint16(blob[8:10]),
		TimeEncoding: blob[10],
		StartTS:      Timestamp(binary.LittleEndian.Uint64(blob[12:20])),
		EndTS:        Timestamp(binary.LittleEndian.Uint64(blob[20:28])),
		PointCount:   binary.LittleEndian.Uint32(blob[28:32]),
		PayloadLen:   binary.LittleEndian.Uint32(blob[32:36]),
		DecodedLen:   binary.LittleEndian.Uint32(blob[36:40]),
	}, nil
}

func (h metricMetricFrameHeaderV2) EncodeBinary() [metricFileV2FrameHeaderLen]byte {
	var out [metricFileV2FrameHeaderLen]byte
	copy(out[0:4], metricFileV2MetricFrameMagic[:])
	binary.LittleEndian.PutUint16(out[4:6], metricFileV2FrameHeaderLen)
	binary.LittleEndian.PutUint16(out[6:8], h.CodecID)
	binary.LittleEndian.PutUint16(out[8:10], uint16(h.MetricID))
	out[10] = h.ValueType
	binary.LittleEndian.PutUint64(out[12:20], uint64(h.StartTS))
	binary.LittleEndian.PutUint64(out[20:28], uint64(h.EndTS))
	binary.LittleEndian.PutUint16(out[28:30], h.TimeFrameID)
	binary.LittleEndian.PutUint32(out[32:36], h.TimeOffset)
	binary.LittleEndian.PutUint32(out[36:40], h.PayloadLen)
	binary.LittleEndian.PutUint32(out[40:44], h.DecodedLen)
	binary.LittleEndian.PutUint32(out[44:48], crc32.ChecksumIEEE(out[:44]))
	return out
}

func decodeMetricMetricFrameHeaderV2(blob []byte) (metricMetricFrameHeaderV2, error) {
	if len(blob) != metricFileV2FrameHeaderLen {
		return metricMetricFrameHeaderV2{}, fmt.Errorf("metric v2 metric frame header length mismatch: got=%d want=%d", len(blob), metricFileV2FrameHeaderLen)
	}
	if string(blob[0:4]) != string(metricFileV2MetricFrameMagic[:]) {
		return metricMetricFrameHeaderV2{}, fmt.Errorf("metric v2 metric frame magic mismatch")
	}
	if got := binary.LittleEndian.Uint16(blob[4:6]); got != metricFileV2FrameHeaderLen {
		return metricMetricFrameHeaderV2{}, fmt.Errorf("metric v2 metric frame header_len mismatch: got=%d want=%d", got, metricFileV2FrameHeaderLen)
	}
	if binary.LittleEndian.Uint16(blob[30:32]) != 0 {
		return metricMetricFrameHeaderV2{}, fmt.Errorf("metric v2 metric frame reserved field must be zero")
	}
	if got, want := binary.LittleEndian.Uint32(blob[44:48]), crc32.ChecksumIEEE(blob[:44]); got != want {
		return metricMetricFrameHeaderV2{}, fmt.Errorf("metric v2 metric frame checksum mismatch: got=%08x want=%08x", got, want)
	}
	return metricMetricFrameHeaderV2{
		CodecID:     binary.LittleEndian.Uint16(blob[6:8]),
		MetricID:    MetricID(binary.LittleEndian.Uint16(blob[8:10])),
		ValueType:   blob[10],
		StartTS:     Timestamp(binary.LittleEndian.Uint64(blob[12:20])),
		EndTS:       Timestamp(binary.LittleEndian.Uint64(blob[20:28])),
		TimeFrameID: binary.LittleEndian.Uint16(blob[28:30]),
		TimeOffset:  binary.LittleEndian.Uint32(blob[32:36]),
		PayloadLen:  binary.LittleEndian.Uint32(blob[36:40]),
		DecodedLen:  binary.LittleEndian.Uint32(blob[40:44]),
	}, nil
}

func (h metricMetricFrameHeaderV2) PointCount() (uint32, error) {
	return metricValuePointCountFromDecodedLen(h.ValueType, h.DecodedLen)
}

func metricValuePointCountFromDecodedLen(valueType byte, decodedLen uint32) (uint32, error) {
	width, err := metricValueWidthBytes(valueType)
	if err != nil {
		return 0, err
	}
	if decodedLen%width != 0 {
		return 0, fmt.Errorf("decoded length %d is not divisible by value width %d", decodedLen, width)
	}
	return decodedLen / width, nil
}

func metricValueWidthBytes(valueType byte) (uint32, error) {
	switch valueType {
	case Int32Sample, Float32Sample:
		return 4, nil
	default:
		return 0, fmt.Errorf("unsupported value type: %d", valueType)
	}
}

func (e metricTimeFrameIndexEntryV2) EncodeBinary() [metricFileV2TimeIndexEntryLen]byte {
	var out [metricFileV2TimeIndexEntryLen]byte
	binary.LittleEndian.PutUint16(out[0:2], e.TimeFrameID)
	binary.LittleEndian.PutUint64(out[4:12], e.FrameOffset)
	binary.LittleEndian.PutUint64(out[12:20], uint64(e.StartTS))
	binary.LittleEndian.PutUint64(out[20:28], uint64(e.EndTS))
	binary.LittleEndian.PutUint32(out[28:32], e.PointCount)
	binary.LittleEndian.PutUint32(out[32:36], e.DecodedLen)
	binary.LittleEndian.PutUint32(out[36:40], e.PayloadLen)
	return out
}

func decodeMetricTimeFrameIndexEntryV2(blob []byte) (metricTimeFrameIndexEntryV2, error) {
	if len(blob) != metricFileV2TimeIndexEntryLen {
		return metricTimeFrameIndexEntryV2{}, fmt.Errorf("metric v2 time index entry length mismatch: got=%d want=%d", len(blob), metricFileV2TimeIndexEntryLen)
	}
	if blob[2] != 0 || blob[3] != 0 {
		return metricTimeFrameIndexEntryV2{}, fmt.Errorf("metric v2 time index reserved bytes must be zero")
	}
	return metricTimeFrameIndexEntryV2{
		TimeFrameID: binary.LittleEndian.Uint16(blob[0:2]),
		FrameOffset: binary.LittleEndian.Uint64(blob[4:12]),
		StartTS:     Timestamp(binary.LittleEndian.Uint64(blob[12:20])),
		EndTS:       Timestamp(binary.LittleEndian.Uint64(blob[20:28])),
		PointCount:  binary.LittleEndian.Uint32(blob[28:32]),
		DecodedLen:  binary.LittleEndian.Uint32(blob[32:36]),
		PayloadLen:  binary.LittleEndian.Uint32(blob[36:40]),
	}, nil
}

func (e metricMetricFrameIndexEntryV2) EncodeBinary() [metricFileV2MetricIndexEntryLen]byte {
	var out [metricFileV2MetricIndexEntryLen]byte
	binary.LittleEndian.PutUint16(out[0:2], uint16(e.MetricID))
	out[2] = e.ValueType
	binary.LittleEndian.PutUint64(out[4:12], e.FrameOffset)
	binary.LittleEndian.PutUint64(out[12:20], uint64(e.StartTS))
	binary.LittleEndian.PutUint64(out[20:28], uint64(e.EndTS))
	binary.LittleEndian.PutUint16(out[28:30], e.TimeFrameID)
	binary.LittleEndian.PutUint32(out[32:36], e.TimeOffset)
	binary.LittleEndian.PutUint32(out[36:40], e.DecodedLen)
	binary.LittleEndian.PutUint32(out[40:44], e.PayloadLen)
	return out
}

func decodeMetricMetricFrameIndexEntryV2(blob []byte) (metricMetricFrameIndexEntryV2, error) {
	if len(blob) != metricFileV2MetricIndexEntryLen {
		return metricMetricFrameIndexEntryV2{}, fmt.Errorf("metric v2 metric index entry length mismatch: got=%d want=%d", len(blob), metricFileV2MetricIndexEntryLen)
	}
	if blob[3] != 0 || binary.LittleEndian.Uint16(blob[30:32]) != 0 {
		return metricMetricFrameIndexEntryV2{}, fmt.Errorf("metric v2 metric index reserved bytes must be zero")
	}
	return metricMetricFrameIndexEntryV2{
		MetricID:    MetricID(binary.LittleEndian.Uint16(blob[0:2])),
		ValueType:   blob[2],
		FrameOffset: binary.LittleEndian.Uint64(blob[4:12]),
		StartTS:     Timestamp(binary.LittleEndian.Uint64(blob[12:20])),
		EndTS:       Timestamp(binary.LittleEndian.Uint64(blob[20:28])),
		TimeFrameID: binary.LittleEndian.Uint16(blob[28:30]),
		TimeOffset:  binary.LittleEndian.Uint32(blob[32:36]),
		DecodedLen:  binary.LittleEndian.Uint32(blob[36:40]),
		PayloadLen:  binary.LittleEndian.Uint32(blob[40:44]),
	}, nil
}

func (f metricFileV2Footer) EncodeBinary() [metricFileV2FooterLen]byte {
	var out [metricFileV2FooterLen]byte
	copy(out[0:4], metricFileFooterMagic[:])
	binary.LittleEndian.PutUint32(out[4:8], metricFileV2Version)
	binary.LittleEndian.PutUint32(out[12:16], crc32.ChecksumIEEE(out[:12]))
	return out
}

func decodeMetricFileV2Footer(blob []byte) (metricFileV2Footer, error) {
	if len(blob) != metricFileV2FooterLen {
		return metricFileV2Footer{}, fmt.Errorf("metric v2 footer length mismatch: got=%d want=%d", len(blob), metricFileV2FooterLen)
	}
	if string(blob[0:4]) != string(metricFileFooterMagic[:]) {
		return metricFileV2Footer{}, fmt.Errorf("metric v2 footer magic mismatch")
	}
	if got := binary.LittleEndian.Uint32(blob[4:8]); got != metricFileV2Version {
		return metricFileV2Footer{}, fmt.Errorf("metric v2 footer version mismatch: got=%d want=%d", got, metricFileV2Version)
	}
	if binary.LittleEndian.Uint32(blob[8:12]) != 0 {
		return metricFileV2Footer{}, fmt.Errorf("metric v2 footer reserved field must be zero")
	}
	if got, want := binary.LittleEndian.Uint32(blob[12:16]), crc32.ChecksumIEEE(blob[:12]); got != want {
		return metricFileV2Footer{}, fmt.Errorf("metric v2 footer checksum mismatch: got=%08x want=%08x", got, want)
	}
	return metricFileV2Footer{}, nil
}

func WriteMetricFileV2(path string, partitionKind uint8, codec BlockCompressionCodec, pages []MetricFilePageInput) error {
	if partitionKind < MetricPartitionDay || partitionKind > MetricPartitionForever {
		return fmt.Errorf("invalid partition kind: %d", partitionKind)
	}
	if len(pages) == 0 {
		return fmt.Errorf("no pages provided")
	}
	if codec == nil {
		codec = DefaultMetricFileCompressionCodec()
	}

	timeFrames, metricFrames, err := planMetricFramesV2(pages)
	if err != nil {
		return err
	}
	if len(timeFrames) == 0 || len(metricFrames) == 0 {
		return fmt.Errorf("metric v2 planning produced no frames")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, tmpPath, err := createAtomicTmp(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}()

	header := metricFileV2Header{Flags: 1, PartitionKind: partitionKind}.EncodeBinary()
	if _, err := f.Write(header[:]); err != nil {
		return err
	}

	workspace := metricFrameEncodeWorkspaceV2{}
	curOffset := uint64(metricFileV2HeaderLen)
	timeInfos := make([]metricTimeFrameIndexEntryV2, 0, len(timeFrames))
	metricInfos := make([]metricMetricFrameIndexEntryV2, 0, len(metricFrames))
	var fileMin, fileMax Timestamp
	firstTS := true

	for i := range timeFrames {
		frame, hdr, err := encodeMetricTimeFrameV2(&workspace, codec, &timeFrames[i], curOffset)
		if err != nil {
			return err
		}
		if _, err := f.Write(frame); err != nil {
			return err
		}
		timeInfos = append(timeInfos, metricTimeFrameIndexEntryV2{
			TimeFrameID: hdr.TimeFrameID,
			FrameOffset: curOffset,
			StartTS:     hdr.StartTS,
			EndTS:       hdr.EndTS,
			PointCount:  hdr.PointCount,
			DecodedLen:  hdr.DecodedLen,
			PayloadLen:  hdr.PayloadLen,
		})
		timeFrames[i].FrameOffset = curOffset
		timeFrames[i].DecodedLen = hdr.DecodedLen
		timeFrames[i].PayloadLen = hdr.PayloadLen
		curOffset += uint64(len(frame))
		if firstTS {
			fileMin, fileMax = hdr.StartTS, hdr.EndTS
			firstTS = false
		} else {
			if hdr.StartTS < fileMin {
				fileMin = hdr.StartTS
			}
			if hdr.EndTS > fileMax {
				fileMax = hdr.EndTS
			}
		}
	}

	for i := range metricFrames {
		frame, hdr, err := encodeMetricMetricFrameV2(&workspace, codec, &metricFrames[i], curOffset)
		if err != nil {
			return err
		}
		if _, err := f.Write(frame); err != nil {
			return err
		}
		metricInfos = append(metricInfos, metricMetricFrameIndexEntryV2{
			MetricID:    hdr.MetricID,
			ValueType:   hdr.ValueType,
			FrameOffset: curOffset,
			StartTS:     hdr.StartTS,
			EndTS:       hdr.EndTS,
			TimeFrameID: hdr.TimeFrameID,
			TimeOffset:  hdr.TimeOffset,
			DecodedLen:  hdr.DecodedLen,
			PayloadLen:  hdr.PayloadLen,
		})
		metricFrames[i].FrameOffset = curOffset
		metricFrames[i].DecodedLen = hdr.DecodedLen
		metricFrames[i].PayloadLen = hdr.PayloadLen
		curOffset += uint64(len(frame))
		if firstTS {
			fileMin, fileMax = hdr.StartTS, hdr.EndTS
			firstTS = false
		} else {
			if hdr.StartTS < fileMin {
				fileMin = hdr.StartTS
			}
			if hdr.EndTS > fileMax {
				fileMax = hdr.EndTS
			}
		}
	}

	timeIndexOffset := curOffset
	for _, info := range timeInfos {
		entry := info.EncodeBinary()
		if _, err := f.Write(entry[:]); err != nil {
			return err
		}
		curOffset += metricFileV2TimeIndexEntryLen
	}
	metricIndexOffset := curOffset
	for _, info := range metricInfos {
		entry := info.EncodeBinary()
		if _, err := f.Write(entry[:]); err != nil {
			return err
		}
		curOffset += metricFileV2MetricIndexEntryLen
	}
	footer := metricFileV2Footer{}.EncodeBinary()
	if _, err := f.Write(footer[:]); err != nil {
		return err
	}

	finalHeader := metricFileV2Header{
		Flags:             1,
		PartitionKind:     partitionKind,
		FileMinTS:         fileMin,
		FileMaxTS:         fileMax,
		TimeFrameCount:    uint32(len(timeInfos)),
		MetricFrameCount:  uint32(len(metricInfos)),
		TimeIndexOffset:   timeIndexOffset,
		MetricIndexOffset: metricIndexOffset,
	}.EncodeBinary()
	if _, err := f.WriteAt(finalHeader[:], 0); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return nil
}

func planMetricFramesV2(pages []MetricFilePageInput) ([]metricTimeFramePlanV2, []metricMetricFramePlanV2, error) {
	indexed := make([]struct {
		idx  int
		page MetricFilePageInput
	}, len(pages))
	for i := range pages {
		indexed[i] = struct {
			idx  int
			page MetricFilePageInput
		}{idx: i, page: pages[i]}
	}
	sort.SliceStable(indexed, func(i, j int) bool {
		if indexed[i].page.MetricID == indexed[j].page.MetricID {
			return indexed[i].idx < indexed[j].idx
		}
		return indexed[i].page.MetricID < indexed[j].page.MetricID
	})

	type assignment struct {
		timeFrameID uint16
		timeOffset  uint32
	}
	assignments := make([]assignment, len(indexed))
	timeFrames := make([]metricTimeFramePlanV2, 0, len(indexed))
	bySize := make([]int, len(indexed))
	for i := range indexed {
		bySize[i] = i
	}
	sort.SliceStable(bySize, func(i, j int) bool {
		li := len(indexed[bySize[i]].page.Times)
		lj := len(indexed[bySize[j]].page.Times)
		if li == lj {
			return indexed[bySize[i]].page.MetricID < indexed[bySize[j]].page.MetricID
		}
		return li > lj
	})

	for _, orderIdx := range bySize {
		page := indexed[orderIdx].page
		if len(page.Times) == 0 {
			return nil, nil, fmt.Errorf("metric %d has no timestamps", page.MetricID)
		}
		assigned := false
		for i := range timeFrames {
			offset, ok := findTimeSliceOffset(timeFrames[i].Times, page.Times)
			if !ok {
				continue
			}
			assignments[orderIdx] = assignment{timeFrameID: timeFrames[i].TimeFrameID, timeOffset: uint32(offset)}
			assigned = true
			break
		}
		if assigned {
			continue
		}
		decodedLen := uint32(len(page.Times) * 8)
		timeFrameID := uint16(len(timeFrames) + 1)
		timeFrames = append(timeFrames, metricTimeFramePlanV2{
			TimeFrameID: timeFrameID,
			Times:       append([]Timestamp(nil), page.Times...),
			StartTS:     page.Times[0],
			EndTS:       page.Times[len(page.Times)-1],
			PointCount:  uint32(len(page.Times)),
			DecodedLen:  decodedLen,
		})
		assignments[orderIdx] = assignment{timeFrameID: timeFrameID, timeOffset: 0}
	}

	metricFrames := make([]metricMetricFramePlanV2, 0, len(indexed))
	for i, item := range indexed {
		page := item.page
		decodedLen, err := metricDecodedValueLen(page)
		if err != nil {
			return nil, nil, err
		}
		metricFrames = append(metricFrames, metricMetricFramePlanV2{
			MetricID:    page.MetricID,
			ValueType:   page.ValueType,
			TimeFrameID: assignments[i].timeFrameID,
			TimeOffset:  assignments[i].timeOffset,
			Times:       append([]Timestamp(nil), page.Times...),
			Int32:       append([]int32(nil), page.Int32...),
			Float32:     append([]float32(nil), page.Float32...),
			StartTS:     page.Times[0],
			EndTS:       page.Times[len(page.Times)-1],
			DecodedLen:  decodedLen,
		})
	}
	return timeFrames, metricFrames, nil
}

func findTimeSliceOffset(haystack []Timestamp, needle []Timestamp) (int, bool) {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return 0, false
	}
	last := len(haystack) - len(needle)
	for start := 0; start <= last; start++ {
		match := true
		for i := range needle {
			if haystack[start+i] != needle[i] {
				match = false
				break
			}
		}
		if match {
			return start, true
		}
	}
	return 0, false
}

func metricDecodedValueLen(page MetricFilePageInput) (uint32, error) {
	width, err := metricValueWidthBytes(page.ValueType)
	if err != nil {
		return 0, err
	}
	count := len(page.Times)
	switch page.ValueType {
	case Int32Sample:
		if len(page.Int32) != count || len(page.Float32) != 0 {
			return 0, fmt.Errorf("invalid int32 payload for metric %d", page.MetricID)
		}
	case Float32Sample:
		if len(page.Float32) != count || len(page.Int32) != 0 {
			return 0, fmt.Errorf("invalid float32 payload for metric %d", page.MetricID)
		}
	}
	return uint32(count) * width, nil
}

func encodeMetricTimePayloadV2(times []Timestamp) []byte {
	var payload bytes.Buffer
	payload.Grow(len(times) * 8)
	for _, ts := range times {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(ts))
		_, _ = payload.Write(b[:])
	}
	return payload.Bytes()
}

func encodeMetricValuePayloadV2(page metricMetricFramePlanV2) ([]byte, error) {
	var payload bytes.Buffer
	payload.Grow(int(page.DecodedLen))
	switch page.ValueType {
	case Int32Sample:
		if len(page.Int32) != len(page.Times) || len(page.Float32) != 0 {
			return nil, fmt.Errorf("invalid int32 payload for metric %d", page.MetricID)
		}
		for _, value := range page.Int32 {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], uint32(value))
			if _, err := payload.Write(b[:]); err != nil {
				return nil, err
			}
		}
	case Float32Sample:
		if len(page.Float32) != len(page.Times) || len(page.Int32) != 0 {
			return nil, fmt.Errorf("invalid float32 payload for metric %d", page.MetricID)
		}
		for _, value := range page.Float32 {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(value))
			if _, err := payload.Write(b[:]); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unsupported value type: %d", page.ValueType)
	}
	return payload.Bytes(), nil
}

func encodeMetricTimeFrameV2(workspace *metricFrameEncodeWorkspaceV2, codec BlockCompressionCodec, plan *metricTimeFramePlanV2, pageOffset uint64) ([]byte, metricTimeFrameHeaderV2, error) {
	if workspace == nil {
		workspace = &metricFrameEncodeWorkspaceV2{}
	}
	if codec == nil {
		codec = DefaultMetricFileCompressionCodec()
	}
	if len(plan.Times) == 0 {
		return nil, metricTimeFrameHeaderV2{}, fmt.Errorf("time frame %d has no timestamps", plan.TimeFrameID)
	}
	payloadRaw := encodeMetricTimePayloadV2(plan.Times)
	compressed, err := codec.Encode(payloadRaw)
	if err != nil {
		return nil, metricTimeFrameHeaderV2{}, err
	}
	hdr := metricTimeFrameHeaderV2{
		CodecID:      codec.ID(),
		TimeFrameID:  plan.TimeFrameID,
		TimeEncoding: metricFileV2TimeEncodingRawInt64,
		StartTS:      plan.StartTS,
		EndTS:        plan.EndTS,
		PointCount:   uint32(len(plan.Times)),
		PayloadLen:   uint32(len(compressed)),
		DecodedLen:   uint32(len(payloadRaw)),
	}
	frame := &workspace.frame
	frame.Reset()
	encodedHdr := hdr.EncodeBinary()
	frame.Grow(len(encodedHdr) + len(compressed) + 4)
	if _, err := frame.Write(encodedHdr[:]); err != nil {
		return nil, metricTimeFrameHeaderV2{}, err
	}
	if _, err := frame.Write(compressed); err != nil {
		return nil, metricTimeFrameHeaderV2{}, err
	}
	var crcTail [4]byte
	binary.LittleEndian.PutUint32(crcTail[:], crc32.ChecksumIEEE(compressed))
	if _, err := frame.Write(crcTail[:]); err != nil {
		return nil, metricTimeFrameHeaderV2{}, err
	}
	_ = pageOffset
	return frame.Bytes(), hdr, nil
}

func encodeMetricMetricFrameV2(workspace *metricFrameEncodeWorkspaceV2, codec BlockCompressionCodec, plan *metricMetricFramePlanV2, pageOffset uint64) ([]byte, metricMetricFrameHeaderV2, error) {
	if workspace == nil {
		workspace = &metricFrameEncodeWorkspaceV2{}
	}
	if codec == nil {
		codec = DefaultMetricFileCompressionCodec()
	}
	payloadRaw, err := encodeMetricValuePayloadV2(*plan)
	if err != nil {
		return nil, metricMetricFrameHeaderV2{}, err
	}
	compressed, err := codec.Encode(payloadRaw)
	if err != nil {
		return nil, metricMetricFrameHeaderV2{}, err
	}
	hdr := metricMetricFrameHeaderV2{
		CodecID:     codec.ID(),
		MetricID:    plan.MetricID,
		ValueType:   plan.ValueType,
		StartTS:     plan.StartTS,
		EndTS:       plan.EndTS,
		TimeFrameID: plan.TimeFrameID,
		TimeOffset:  plan.TimeOffset,
		PayloadLen:  uint32(len(compressed)),
		DecodedLen:  uint32(len(payloadRaw)),
	}
	frame := &workspace.frame
	frame.Reset()
	encodedHdr := hdr.EncodeBinary()
	frame.Grow(len(encodedHdr) + len(compressed) + 4)
	if _, err := frame.Write(encodedHdr[:]); err != nil {
		return nil, metricMetricFrameHeaderV2{}, err
	}
	if _, err := frame.Write(compressed); err != nil {
		return nil, metricMetricFrameHeaderV2{}, err
	}
	var crcTail [4]byte
	binary.LittleEndian.PutUint32(crcTail[:], crc32.ChecksumIEEE(compressed))
	if _, err := frame.Write(crcTail[:]); err != nil {
		return nil, metricMetricFrameHeaderV2{}, err
	}
	_ = pageOffset
	return frame.Bytes(), hdr, nil
}

func readMetricFileV2Header(path string) (metricFileV2Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return metricFileV2Header{}, err
	}
	defer f.Close()
	return readMetricFileV2HeaderFromFile(f)
}

func readMetricFileV2HeaderFromFile(f *os.File) (metricFileV2Header, error) {
	var hdr [metricFileV2HeaderLen]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return metricFileV2Header{}, err
	}
	return decodeMetricFileV2Header(hdr[:])
}

func readMetricFileV2Footer(path string) (metricFileV2Footer, error) {
	f, err := os.Open(path)
	if err != nil {
		return metricFileV2Footer{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return metricFileV2Footer{}, err
	}
	if st.Size() < metricFileV2FooterLen {
		return metricFileV2Footer{}, fmt.Errorf("metric v2 file too small")
	}
	return readMetricFileV2FooterFromFile(f, st.Size())
}

func readMetricFileV2FooterFromFile(f *os.File, fileSize int64) (metricFileV2Footer, error) {
	var footer [metricFileV2FooterLen]byte
	if _, err := f.ReadAt(footer[:], fileSize-metricFileV2FooterLen); err != nil {
		return metricFileV2Footer{}, err
	}
	return decodeMetricFileV2Footer(footer[:])
}

func readMetricTimeFrameIndexEntriesV2(path string) ([]metricTimeFrameIndexEntryV2, error) {
	hdr, err := readMetricFileV2Header(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readMetricTimeFrameIndexEntriesV2FromFile(f, hdr)
}

func readMetricTimeFrameIndexEntriesV2FromFile(f *os.File, hdr metricFileV2Header) ([]metricTimeFrameIndexEntryV2, error) {
	entries := make([]metricTimeFrameIndexEntryV2, 0, hdr.TimeFrameCount)
	buf := make([]byte, metricFileV2TimeIndexEntryLen)
	offset := int64(hdr.TimeIndexOffset)
	for i := uint32(0); i < hdr.TimeFrameCount; i++ {
		if _, err := f.ReadAt(buf, offset); err != nil {
			return nil, err
		}
		entry, err := decodeMetricTimeFrameIndexEntryV2(buf)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		offset += metricFileV2TimeIndexEntryLen
	}
	return entries, nil
}

func readMetricMetricFrameIndexEntriesV2(path string) ([]metricMetricFrameIndexEntryV2, error) {
	hdr, err := readMetricFileV2Header(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readMetricMetricFrameIndexEntriesV2FromFile(f, hdr)
}

func readMetricMetricFrameIndexEntriesV2FromFile(f *os.File, hdr metricFileV2Header) ([]metricMetricFrameIndexEntryV2, error) {
	entries := make([]metricMetricFrameIndexEntryV2, 0, hdr.MetricFrameCount)
	buf := make([]byte, metricFileV2MetricIndexEntryLen)
	offset := int64(hdr.MetricIndexOffset)
	for i := uint32(0); i < hdr.MetricFrameCount; i++ {
		if _, err := f.ReadAt(buf, offset); err != nil {
			return nil, err
		}
		entry, err := decodeMetricMetricFrameIndexEntryV2(buf)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		offset += metricFileV2MetricIndexEntryLen
	}
	return entries, nil
}

func metricTimeFrameCacheIdentityV2(path string, st os.FileInfo) metricTimeFrameCacheKeyV2 {
	return metricTimeFrameCacheKeyV2{
		Path:        path,
		FileSize:    st.Size(),
		FileModUnix: st.ModTime().UnixNano(),
	}
}

func resolveMetricTimeFrameV2(f *os.File, identity metricTimeFrameCacheKeyV2, local map[uint16][]Timestamp, info metricTimeFrameIndexEntryV2) ([]Timestamp, error) {
	if times, ok := local[info.TimeFrameID]; ok {
		return times, nil
	}
	key := identity
	key.TimeFrameID = info.TimeFrameID
	if times, ok := sharedMetricTimeFrameCacheV2.get(key); ok {
		local[info.TimeFrameID] = times
		return times, nil
	}
	times, err := readOneMetricTimeFrameV2(f, info)
	if err != nil {
		return nil, err
	}
	sharedMetricTimeFrameCacheV2.put(key, times)
	local[info.TimeFrameID] = times
	return times, nil
}

func (c *metricTimeFrameCacheV2) get(key metricTimeFrameCacheKeyV2) ([]Timestamp, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		c.misses++
		return nil, false
	}
	c.hits++
	c.order.MoveToFront(entry.link)
	return entry.times, true
}

func (c *metricTimeFrameCacheV2) configure(maxEntries int) {
	if maxEntries <= 0 {
		maxEntries = metricTimeFrameCacheMaxEntriesV2
	}
	c.mu.Lock()
	c.maxEntries = maxEntries
	c.evictLocked()
	c.mu.Unlock()
}

func (c *metricTimeFrameCacheV2) snapshot() metricTimeFrameCacheStatsV2 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return metricTimeFrameCacheStatsV2{
		Entries:    len(c.entries),
		Bytes:      c.bytes,
		MaxEntries: c.maxEntries,
		Hits:       c.hits,
		Misses:     c.misses,
		Evictions:  c.evictions,
	}
}

func (c *metricTimeFrameCacheV2) put(key metricTimeFrameCacheKeyV2, times []Timestamp) {
	clone := append([]Timestamp(nil), times...)
	bytesUsed := int64(len(clone) * 8)
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[key]; ok {
		c.bytes -= existing.bytes
		existing.times = clone
		existing.bytes = bytesUsed
		c.bytes += bytesUsed
		c.order.MoveToFront(existing.link)
		c.evictLocked()
		return
	}
	entry := &metricTimeFrameCacheEntryV2{key: key, times: clone, bytes: bytesUsed}
	entry.link = c.order.PushFront(entry)
	c.entries[key] = entry
	c.bytes += bytesUsed
	c.evictLocked()
}

func (c *metricTimeFrameCacheV2) evictLocked() {
	maxEntries := c.maxEntries
	if maxEntries <= 0 {
		maxEntries = metricTimeFrameCacheMaxEntriesV2
	}
	for len(c.entries) > maxEntries || c.bytes > metricTimeFrameCacheMaxBytesV2 {
		back := c.order.Back()
		if back == nil {
			return
		}
		entry := back.Value.(*metricTimeFrameCacheEntryV2)
		delete(c.entries, entry.key)
		c.bytes -= entry.bytes
		c.order.Remove(back)
		c.evictions++
	}
}

func readOneMetricTimeFrameV2(f *os.File, info metricTimeFrameIndexEntryV2) ([]Timestamp, error) {
	hdrBuf := make([]byte, metricFileV2FrameHeaderLen)
	if _, err := f.ReadAt(hdrBuf, int64(info.FrameOffset)); err != nil {
		return nil, err
	}
	hdr, err := decodeMetricTimeFrameHeaderV2(hdrBuf)
	if err != nil {
		return nil, err
	}
	if hdr.TimeFrameID != info.TimeFrameID || hdr.PointCount != info.PointCount || hdr.PayloadLen != info.PayloadLen || hdr.DecodedLen != info.DecodedLen {
		return nil, fmt.Errorf("time frame index/header mismatch")
	}
	if hdr.StartTS != info.StartTS || hdr.EndTS != info.EndTS {
		return nil, fmt.Errorf("time frame timestamp bounds mismatch")
	}
	codec, err := BlockCompressionCodecByID(hdr.CodecID)
	if err != nil {
		return nil, err
	}
	payloadOffset := int64(info.FrameOffset) + metricFileV2FrameHeaderLen
	compressed := make([]byte, hdr.PayloadLen)
	if _, err := f.ReadAt(compressed, payloadOffset); err != nil {
		return nil, err
	}
	var crcBuf [4]byte
	if _, err := f.ReadAt(crcBuf[:], payloadOffset+int64(hdr.PayloadLen)); err != nil {
		return nil, err
	}
	if got := binary.LittleEndian.Uint32(crcBuf[:]); got != crc32.ChecksumIEEE(compressed) {
		return nil, fmt.Errorf("time frame payload crc mismatch")
	}
	decoded, err := codec.Decode(compressed)
	if err != nil {
		return nil, err
	}
	if len(decoded) != int(hdr.DecodedLen) {
		return nil, fmt.Errorf("time frame decoded length mismatch")
	}
	if hdr.TimeEncoding != metricFileV2TimeEncodingRawInt64 {
		return nil, fmt.Errorf("unsupported time encoding: %d", hdr.TimeEncoding)
	}
	if len(decoded) != int(hdr.PointCount)*8 {
		return nil, fmt.Errorf("time frame decoded payload shape mismatch")
	}
	times := make([]Timestamp, hdr.PointCount)
	for i := range times {
		times[i] = Timestamp(binary.LittleEndian.Uint64(decoded[i*8 : i*8+8]))
	}
	if len(times) > 0 && (times[0] != hdr.StartTS || times[len(times)-1] != hdr.EndTS) {
		return nil, fmt.Errorf("time frame header bounds mismatch")
	}
	return times, nil
}

type metricMetricFrameV2 struct {
	MetricID    MetricID
	ValueType   byte
	TimeFrameID uint16
	TimeOffset  uint32
	StartTS     Timestamp
	EndTS       Timestamp
	DecodedLen  uint32
	PayloadLen  uint32
	Int32       []int32
	Float32     []float32
}

func readOneMetricMetricFrameV2(f *os.File, fileSize int64, info metricMetricFrameIndexEntryV2) (metricMetricFrameV2, error) {
	hdrBuf := make([]byte, metricFileV2FrameHeaderLen)
	if _, err := f.ReadAt(hdrBuf, int64(info.FrameOffset)); err != nil {
		return metricMetricFrameV2{}, err
	}
	hdr, err := decodeMetricMetricFrameHeaderV2(hdrBuf)
	if err != nil {
		return metricMetricFrameV2{}, err
	}
	if hdr.MetricID != info.MetricID || hdr.ValueType != info.ValueType || hdr.TimeFrameID != info.TimeFrameID || hdr.TimeOffset != info.TimeOffset || hdr.PayloadLen != info.PayloadLen || hdr.DecodedLen != info.DecodedLen {
		return metricMetricFrameV2{}, fmt.Errorf("metric frame index/header mismatch")
	}
	if hdr.StartTS != info.StartTS || hdr.EndTS != info.EndTS {
		return metricMetricFrameV2{}, fmt.Errorf("metric frame timestamp bounds mismatch")
	}
	codec, err := BlockCompressionCodecByID(hdr.CodecID)
	if err != nil {
		return metricMetricFrameV2{}, err
	}
	payloadOffset := int64(info.FrameOffset) + metricFileV2FrameHeaderLen
	payloadEnd := payloadOffset + int64(hdr.PayloadLen) + 4
	if payloadEnd > fileSize {
		return metricMetricFrameV2{}, fmt.Errorf("metric frame payload out of bounds")
	}
	compressed := make([]byte, hdr.PayloadLen)
	if _, err := f.ReadAt(compressed, payloadOffset); err != nil {
		return metricMetricFrameV2{}, err
	}
	var crcBuf [4]byte
	if _, err := f.ReadAt(crcBuf[:], payloadOffset+int64(hdr.PayloadLen)); err != nil {
		return metricMetricFrameV2{}, err
	}
	if got := binary.LittleEndian.Uint32(crcBuf[:]); got != crc32.ChecksumIEEE(compressed) {
		return metricMetricFrameV2{}, fmt.Errorf("metric frame payload crc mismatch")
	}
	decoded, err := codec.Decode(compressed)
	if err != nil {
		return metricMetricFrameV2{}, err
	}
	if len(decoded) != int(hdr.DecodedLen) {
		return metricMetricFrameV2{}, fmt.Errorf("metric frame decoded length mismatch")
	}
	pointCount, err := hdr.PointCount()
	if err != nil {
		return metricMetricFrameV2{}, err
	}
	out := metricMetricFrameV2{MetricID: hdr.MetricID, ValueType: hdr.ValueType, TimeFrameID: hdr.TimeFrameID, TimeOffset: hdr.TimeOffset, StartTS: hdr.StartTS, EndTS: hdr.EndTS, DecodedLen: hdr.DecodedLen, PayloadLen: hdr.PayloadLen}
	switch hdr.ValueType {
	case Int32Sample:
		out.Int32 = make([]int32, pointCount)
		for i := 0; i < int(pointCount); i++ {
			out.Int32[i] = int32(binary.LittleEndian.Uint32(decoded[i*4 : i*4+4]))
		}
	case Float32Sample:
		out.Float32 = make([]float32, pointCount)
		for i := 0; i < int(pointCount); i++ {
			out.Float32[i] = math.Float32frombits(binary.LittleEndian.Uint32(decoded[i*4 : i*4+4]))
		}
	default:
		return metricMetricFrameV2{}, fmt.Errorf("unsupported value type: %d", hdr.ValueType)
	}
	return out, nil
}

func collectMetricFromMetricFrameV2(database, metric string, entry MetricEntry, frame metricMetricFrameV2, times []Timestamp, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	if frame.ValueType != entry.ValueType {
		return fmt.Errorf("metric value type mismatch")
	}
	pointCount, err := metricValuePointCountFromDecodedLen(frame.ValueType, frame.DecodedLen)
	if err != nil {
		return err
	}
	if len(times) != int(pointCount) {
		return fmt.Errorf("metric frame time/value length mismatch")
	}
	for i, ts := range times {
		if ts < fromTS || ts > toTS {
			continue
		}
		if *count%stride != 0 {
			*count++
			continue
		}
		*count++
		s := Sample{Database: database, Metric: metric, TS: ts, ValueType: entry.ValueType}
		if entry.ValueType == Int32Sample {
			s.Int32 = frame.Int32[i]
		} else {
			s.Float32 = frame.Float32[i]
		}
		if err := fn(s); err != nil {
			return err
		}
	}
	return nil
}

func readMetricFrameVersion(path string) (uint16, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var header [8]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return 0, err
	}
	if !bytes.Equal(header[0:4], metricFileMagic[:]) {
		return 0, fmt.Errorf("invalid file magic")
	}
	return binary.LittleEndian.Uint16(header[4:6]), nil
}

func collectMetricFromMetricFileV2(database, metric string, entry MetricEntry, path string, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() < metricFileV2HeaderLen+metricFileV2FooterLen {
		return fmt.Errorf("file too small")
	}
	if _, err := readMetricFileV2FooterFromFile(f, st.Size()); err != nil {
		return err
	}
	hdr, err := readMetricFileV2HeaderFromFile(f)
	if err != nil {
		return err
	}
	timeEntries, err := readMetricTimeFrameIndexEntriesV2FromFile(f, hdr)
	if err != nil {
		return err
	}
	metricEntries, err := readMetricMetricFrameIndexEntriesV2FromFile(f, hdr)
	if err != nil {
		return err
	}
	timeByID := make(map[uint16]metricTimeFrameIndexEntryV2, len(timeEntries))
	for _, entry := range timeEntries {
		timeByID[entry.TimeFrameID] = entry
	}
	localTimeCache := make(map[uint16][]Timestamp, len(timeEntries))
	cacheIdentity := metricTimeFrameCacheIdentityV2(path, st)

	for _, info := range metricEntries {
		if info.MetricID != entry.MetricID {
			continue
		}
		if info.EndTS < fromTS || info.StartTS > toTS {
			continue
		}
		timeInfo, ok := timeByID[info.TimeFrameID]
		if !ok {
			return fmt.Errorf("missing time frame %d for metric %d", info.TimeFrameID, info.MetricID)
		}
		times, err := resolveMetricTimeFrameV2(f, cacheIdentity, localTimeCache, timeInfo)
		if err != nil {
			return err
		}
		frame, err := readOneMetricMetricFrameV2(f, st.Size(), info)
		if err != nil {
			return err
		}
		pointCount, err := metricValuePointCountFromDecodedLen(frame.ValueType, frame.DecodedLen)
		if err != nil {
			return err
		}
		endOffset := int(info.TimeOffset + pointCount)
		if endOffset > len(times) {
			return fmt.Errorf("metric time slice out of bounds: offset=%d points=%d len=%d", info.TimeOffset, pointCount, len(times))
		}
		if err := collectMetricFromMetricFrameV2(database, metric, entry, frame, times[info.TimeOffset:endOffset], fromTS, toTS, stride, count, fn); err != nil {
			return err
		}
	}
	return nil
}

// BuildMetricFileV2 creates metric-<partition>.dat from data-<partition>.dat for one database.
// It does not delete or modify the source data file until the configured raw-ingest action is applied after a successful write.
func (e *Engine) BuildMetricFileV2(database, partition string) (string, error) {
	database = strings.TrimSpace(database)
	partition = strings.TrimSpace(partition)
	if database == "" {
		return "", fmt.Errorf("database cannot be empty")
	}
	if partition == "" {
		return "", fmt.Errorf("partition cannot be empty")
	}

	db, rt, err := e.getOrCreateDB(database)
	if err != nil {
		return "", err
	}
	if err := e.flushDatabases([]string{database}); err != nil {
		return "", err
	}

	partitionKind, err := partitionModeToMetricPartitionKind(rt.info.Partition)
	if err != nil {
		return "", err
	}
	return e.buildMetricFileForPartitionV2(db, partitionKind, partition)
}

// CompareDataAndMetricPartitionV2 validates that data-<partition>.dat and
// a v2 metric-<partition>.dat contain exactly the same per-metric sample stream.
func (e *Engine) CompareDataAndMetricPartitionV2(database, partition string) error {
	return e.compareDataAndMetricPartition(database, partition, compareMetricPartitionSamplesFromFileV2)
}

func (e *Engine) buildMetricFileForPartitionV2(db *Database, partitionKind byte, partition string) (string, error) {
	if db == nil {
		return "", fmt.Errorf("database unavailable")
	}

	dataPath, err := resolveMetricRawPartitionPath(db.RootDataDir, partition)
	if err != nil {
		return "", err
	}
	metricPath := filepath.Join(db.RootDataDir, "metric-"+partition+".dat")

	pages, err := buildCoalescedMetricInputsFromDataFile(db, dataPath)
	if err != nil {
		return "", err
	}
	if len(pages) == 0 {
		return "", fmt.Errorf("no persisted pages in %s", dataPath)
	}
	codec, err := BlockCompressionCodecByName(e.MetricFileCompression)
	if err != nil {
		return "", err
	}
	if err := WriteMetricFileV2(metricPath, partitionKind, codec, pages); err != nil {
		return "", err
	}
	if err := applyMetricRawIngestAction(dataPath, e.MetricRawIngestAction); err != nil {
		return "", err
	}
	return metricPath, nil
}

func compareMetricPartitionSamplesFromFileV2(c *Catalog, path string, expected map[MetricID][]partitionSamplePoint) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	hdr, err := readMetricFileV2HeaderFromFile(f)
	if err != nil {
		return err
	}
	if _, err := readMetricFileV2FooterFromFile(f, st.Size()); err != nil {
		return err
	}
	timeEntries, err := readMetricTimeFrameIndexEntriesV2FromFile(f, hdr)
	if err != nil {
		return err
	}
	metricEntries, err := readMetricMetricFrameIndexEntriesV2FromFile(f, hdr)
	if err != nil {
		return err
	}
	timeByID := make(map[uint16]metricTimeFrameIndexEntryV2, len(timeEntries))
	for _, entry := range timeEntries {
		timeByID[entry.TimeFrameID] = entry
	}
	positions := make(map[MetricID]int, len(expected))
	localTimes := make(map[uint16][]Timestamp, len(timeEntries))
	identity := metricTimeFrameCacheIdentityV2(path, st)

	for _, info := range metricEntries {
		pts, ok := expected[info.MetricID]
		if !ok {
			return fmt.Errorf("metric %d present in metric partition but missing in data partition", info.MetricID)
		}
		timeInfo, ok := timeByID[info.TimeFrameID]
		if !ok {
			return fmt.Errorf("metric %d references missing time frame %d", info.MetricID, info.TimeFrameID)
		}
		times, err := resolveMetricTimeFrameV2(f, identity, localTimes, timeInfo)
		if err != nil {
			return err
		}
		frame, err := readOneMetricMetricFrameV2(f, st.Size(), info)
		if err != nil {
			return err
		}
		pointCount, err := metricValuePointCountFromDecodedLen(frame.ValueType, frame.DecodedLen)
		if err != nil {
			return err
		}
		endOffset := int(info.TimeOffset) + int(pointCount)
		if int(info.TimeOffset) < 0 || endOffset > len(times) {
			return fmt.Errorf("metric %d time slice out of bounds: offset=%d points=%d len=%d", info.MetricID, info.TimeOffset, pointCount, len(times))
		}
		frameTimes := times[info.TimeOffset:endOffset]

		start := positions[info.MetricID]
		end := start + len(frameTimes)
		if end > len(pts) {
			name := metricNameByID(c, info.MetricID)
			return fmt.Errorf("sample count mismatch for metric %s(%d): data=%d metric=%d", name, info.MetricID, len(pts), end)
		}

		switch frame.ValueType {
		case Int32Sample:
			if len(frame.Int32) != len(frameTimes) {
				return fmt.Errorf("metric frame corruption: int32 vector mismatch")
			}
			for i, ts := range frameTimes {
				expectedPt := pts[start+i]
				if expectedPt.TS != ts || expectedPt.ValueType != frame.ValueType || expectedPt.Raw != uint32(frame.Int32[i]) {
					name := metricNameByID(c, info.MetricID)
					return fmt.Errorf("sample mismatch for metric %s(%d) at index %d", name, info.MetricID, start+i)
				}
			}
		case Float32Sample:
			if len(frame.Float32) != len(frameTimes) {
				return fmt.Errorf("metric frame corruption: float32 vector mismatch")
			}
			for i, ts := range frameTimes {
				expectedPt := pts[start+i]
				if expectedPt.TS != ts || expectedPt.ValueType != frame.ValueType || expectedPt.Raw != math.Float32bits(frame.Float32[i]) {
					name := metricNameByID(c, info.MetricID)
					return fmt.Errorf("sample mismatch for metric %s(%d) at index %d", name, info.MetricID, start+i)
				}
			}
		default:
			return fmt.Errorf("unsupported value type: %d", frame.ValueType)
		}

		positions[info.MetricID] = end
	}

	for mid, pts := range expected {
		if positions[mid] != len(pts) {
			name := metricNameByID(c, mid)
			return fmt.Errorf("sample count mismatch for metric %s(%d): data=%d metric=%d", name, mid, len(pts), positions[mid])
		}
	}
	return nil
}

func readMetricFileSummaryV2(path string) (MetricFileSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return MetricFileSummary{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return MetricFileSummary{}, err
	}
	if st.Size() < metricFileV2HeaderLen+metricFileV2FooterLen {
		return MetricFileSummary{}, fmt.Errorf("file too small")
	}
	if _, err := readMetricFileV2FooterFromFile(f, st.Size()); err != nil {
		return MetricFileSummary{}, err
	}
	hdr, err := readMetricFileV2HeaderFromFile(f)
	if err != nil {
		return MetricFileSummary{}, err
	}
	metricEntries, err := readMetricMetricFrameIndexEntriesV2FromFile(f, hdr)
	if err != nil {
		return MetricFileSummary{}, err
	}
	summary := MetricFileSummary{
		Version:        metricFileV2Version,
		TimeFrameCount: int(hdr.TimeFrameCount),
		MetricFrames:   make([]MetricFileFrameInfo, 0, len(metricEntries)),
	}
	for i, info := range metricEntries {
		points, err := metricValuePointCountFromDecodedLen(info.ValueType, info.DecodedLen)
		if err != nil {
			return MetricFileSummary{}, err
		}
		summary.MetricFrames = append(summary.MetricFrames, MetricFileFrameInfo{
			Index:           i,
			MetricID:        info.MetricID,
			ValueType:       info.ValueType,
			MetricMinTS:     info.StartTS,
			MetricMaxTS:     info.EndTS,
			PointCount:      points,
			UncompressedLen: info.DecodedLen,
			PayloadLen:      info.PayloadLen,
		})
	}
	return summary, nil
}

func walkMetricFileFramesV2(path string, fn func(MetricFileFrameInfo) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() < metricFileV2HeaderLen+metricFileV2FooterLen {
		return fmt.Errorf("file too small")
	}
	if _, err := readMetricFileV2FooterFromFile(f, st.Size()); err != nil {
		return err
	}
	hdr, err := readMetricFileV2HeaderFromFile(f)
	if err != nil {
		return err
	}
	timeEntries, err := readMetricTimeFrameIndexEntriesV2FromFile(f, hdr)
	if err != nil {
		return err
	}
	metricEntries, err := readMetricMetricFrameIndexEntriesV2FromFile(f, hdr)
	if err != nil {
		return err
	}
	timeByID := make(map[uint16]metricTimeFrameIndexEntryV2, len(timeEntries))
	for _, entry := range timeEntries {
		timeByID[entry.TimeFrameID] = entry
	}
	localTimeCache := make(map[uint16][]Timestamp, len(timeEntries))
	cacheIdentity := metricTimeFrameCacheIdentityV2(path, st)

	for i, info := range metricEntries {
		timeInfo, ok := timeByID[info.TimeFrameID]
		if !ok {
			return fmt.Errorf("missing time frame %d for metric %d", info.TimeFrameID, info.MetricID)
		}
		times, err := resolveMetricTimeFrameV2(f, cacheIdentity, localTimeCache, timeInfo)
		if err != nil {
			return err
		}
		frame, err := readOneMetricMetricFrameV2(f, st.Size(), info)
		if err != nil {
			return err
		}
		pointCount, err := metricValuePointCountFromDecodedLen(frame.ValueType, frame.DecodedLen)
		if err != nil {
			return err
		}
		endOffset := int(info.TimeOffset + pointCount)
		if endOffset > len(times) {
			return fmt.Errorf("metric time slice out of bounds: offset=%d points=%d len=%d", info.TimeOffset, pointCount, len(times))
		}
		if err := fn(MetricFileFrameInfo{
			Index:           i,
			MetricID:        frame.MetricID,
			ValueType:       frame.ValueType,
			MetricMinTS:     frame.StartTS,
			MetricMaxTS:     frame.EndTS,
			PointCount:      pointCount,
			UncompressedLen: frame.DecodedLen,
			PayloadLen:      frame.PayloadLen,
		}); err != nil {
			return err
		}
	}
	return nil
}
