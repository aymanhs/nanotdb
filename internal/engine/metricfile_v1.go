// Metric partition file logic.
// See DESIGN.md for format details

package engine

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	metricFileV1HeaderLen      = 64
	metricFileV1FrameHeaderLen = 48
	metricFileV1PageInfoLen    = 44
	metricFileV1FooterLen      = 16

	metricFileV1Version = 1
)

var (
	metricFileMagic       = [4]byte{'N', 'T', 'M', 'F'}
	metricFileFrameMagic  = [4]byte{'M', 'P', 'G', '1'}
	metricFileFooterMagic = [4]byte{'N', 'T', 'F', 'T'}
)

const (
	MetricPartitionDay uint8 = iota + 1
	MetricPartitionMonth
	MetricPartitionYear
	MetricPartitionForever
)

const (
	MetricRawIngestActionKeep   = "keep"
	MetricRawIngestActionDelete = "delete"
	MetricRawIngestActionRename = "rename"
)

type MetricFilePageInput struct {
	MetricID  MetricID
	ValueType byte
	Times     []Timestamp
	Int32     []int32
	Float32   []float32
}

type MetricFilePage struct {
	MetricID        MetricID
	CodecID         uint16
	ValueType       byte
	Times           []Timestamp
	Int32           []int32
	Float32         []float32
	PageOffset      uint64
	PointCount      uint32
	PayloadLen      uint32
	UncompressedLen uint32
	MetricMinTS     Timestamp
	MetricMaxTS     Timestamp
}

type metricFileV1PageInfo struct {
	MetricID        MetricID
	ValueType       byte
	PageOffset      uint64
	MetricMinTS     Timestamp
	MetricMaxTS     Timestamp
	PointCount      uint32
	UncompressedLen uint32
	PayloadLen      uint32
}

type metricFileSamplePoint struct {
	TS        Timestamp
	ValueType byte
	Raw       uint32
}

