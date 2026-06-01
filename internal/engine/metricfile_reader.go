package engine

import (
	"fmt"
	"math"
	"os"
)

// MetricFileReader is the version-agnostic interface to a metric-*.dat file.
// One implementation per on-disk format (V1, V2, …). Callers in the engine,
// CLI, and HTTP layers work in terms of this interface; the only place that
// switches on the version byte is OpenMetricFile.
//
// Adding a V3 format means: implement MetricFileReader, register it in
// openMetricFileForVersion. No other call site changes.
type MetricFileReader interface {
	// Version returns the metric-file format version (e.g. metricFileV1Version).
	Version() uint16
	// Summary returns a compact description of every frame in the file.
	Summary() (MetricFileSummary, error)
	// WalkFrames invokes fn for every frame in the file. Returning an error
	// from fn short-circuits the walk.
	WalkFrames(fn func(MetricFileFrameInfo) error) error
	// CollectMetric streams samples for a single metric within [fromTS, toTS]
	// to fn, advancing count by every sample emitted. stride applies to the
	// callback (0/1 = every sample).
	CollectMetric(database, metric string, entry MetricEntry, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error
	// CollectMetricSet streams samples for the supplied target set in one
	// pass over the file (used by multi-metric queries).
	CollectMetricSet(database string, targets map[MetricID]*rangeQueryTarget, fromTS, toTS Timestamp, stride int, fn SampleCallback) error
	// Compare validates this metric file against an expected sample map
	// derived from the raw data partition.
	Compare(catalog *Catalog, expected map[MetricID][]partitionSamplePoint) error
	// WalkSamples invokes fn for every (metricID, valueType, ts, raw)
	// 4-byte sample in the file. raw holds the encoded value (int32 LE for
	// Int32Sample, float32 LE bits for Float32Sample). Used by offline
	// exporters that need to translate metric files back to line protocol.
	WalkSamples(fn func(metricID MetricID, valueType byte, ts Timestamp, raw uint32) error) error
}

// OpenMetricFile reads the version byte and returns the matching
// MetricFileReader implementation. The reader is stateless w.r.t. the OS
// file handle — each method re-opens the file as needed (matching the
// pre-interface behaviour of the v1/v2 helpers).
func OpenMetricFile(path string) (MetricFileReader, error) {
	version, err := readMetricFrameVersion(path)
	if err != nil {
		return nil, err
	}
	return openMetricFileForVersion(path, version)
}

func openMetricFileForVersion(path string, version uint16) (MetricFileReader, error) {
	switch version {
	case metricFileV1Version:
		return metricFileReaderV1{path: path}, nil
	case metricFileV2Version:
		return metricFileReaderV2{path: path}, nil
	default:
		return nil, fmt.Errorf("unsupported metric file version: %d", version)
	}
}

// metricFileReaderV1 implements MetricFileReader for the V1 on-disk format.
type metricFileReaderV1 struct{ path string }

func (r metricFileReaderV1) Version() uint16 { return metricFileV1Version }

func (r metricFileReaderV1) Summary() (MetricFileSummary, error) {
	infos, err := ReadMetricFilePageInfosV1(r.path)
	if err != nil {
		return MetricFileSummary{}, err
	}
	summary := MetricFileSummary{Version: metricFileV1Version, MetricFrames: make([]MetricFileFrameInfo, 0, len(infos))}
	for _, info := range infos {
		summary.MetricFrames = append(summary.MetricFrames, MetricFileFrameInfo{
			Index:           info.Index,
			MetricID:        info.MetricID,
			ValueType:       info.ValueType,
			MetricMinTS:     info.MetricMinTS,
			MetricMaxTS:     info.MetricMaxTS,
			PointCount:      info.PointCount,
			UncompressedLen: info.UncompressedLen,
			PayloadLen:      info.PayloadLen,
		})
	}
	return summary, nil
}

func (r metricFileReaderV1) WalkFrames(fn func(MetricFileFrameInfo) error) error {
	idx := 0
	return WalkMetricFileV1(r.path, func(page MetricFilePage) error {
		info := MetricFileFrameInfo{
			Index:           idx,
			MetricID:        page.MetricID,
			ValueType:       page.ValueType,
			MetricMinTS:     page.MetricMinTS,
			MetricMaxTS:     page.MetricMaxTS,
			PointCount:      page.PointCount,
			UncompressedLen: page.UncompressedLen,
			PayloadLen:      page.PayloadLen,
		}
		idx++
		return fn(info)
	})
}

func (r metricFileReaderV1) CollectMetric(database, metric string, entry MetricEntry, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	return collectMetricFromMetricFileV1(database, metric, entry, r.path, fromTS, toTS, stride, count, fn)
}

func (r metricFileReaderV1) CollectMetricSet(database string, targets map[MetricID]*rangeQueryTarget, fromTS, toTS Timestamp, stride int, fn SampleCallback) error {
	return collectMetricSetFromMetricFileV1(database, targets, r.path, fromTS, toTS, stride, fn)
}

func (r metricFileReaderV1) Compare(catalog *Catalog, expected map[MetricID][]partitionSamplePoint) error {
	return compareMetricPartitionSamplesFromFile(catalog, r.path, expected)
}

func (r metricFileReaderV1) WalkSamples(fn func(MetricID, byte, Timestamp, uint32) error) error {
	return walkMetricFileSamplesV1(r.path, fn)
}

// metricFileReaderV2 implements MetricFileReader for the V2 on-disk format.
type metricFileReaderV2 struct{ path string }

func (r metricFileReaderV2) Version() uint16 { return metricFileV2Version }

func (r metricFileReaderV2) Summary() (MetricFileSummary, error) {
	return readMetricFileSummaryV2(r.path)
}

func (r metricFileReaderV2) WalkFrames(fn func(MetricFileFrameInfo) error) error {
	return walkMetricFileFramesV2(r.path, fn)
}

func (r metricFileReaderV2) CollectMetric(database, metric string, entry MetricEntry, fromTS, toTS Timestamp, stride int, count *int, fn SampleCallback) error {
	return collectMetricFromMetricFileV2(database, metric, entry, r.path, fromTS, toTS, stride, count, fn)
}

func (r metricFileReaderV2) CollectMetricSet(database string, targets map[MetricID]*rangeQueryTarget, fromTS, toTS Timestamp, stride int, fn SampleCallback) error {
	return collectMetricSetFromMetricFileV2(database, targets, r.path, fromTS, toTS, stride, fn)
}

func (r metricFileReaderV2) Compare(catalog *Catalog, expected map[MetricID][]partitionSamplePoint) error {
	return compareMetricPartitionSamplesFromFileV2(catalog, r.path, expected)
}

func (r metricFileReaderV2) WalkSamples(fn func(MetricID, byte, Timestamp, uint32) error) error {
	return walkMetricFileSamplesV2(r.path, fn)
}

// walkMetricFileSamplesV1 yields every sample in a V1 metric file as a raw
// 4-byte payload tagged with its metric id, value type, and timestamp. Used by
// offline LP export and any other consumer that needs to translate metric
// files back to per-sample form.
func walkMetricFileSamplesV1(path string, fn func(MetricID, byte, Timestamp, uint32) error) error {
	return WalkMetricFileV1(path, func(page MetricFilePage) error {
		switch page.ValueType {
		case Int32Sample:
			for i, ts := range page.Times {
				if err := fn(page.MetricID, page.ValueType, ts, uint32(page.Int32[i])); err != nil {
					return err
				}
			}
		case Float32Sample:
			for i, ts := range page.Times {
				if err := fn(page.MetricID, page.ValueType, ts, math.Float32bits(page.Float32[i])); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported metric value type: %d", page.ValueType)
		}
		return nil
	})
}

// walkMetricFileSamplesV2 is the V2 equivalent of walkMetricFileSamplesV1.
// It joins each metric frame against its referenced time frame to reconstruct
// (metricID, valueType, ts, raw) tuples.
func walkMetricFileSamplesV2(path string, fn func(MetricID, byte, Timestamp, uint32) error) error {
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
	timeEntries, metricEntries, err := resolveMetricFileIndexesV2(f, path, st, hdr)
	if err != nil {
		return err
	}
	timeByID := make(map[uint16]metricTimeFrameIndexEntryV2, len(timeEntries))
	for _, entry := range timeEntries {
		timeByID[entry.TimeFrameID] = entry
	}
	localTimes := make(map[uint16][]Timestamp, len(timeEntries))
	identity := metricTimeFrameCacheIdentityV2(path, st)
	for _, info := range metricEntries {
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
		start := int(info.TimeOffset)
		end := start + int(pointCount)
		if start < 0 || end > len(times) {
			return fmt.Errorf("metric %d time slice out of bounds", info.MetricID)
		}
		frameTimes := times[start:end]
		switch frame.ValueType {
		case Int32Sample:
			for i, ts := range frameTimes {
				if err := fn(info.MetricID, frame.ValueType, ts, uint32(frame.Int32[i])); err != nil {
					return err
				}
			}
		case Float32Sample:
			for i, ts := range frameTimes {
				if err := fn(info.MetricID, frame.ValueType, ts, math.Float32bits(frame.Float32[i])); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported metric value type: %d", frame.ValueType)
		}
	}
	return nil
}