func WriteMetricFileV1(path string, partitionKind uint8, codec BlockCompressionCodec, pages []MetricFilePageInput) error {
	if partitionKind < MetricPartitionDay || partitionKind > MetricPartitionForever {
		return fmt.Errorf("invalid partition kind: %d", partitionKind)
	}
	if len(pages) == 0 {
		return fmt.Errorf("no pages provided")
	}
	if codec == nil {
		codec = DefaultMetricFileCompressionCodec()
	}

	indexed := make([]struct {
		idx int
		in  MetricFilePageInput
	}, len(pages))
	for i := range pages {
		indexed[i] = struct {
			idx int
			in  MetricFilePageInput
		}{idx: i, in: pages[i]}
	}
	sort.SliceStable(indexed, func(i, j int) bool {
		if indexed[i].in.MetricID == indexed[j].in.MetricID {
			return indexed[i].idx < indexed[j].idx
		}
		return indexed[i].in.MetricID < indexed[j].in.MetricID
	})

	infos := make([]metricFileV1PageInfo, 0, len(indexed))
	var fileMin, fileMax Timestamp
	firstTS := true

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

	header := make([]byte, metricFileV1HeaderLen)
	copy(header[0:4], metricFileMagic[:])
	binary.LittleEndian.PutUint16(header[4:6], metricFileV1Version)
	binary.LittleEndian.PutUint16(header[6:8], metricFileV1HeaderLen)
	binary.LittleEndian.PutUint32(header[8:12], 1)
	header[12] = partitionKind
	binary.LittleEndian.PutUint32(header[32:36], uint32(len(indexed)))
	binary.LittleEndian.PutUint32(header[36:40], uint32(len(indexed)))
	binary.LittleEndian.PutUint32(header[60:64], crc32.ChecksumIEEE(header[:60]))
	if _, err := f.Write(header); err != nil {
		return err
	}

	curOffset := uint64(metricFileV1HeaderLen)
	for _, item := range indexed {
		in := item.in
		frame, info, err := encodeMetricFrame(codec, in, curOffset)
		if err != nil {
			return err
		}
		if _, err := f.Write(frame); err != nil {
			return err
		}
		curOffset += uint64(len(frame))
		infos = append(infos, info)

		if firstTS {
			fileMin = info.MetricMinTS
			fileMax = info.MetricMaxTS
			firstTS = false
		} else {
			if info.MetricMinTS < fileMin {
				fileMin = info.MetricMinTS
			}
			if info.MetricMaxTS > fileMax {
				fileMax = info.MetricMaxTS
			}
		}
	}

	binary.LittleEndian.PutUint64(header[16:24], uint64(fileMin))
	binary.LittleEndian.PutUint64(header[24:32], uint64(fileMax))
	binary.LittleEndian.PutUint32(header[60:64], crc32.ChecksumIEEE(header[:60]))
	if _, err := f.WriteAt(header, 0); err != nil {
		return err
	}

	for _, info := range infos {
		entry := make([]byte, metricFileV1PageInfoLen)
		binary.LittleEndian.PutUint16(entry[0:2], uint16(info.MetricID))
		entry[2] = info.ValueType
		entry[3] = 0
		binary.LittleEndian.PutUint64(entry[4:12], info.PageOffset)
		binary.LittleEndian.PutUint64(entry[12:20], uint64(info.MetricMinTS))
		binary.LittleEndian.PutUint64(entry[20:28], uint64(info.MetricMaxTS))
		binary.LittleEndian.PutUint32(entry[28:32], info.PointCount)
		binary.LittleEndian.PutUint32(entry[32:36], info.UncompressedLen)
		binary.LittleEndian.PutUint32(entry[36:40], info.PayloadLen)
		binary.LittleEndian.PutUint32(entry[40:44], 0)
		if _, err := f.Write(entry); err != nil {
			return err
		}
	}

	footer := make([]byte, metricFileV1FooterLen)
	copy(footer[0:4], metricFileFooterMagic[:])
	binary.LittleEndian.PutUint32(footer[4:8], metricFileV1Version)
	binary.LittleEndian.PutUint32(footer[8:12], uint32(len(infos)))
	binary.LittleEndian.PutUint32(footer[12:16], crc32.ChecksumIEEE(footer[:12]))
	if _, err := f.Write(footer); err != nil {
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

// BuildMetricFileV1 creates metric-<partition>.dat from data-<partition>.dat for one database.
// It does not delete or modify the source data file.
func (e *Engine) BuildMetricFileV1(database, partition string) (string, error) {
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

	dataPath, err := resolveMetricRawPartitionPath(db.RootDataDir, partition)
	if err != nil {
		return "", err
	}
	metricPath := filepath.Join(db.RootDataDir, "metric-"+partition+".dat")

	pages, err := buildMetricPagesFromDataFile(db, dataPath)
	if err != nil {
		return "", err
	}
	if len(pages) == 0 {
		return "", fmt.Errorf("no persisted pages in %s", dataPath)
	}
	pages, err = coalesceMetricPageInputs(pages)
	if err != nil {
		return "", err
	}
	codec, err := BlockCompressionCodecByName(e.MetricFileCompression)
	if err != nil {
		return "", err
	}
	if err := WriteMetricFileV1(metricPath, partitionKind, codec, pages); err != nil {
		return "", err
	}
	if err := applyMetricRawIngestAction(dataPath, e.MetricRawIngestAction); err != nil {
		return "", err
	}
	return metricPath, nil
}

// CompareDataAndMetricPartitionV1 validates that data-<partition>.dat and
// metric-<partition>.dat contain exactly the same per-metric sample stream.
func (e *Engine) CompareDataAndMetricPartitionV1(database, partition string) error {
	database = strings.TrimSpace(database)
	partition = strings.TrimSpace(partition)
	if database == "" {
		return fmt.Errorf("database cannot be empty")
	}
	if partition == "" {
		return fmt.Errorf("partition cannot be empty")
	}

	db, _, err := e.getOrCreateDB(database)
	if err != nil {
		return err
	}

	dataPath, err := resolveMetricRawPartitionPath(db.RootDataDir, partition)
	if err != nil {
		return err
	}
	metricPath := filepath.Join(db.RootDataDir, "metric-"+partition+".dat")

	dataSamples, err := collectDataPartitionSamples(db, dataPath)
	if err != nil {
		return err
	}
	metricPages, err := ReadMetricFileV1(metricPath)
	if err != nil {
		return err
	}
	metricSamples, err := collectMetricPartitionSamples(metricPages)
	if err != nil {
		return err
	}

	if len(dataSamples) != len(metricSamples) {
		return fmt.Errorf("metric set mismatch: data=%d metric=%d", len(dataSamples), len(metricSamples))
	}

	for mid, lhs := range dataSamples {
		rhs, ok := metricSamples[mid]
		if !ok {
			return fmt.Errorf("metric %d missing from metric partition", mid)
		}
		if len(lhs) != len(rhs) {
			name := metricNameByID(db.catalog, mid)
			return fmt.Errorf("sample count mismatch for metric %s(%d): data=%d metric=%d", name, mid, len(lhs), len(rhs))
		}
		for i := range lhs {
			if lhs[i].TS != rhs[i].TS || lhs[i].ValueType != rhs[i].ValueType || lhs[i].Raw != rhs[i].Raw {
				name := metricNameByID(db.catalog, mid)
				return fmt.Errorf("sample mismatch for metric %s(%d) at index %d", name, mid, i)
			}
		}
	}

	for mid := range metricSamples {
		if _, ok := dataSamples[mid]; !ok {
			return fmt.Errorf("metric %d present in metric partition but missing in data partition", mid)
		}
	}
	return nil
}

func ReadMetricFileV1(path string) ([]MetricFilePage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < metricFileV1HeaderLen+metricFileV1FooterLen {
		return nil, fmt.Errorf("file too small")
	}

	infos, err := readMetricFilePageInfosV1(f, st.Size())
	if err != nil {
		return nil, err
	}

	out := make([]MetricFilePage, 0, len(infos))
	for _, pi := range infos {
		page, err := readOneMetricPageV1(f, st.Size(), pi)
		if err != nil {
			return nil, err
		}
		out = append(out, page)
	}
	return out, nil
}

func collectMetricFromMetricFile(database, metric string, entry MetricEntry, path string, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() < metricFileV1HeaderLen+metricFileV1FooterLen {
		return fmt.Errorf("file too small")
	}

	infos, err := readMetricFilePageInfosV1(f, st.Size())
	if err != nil {
		return err
	}

	for _, pi := range infos {
		if pi.MetricID != entry.MetricID {
			continue
		}
		if pi.MetricMaxTS < fromTS || pi.MetricMinTS > toTS {
			continue
		}

		page, err := readOneMetricPageV1(f, st.Size(), pi)
		if err != nil {
			return err
		}
		if err := collectMetricFromMetricPage(database, metric, entry, page, fromTS, toTS, stride, count, fn); err != nil {
			return err
		}
	}
	return nil
}

func readMetricFilePageInfosV1(f *os.File, fileSize int64) ([]metricFileV1PageInfo, error) {
	footer := make([]byte, metricFileV1FooterLen)
	if _, err := f.ReadAt(footer, fileSize-metricFileV1FooterLen); err != nil {
		return nil, err
	}
	if !bytes.Equal(footer[0:4], metricFileFooterMagic[:]) {
		return nil, fmt.Errorf("invalid footer magic")
	}
	if got := binary.LittleEndian.Uint32(footer[12:16]); got != crc32.ChecksumIEEE(footer[:12]) {
		return nil, fmt.Errorf("footer crc mismatch")
	}
	if binary.LittleEndian.Uint32(footer[4:8]) != metricFileV1Version {
		return nil, fmt.Errorf("unsupported trailer version")
	}
	pageInfoCount := binary.LittleEndian.Uint32(footer[8:12])

	trailerBytes := int64(pageInfoCount) * metricFileV1PageInfoLen
	trailerStart := fileSize - metricFileV1FooterLen - trailerBytes
	if trailerStart < metricFileV1HeaderLen {
		return nil, fmt.Errorf("invalid trailer bounds")
	}

	header := make([]byte, metricFileV1HeaderLen)
	if _, err := f.ReadAt(header, 0); err != nil {
		return nil, err
	}
	if !bytes.Equal(header[0:4], metricFileMagic[:]) {
		return nil, fmt.Errorf("invalid file magic")
	}
	if binary.LittleEndian.Uint16(header[4:6]) != metricFileV1Version {
		return nil, fmt.Errorf("unsupported file version")
	}
	if binary.LittleEndian.Uint16(header[6:8]) != metricFileV1HeaderLen {
		return nil, fmt.Errorf("invalid header length")
	}
	if got := binary.LittleEndian.Uint32(header[60:64]); got != crc32.ChecksumIEEE(header[:60]) {
		return nil, fmt.Errorf("header crc mismatch")
	}

	metricCount := binary.LittleEndian.Uint32(header[32:36])
	pageCount := binary.LittleEndian.Uint32(header[36:40])
	if metricCount != pageInfoCount || pageCount != pageInfoCount {
		return nil, fmt.Errorf("header/footer count mismatch")
	}

	infos := make([]metricFileV1PageInfo, 0, pageInfoCount)
	seenOffsets := make(map[uint64]struct{}, pageInfoCount)
	for i := uint32(0); i < pageInfoCount; i++ {
		off := trailerStart + int64(i)*metricFileV1PageInfoLen
		entry := make([]byte, metricFileV1PageInfoLen)
		if _, err := f.ReadAt(entry, off); err != nil {
			return nil, err
		}
		if binary.LittleEndian.Uint32(entry[40:44]) != 0 {
			return nil, fmt.Errorf("page_info reserved not zero")
		}
		pi := metricFileV1PageInfo{
			MetricID:        MetricID(binary.LittleEndian.Uint16(entry[0:2])),
			ValueType:       entry[2],
			PageOffset:      binary.LittleEndian.Uint64(entry[4:12]),
			MetricMinTS:     Timestamp(binary.LittleEndian.Uint64(entry[12:20])),
			MetricMaxTS:     Timestamp(binary.LittleEndian.Uint64(entry[20:28])),
			PointCount:      binary.LittleEndian.Uint32(entry[28:32]),
			UncompressedLen: binary.LittleEndian.Uint32(entry[32:36]),
			PayloadLen:      binary.LittleEndian.Uint32(entry[36:40]),
		}
		if _, ok := seenOffsets[pi.PageOffset]; ok {
			return nil, fmt.Errorf("duplicate page offset in page_info: %d", pi.PageOffset)
		}
		seenOffsets[pi.PageOffset] = struct{}{}
		infos = append(infos, pi)
	}
	return infos, nil
}

func collectMetricFromMetricPage(database, metric string, entry MetricEntry, page MetricFilePage, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	if page.ValueType != entry.ValueType {
		return fmt.Errorf("metric value type mismatch")
	}
	if len(page.Times) != int(page.PointCount) {
		return fmt.Errorf("metric page corruption: point count mismatch")
	}

	switch entry.ValueType {
	case Int32Sample:
		if len(page.Int32) != len(page.Times) {
			return fmt.Errorf("metric page corruption: int32 vector mismatch")
		}
	case Float32Sample:
		if len(page.Float32) != len(page.Times) {
			return fmt.Errorf("metric page corruption: float32 vector mismatch")
		}
	default:
		return fmt.Errorf("unsupported value type: %d", entry.ValueType)
	}

	for i, ts := range page.Times {
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
			s.Int32 = page.Int32[i]
		} else {
			s.Float32 = page.Float32[i]
		}
		if err := fn(s); err != nil {
			return err
		}
	}
	return nil
}

func readOneMetricPageV1(f *os.File, fileSize int64, pi metricFileV1PageInfo) (MetricFilePage, error) {
	hdr := make([]byte, metricFileV1FrameHeaderLen)
	if _, err := f.ReadAt(hdr, int64(pi.PageOffset)); err != nil {
		return MetricFilePage{}, err
	}
	if !bytes.Equal(hdr[0:4], metricFileFrameMagic[:]) {
		return MetricFilePage{}, fmt.Errorf("invalid frame magic at offset %d", pi.PageOffset)
	}
	if binary.LittleEndian.Uint16(hdr[4:6]) != metricFileV1FrameHeaderLen {
		return MetricFilePage{}, fmt.Errorf("invalid frame header length")
	}
	codecID := binary.LittleEndian.Uint16(hdr[6:8])
	codec, err := BlockCompressionCodecByID(codecID)
	if err != nil {
		return MetricFilePage{}, err
	}
	if got := binary.LittleEndian.Uint32(hdr[44:48]); got != crc32.ChecksumIEEE(hdr[:44]) {
		return MetricFilePage{}, fmt.Errorf("frame header crc mismatch")
	}

	metricID := MetricID(binary.LittleEndian.Uint16(hdr[8:10]))
	valueType := hdr[10]
	startTS := Timestamp(binary.LittleEndian.Uint64(hdr[12:20]))
	endTS := Timestamp(binary.LittleEndian.Uint64(hdr[20:28]))
	pointCount := binary.LittleEndian.Uint32(hdr[28:32])
	payloadLen := binary.LittleEndian.Uint32(hdr[32:36])
	uncompressedLen := binary.LittleEndian.Uint32(hdr[36:40])

	if metricID != pi.MetricID {
		return MetricFilePage{}, fmt.Errorf("metric id mismatch between page_info and frame")
	}
	if valueType != pi.ValueType {
		return MetricFilePage{}, fmt.Errorf("value type mismatch between page_info and frame")
	}
	if payloadLen != pi.PayloadLen || uncompressedLen != pi.UncompressedLen || pointCount != pi.PointCount {
		return MetricFilePage{}, fmt.Errorf("page_info/frame length mismatch")
	}

	payloadOffset := int64(pi.PageOffset) + metricFileV1FrameHeaderLen
	payloadEnd := payloadOffset + int64(payloadLen) + 4
	if payloadOffset < 0 || payloadEnd > fileSize {
		return MetricFilePage{}, fmt.Errorf("payload out of bounds")
	}

	compressed := make([]byte, payloadLen)
	if _, err := f.ReadAt(compressed, payloadOffset); err != nil {
		return MetricFilePage{}, err
	}
	var crcBuf [4]byte
	if _, err := f.ReadAt(crcBuf[:], payloadOffset+int64(payloadLen)); err != nil {
		return MetricFilePage{}, err
	}
	if got := binary.LittleEndian.Uint32(crcBuf[:]); got != crc32.ChecksumIEEE(compressed) {
		return MetricFilePage{}, fmt.Errorf("payload crc mismatch")
	}

	decoded, err := codec.Decode(compressed)
	if err != nil {
		return MetricFilePage{}, err
	}
	if len(decoded) != int(uncompressedLen) {
		return MetricFilePage{}, fmt.Errorf("uncompressed length mismatch")
	}
	expectedUncompressed := int(pointCount) * 12
	if expectedUncompressed != len(decoded) {
		return MetricFilePage{}, fmt.Errorf("decoded payload shape mismatch")
	}

	times := make([]Timestamp, pointCount)
	valuesOff := int(pointCount) * 8
	for i := 0; i < int(pointCount); i++ {
		times[i] = Timestamp(binary.LittleEndian.Uint64(decoded[i*8 : i*8+8]))
	}

	out := MetricFilePage{
		MetricID:        metricID,
		CodecID:         codecID,
		ValueType:       valueType,
		Times:           times,
		PageOffset:      pi.PageOffset,
		PointCount:      pointCount,
		PayloadLen:      payloadLen,
		UncompressedLen: uncompressedLen,
		MetricMinTS:     startTS,
		MetricMaxTS:     endTS,
	}
	if len(times) > 0 {
		if times[0] != startTS || times[len(times)-1] != endTS {
			return MetricFilePage{}, fmt.Errorf("frame timestamp bounds mismatch")
		}
	}

	switch valueType {
	case Int32Sample:
		out.Int32 = make([]int32, pointCount)
		for i := 0; i < int(pointCount); i++ {
			raw := binary.LittleEndian.Uint32(decoded[valuesOff+i*4 : valuesOff+i*4+4])
			out.Int32[i] = int32(raw)
		}
	case Float32Sample:
		out.Float32 = make([]float32, pointCount)
		for i := 0; i < int(pointCount); i++ {
			raw := binary.LittleEndian.Uint32(decoded[valuesOff+i*4 : valuesOff+i*4+4])
			out.Float32[i] = math.Float32frombits(raw)
		}
	default:
		return MetricFilePage{}, fmt.Errorf("unsupported value type: %d", valueType)
	}
	return out, nil
}

func encodeMetricFrame(codec BlockCompressionCodec, in MetricFilePageInput, pageOffset uint64) ([]byte, metricFileV1PageInfo, error) {
	if in.MetricID == 0 {
		return nil, metricFileV1PageInfo{}, fmt.Errorf("metric id cannot be 0")
	}
	if codec == nil {
		codec = DefaultMetricFileCompressionCodec()
	}
	n := len(in.Times)
	if n == 0 {
		return nil, metricFileV1PageInfo{}, fmt.Errorf("empty times for metric %d", in.MetricID)
	}
	for i := 1; i < n; i++ {
		if in.Times[i] < in.Times[i-1] {
			return nil, metricFileV1PageInfo{}, fmt.Errorf("non-monotonic times for metric %d", in.MetricID)
		}
	}

	payloadRaw := bytes.NewBuffer(make([]byte, 0, n*12))
	for _, ts := range in.Times {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(ts))
		if _, err := payloadRaw.Write(b[:]); err != nil {
			return nil, metricFileV1PageInfo{}, err
		}
	}
	switch in.ValueType {
	case Int32Sample:
		if len(in.Int32) != n || len(in.Float32) != 0 {
			return nil, metricFileV1PageInfo{}, fmt.Errorf("invalid int32 value vector for metric %d", in.MetricID)
		}
		for _, v := range in.Int32 {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], uint32(v))
			if _, err := payloadRaw.Write(b[:]); err != nil {
				return nil, metricFileV1PageInfo{}, err
			}
		}
	case Float32Sample:
		if len(in.Float32) != n || len(in.Int32) != 0 {
			return nil, metricFileV1PageInfo{}, fmt.Errorf("invalid float32 value vector for metric %d", in.MetricID)
		}
		for _, v := range in.Float32 {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
			if _, err := payloadRaw.Write(b[:]); err != nil {
				return nil, metricFileV1PageInfo{}, err
			}
		}
	default:
		return nil, metricFileV1PageInfo{}, fmt.Errorf("unsupported value type: %d", in.ValueType)
	}

	compressed, err := codec.Encode(payloadRaw.Bytes())
	if err != nil {
		return nil, metricFileV1PageInfo{}, err
	}

	frameHdr := make([]byte, metricFileV1FrameHeaderLen)
	copy(frameHdr[0:4], metricFileFrameMagic[:])
	binary.LittleEndian.PutUint16(frameHdr[4:6], metricFileV1FrameHeaderLen)
	binary.LittleEndian.PutUint16(frameHdr[6:8], codec.ID())
	binary.LittleEndian.PutUint16(frameHdr[8:10], uint16(in.MetricID))
	frameHdr[10] = in.ValueType
	frameHdr[11] = 0
	binary.LittleEndian.PutUint64(frameHdr[12:20], uint64(in.Times[0]))
	binary.LittleEndian.PutUint64(frameHdr[20:28], uint64(in.Times[n-1]))
	binary.LittleEndian.PutUint32(frameHdr[28:32], uint32(n))
	binary.LittleEndian.PutUint32(frameHdr[32:36], uint32(len(compressed)))
	binary.LittleEndian.PutUint32(frameHdr[36:40], uint32(payloadRaw.Len()))
	binary.LittleEndian.PutUint32(frameHdr[40:44], 0)
	binary.LittleEndian.PutUint32(frameHdr[44:48], crc32.ChecksumIEEE(frameHdr[:44]))

	frame := bytes.NewBuffer(make([]byte, 0, len(frameHdr)+len(compressed)+4))
	if _, err := frame.Write(frameHdr); err != nil {
		return nil, metricFileV1PageInfo{}, err
	}
	if _, err := frame.Write(compressed); err != nil {
		return nil, metricFileV1PageInfo{}, err
	}
	var crcTail [4]byte
	binary.LittleEndian.PutUint32(crcTail[:], crc32.ChecksumIEEE(compressed))
	if _, err := frame.Write(crcTail[:]); err != nil {
		return nil, metricFileV1PageInfo{}, err
	}

	info := metricFileV1PageInfo{
		MetricID:        in.MetricID,
		ValueType:       in.ValueType,
		PageOffset:      pageOffset,
		MetricMinTS:     in.Times[0],
		MetricMaxTS:     in.Times[n-1],
		PointCount:      uint32(n),
		UncompressedLen: uint32(payloadRaw.Len()),
		PayloadLen:      uint32(len(compressed)),
	}
	return frame.Bytes(), info, nil
}

func createAtomicTmp(path string) (*os.File, string, error) {
	tmpPath := path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		return nil, "", err
	}
	return f, tmpPath, nil
}

func isValidMetricRawIngestAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case MetricRawIngestActionKeep, MetricRawIngestActionDelete, MetricRawIngestActionRename:
		return true
	default:
		return false
	}
}

func metricRawPartitionPath(rootDir, partition string) string {
	return filepath.Join(rootDir, "data-"+partition+".dat")
}

func metricRenamedRawPartitionPath(rootDir, partition string) string {
	return filepath.Join(rootDir, "raw-"+partition+".dat")
}

func resolveMetricRawPartitionPath(rootDir, partition string) (string, error) {
	activePath := metricRawPartitionPath(rootDir, partition)
	if _, err := os.Stat(activePath); err == nil {
		return activePath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	renamedPath := metricRenamedRawPartitionPath(rootDir, partition)
	if _, err := os.Stat(renamedPath); err == nil {
		return renamedPath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return activePath, os.ErrNotExist
}

func applyMetricRawIngestAction(rawPath, action string) error {
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		action = MetricRawIngestActionKeep
	}
	switch action {
	case MetricRawIngestActionKeep:
		return nil
	case MetricRawIngestActionDelete:
		if err := os.Remove(rawPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case MetricRawIngestActionRename:
		if strings.HasPrefix(filepath.Base(rawPath), "raw-") {
			return nil
		}
		partition := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(rawPath), "data-"), ".dat")
		renamedPath := metricRenamedRawPartitionPath(filepath.Dir(rawPath), partition)
		if err := os.Remove(renamedPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Rename(rawPath, renamedPath)
	default:
		return fmt.Errorf("invalid metric raw ingest action: %q", action)
	}
}

func partitionModeToMetricPartitionKind(mode string) (uint8, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "day":
		return MetricPartitionDay, nil
	case "month":
		return MetricPartitionMonth, nil
	case "year":
		return MetricPartitionYear, nil
	case "forever":
		return MetricPartitionForever, nil
	default:
		return 0, fmt.Errorf("unsupported partition mode: %q", mode)
	}
}

func buildMetricPagesFromDataFile(db *Database, dataPath string) ([]MetricFilePageInput, error) {
	f, err := os.Open(dataPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	out := make([]MetricFilePageInput, 0)

	for {
		var header [HeaderSize]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if err == io.EOF {
				return out, nil
			}
			if err == io.ErrUnexpectedEOF {
				return nil, fmt.Errorf("truncated frame header")
			}
			return nil, err
		}

		compressedLen, err := binary.ReadUvarint(r)
		if err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("truncated frame length")
			}
			return nil, err
		}

		compressed := make([]byte, compressedLen)
		if _, err := io.ReadFull(r, compressed); err != nil {
			return nil, fmt.Errorf("truncated compressed payload")
		}
		var crcBytes [4]byte
		if _, err := io.ReadFull(r, crcBytes[:]); err != nil {
			return nil, fmt.Errorf("truncated frame checksum")
		}

		var frame bytes.Buffer
		frame.Grow(HeaderSize + binary.MaxVarintLen64 + int(compressedLen) + 4)
		_, _ = frame.Write(header[:])
		var varintBuf [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(varintBuf[:], compressedLen)
		_, _ = frame.Write(varintBuf[:n])
		_, _ = frame.Write(compressed)
		_, _ = frame.Write(crcBytes[:])

		var p Page
		if err := p.DecodeFrom(bytes.NewReader(frame.Bytes())); err != nil {
			return nil, fmt.Errorf("decode page: %w", err)
		}

		pageInputs, err := splitPageByMetric(db.catalog, &p)
		if err != nil {
			return nil, err
		}
		out = append(out, pageInputs...)
	}
}

func splitPageByMetric(c *Catalog, p *Page) ([]MetricFilePageInput, error) {
	if len(p.Metrics) != len(p.Times) {
		return nil, fmt.Errorf("page corruption: metrics/times length mismatch")
	}
	values := p.Values.Bytes()
	if len(values) < len(p.Metrics)*4 {
		return nil, fmt.Errorf("page corruption: values blob too short")
	}

	order := make([]MetricID, 0)
	byMetric := make(map[MetricID]*MetricFilePageInput)

	for i, mid := range p.Metrics {
		_, entry, ok := c.GetMetricByID(mid)
		if !ok {
			return nil, fmt.Errorf("unknown metric id in page: %d", mid)
		}
		bucket := byMetric[mid]
		if bucket == nil {
			bucket = &MetricFilePageInput{MetricID: mid, ValueType: entry.ValueType}
			byMetric[mid] = bucket
			order = append(order, mid)
		}

		bucket.Times = append(bucket.Times, p.Times[i])
		raw := binary.LittleEndian.Uint32(values[i*4 : i*4+4])
		if entry.ValueType == Int32Sample {
			bucket.Int32 = append(bucket.Int32, int32(raw))
		} else {
			bucket.Float32 = append(bucket.Float32, math.Float32frombits(raw))
		}
	}

	out := make([]MetricFilePageInput, 0, len(order))
	for _, mid := range order {
		out = append(out, *byMetric[mid])
	}
	return out, nil
}

func coalesceMetricPageInputs(pages []MetricFilePageInput) ([]MetricFilePageInput, error) {
	if len(pages) == 0 {
		return nil, nil
	}

	byMetric := make(map[MetricID]int, len(pages))
	out := make([]MetricFilePageInput, 0, len(pages))
	for _, page := range pages {
		if page.MetricID == 0 {
			return nil, fmt.Errorf("metric id cannot be 0")
		}
		if len(page.Times) == 0 {
			return nil, fmt.Errorf("empty times for metric %d", page.MetricID)
		}

		idx, ok := byMetric[page.MetricID]
		if !ok {
			copyPage := MetricFilePageInput{
				MetricID:  page.MetricID,
				ValueType: page.ValueType,
				Times:     append([]Timestamp(nil), page.Times...),
				Int32:     append([]int32(nil), page.Int32...),
				Float32:   append([]float32(nil), page.Float32...),
			}
			byMetric[page.MetricID] = len(out)
			out = append(out, copyPage)
			continue
		}

		merged := &out[idx]
		if merged.ValueType != page.ValueType {
			return nil, fmt.Errorf("value type mismatch while merging metric %d", page.MetricID)
		}
		if merged.Times[len(merged.Times)-1] > page.Times[0] {
			return nil, fmt.Errorf("non-monotonic merge order for metric %d", page.MetricID)
		}

		merged.Times = append(merged.Times, page.Times...)
		switch page.ValueType {
		case Int32Sample:
			if len(page.Int32) != len(page.Times) || len(page.Float32) != 0 {
				return nil, fmt.Errorf("invalid int32 value vector for metric %d", page.MetricID)
			}
			merged.Int32 = append(merged.Int32, page.Int32...)
		case Float32Sample:
			if len(page.Float32) != len(page.Times) || len(page.Int32) != 0 {
				return nil, fmt.Errorf("invalid float32 value vector for metric %d", page.MetricID)
			}
			merged.Float32 = append(merged.Float32, page.Float32...)
		default:
			return nil, fmt.Errorf("unsupported value type: %d", page.ValueType)
		}
	}

	return out, nil
}

func collectDataPartitionSamples(db *Database, dataPath string) (map[MetricID][]metricFileSamplePoint, error) {
	pages, err := buildMetricPagesFromDataFile(db, dataPath)
	if err != nil {
		return nil, err
	}
	out := make(map[MetricID][]metricFileSamplePoint)
	for _, p := range pages {
		pts, err := samplePointsFromInput(p)
		if err != nil {
			return nil, err
		}
		out[p.MetricID] = append(out[p.MetricID], pts...)
	}
	return out, nil
}

func collectMetricPartitionSamples(pages []MetricFilePage) (map[MetricID][]metricFileSamplePoint, error) {
	out := make(map[MetricID][]metricFileSamplePoint)
	for _, p := range pages {
		pts, err := samplePointsFromPage(p)
		if err != nil {
			return nil, err
		}
		out[p.MetricID] = append(out[p.MetricID], pts...)
	}
	return out, nil
}

func samplePointsFromInput(p MetricFilePageInput) ([]metricFileSamplePoint, error) {
	n := len(p.Times)
	out := make([]metricFileSamplePoint, 0, n)
	switch p.ValueType {
	case Int32Sample:
		if len(p.Int32) != n || len(p.Float32) != 0 {
			return nil, fmt.Errorf("invalid int32 page input for metric %d", p.MetricID)
		}
		for i := 0; i < n; i++ {
			out = append(out, metricFileSamplePoint{TS: p.Times[i], ValueType: p.ValueType, Raw: uint32(p.Int32[i])})
		}
	case Float32Sample:
		if len(p.Float32) != n || len(p.Int32) != 0 {
			return nil, fmt.Errorf("invalid float32 page input for metric %d", p.MetricID)
		}
		for i := 0; i < n; i++ {
			out = append(out, metricFileSamplePoint{TS: p.Times[i], ValueType: p.ValueType, Raw: math.Float32bits(p.Float32[i])})
		}
	default:
		return nil, fmt.Errorf("unsupported value type: %d", p.ValueType)
	}
	return out, nil
}

func samplePointsFromPage(p MetricFilePage) ([]metricFileSamplePoint, error) {
	n := len(p.Times)
	out := make([]metricFileSamplePoint, 0, n)
	switch p.ValueType {
	case Int32Sample:
		if len(p.Int32) != n || len(p.Float32) != 0 {
			return nil, fmt.Errorf("invalid int32 page for metric %d", p.MetricID)
		}
		for i := 0; i < n; i++ {
			out = append(out, metricFileSamplePoint{TS: p.Times[i], ValueType: p.ValueType, Raw: uint32(p.Int32[i])})
		}
	case Float32Sample:
		if len(p.Float32) != n || len(p.Int32) != 0 {
			return nil, fmt.Errorf("invalid float32 page for metric %d", p.MetricID)
		}
		for i := 0; i < n; i++ {
			out = append(out, metricFileSamplePoint{TS: p.Times[i], ValueType: p.ValueType, Raw: math.Float32bits(p.Float32[i])})
		}
	default:
		return nil, fmt.Errorf("unsupported value type: %d", p.ValueType)
	}
	return out, nil
}

func metricNameByID(c *Catalog, mid MetricID) string {
	name, _, ok := c.GetMetricByID(mid)
	if !ok {
		return "unknown"
	}
	return name
}
