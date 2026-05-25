package engine

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

type partitionSamplePoint struct {
	TS        Timestamp
	ValueType byte
	Raw       uint32
}

type metricBuildPlan struct {
	valueType byte
	points    int
}

type metricBuildCursor struct {
	index int
	next  int
}

func buildCoalescedMetricInputsFromDataFile(db *Database, dataPath string) ([]MetricFilePageInput, error) {
	totals, order, err := scanMetricBuildPlansFromDataFile(db.catalog, dataPath)
	if err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return nil, nil
	}

	cursors := make(map[MetricID]metricBuildCursor, len(order))
	out := make([]MetricFilePageInput, 0, len(order))
	for _, mid := range order {
		plan := totals[mid]
		page := MetricFilePageInput{
			MetricID:  mid,
			ValueType: plan.valueType,
			Times:     make([]Timestamp, plan.points),
		}
		switch plan.valueType {
		case Int32Sample:
			page.Int32 = make([]int32, plan.points)
		case Float32Sample:
			page.Float32 = make([]float32, plan.points)
		default:
			return nil, fmt.Errorf("unsupported value type: %d", plan.valueType)
		}
		cursors[mid] = metricBuildCursor{index: len(out)}
		out = append(out, page)
	}

	err = walkDataPages(dataPath, func(p *Page) error {
		return appendPageSamplesToMetricPages(db.catalog, p, out, cursors)
	})
	if err != nil {
		return nil, err
	}

	for _, cursor := range cursors {
		page := out[cursor.index]
		if cursor.next != len(page.Times) {
			return nil, fmt.Errorf("metric %d fill mismatch: wrote=%d want=%d", page.MetricID, cursor.next, len(page.Times))
		}
	}
	for i := range out {
		normalizeMetricFilePageInputOrder(&out[i])
	}

	return out, nil
}

func scanMetricBuildPlansFromDataFile(c *Catalog, dataPath string) (map[MetricID]metricBuildPlan, []MetricID, error) {
	totals := make(map[MetricID]metricBuildPlan)
	order := make([]MetricID, 0)
	err := walkDataPages(dataPath, func(p *Page) error {
		if len(p.Metrics) != len(p.Times) {
			return fmt.Errorf("page corruption: metrics/times length mismatch")
		}
		for _, mid := range p.Metrics {
			_, entry, ok := c.GetMetricByID(mid)
			if !ok {
				return fmt.Errorf("unknown metric id in page: %d", mid)
			}
			plan, ok := totals[mid]
			if !ok {
				totals[mid] = metricBuildPlan{valueType: entry.ValueType, points: 1}
				order = append(order, mid)
				continue
			}
			if plan.valueType != entry.ValueType {
				return fmt.Errorf("value type mismatch while planning metric %d", mid)
			}
			plan.points++
			totals[mid] = plan
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return totals, order, nil
}

func appendPageSamplesToMetricPages(c *Catalog, p *Page, out []MetricFilePageInput, cursors map[MetricID]metricBuildCursor) error {
	if len(p.Metrics) != len(p.Times) {
		return fmt.Errorf("page corruption: metrics/times length mismatch")
	}
	values := p.Values.Bytes()
	if len(values) < len(p.Metrics)*4 {
		return fmt.Errorf("page corruption: values blob too short")
	}

	for i, mid := range p.Metrics {
		_, entry, ok := c.GetMetricByID(mid)
		if !ok {
			return fmt.Errorf("unknown metric id in page: %d", mid)
		}
		cursor, ok := cursors[mid]
		if !ok {
			return fmt.Errorf("metric %d missing from build plan", mid)
		}
		page := &out[cursor.index]
		if page.ValueType != entry.ValueType {
			return fmt.Errorf("value type mismatch while filling metric %d", mid)
		}
		if cursor.next >= len(page.Times) {
			return fmt.Errorf("metric %d fill overflow", mid)
		}

		page.Times[cursor.next] = p.Times[i]

		raw := binary.LittleEndian.Uint32(values[i*4 : i*4+4])
		switch entry.ValueType {
		case Int32Sample:
			page.Int32[cursor.next] = int32(raw)
		case Float32Sample:
			page.Float32[cursor.next] = math.Float32frombits(raw)
		default:
			return fmt.Errorf("unsupported value type: %d", entry.ValueType)
		}

		cursor.next++
		cursors[mid] = cursor
	}
	return nil
}

func walkDataPages(dataPath string, fn func(*Page) error) error {
	f, err := os.Open(dataPath)
	if err != nil {
		return err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	var p Page
	for {
		var header [HeaderSize]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			if err == io.ErrUnexpectedEOF {
				return fmt.Errorf("truncated frame header")
			}
			return err
		}

		compressedLen, err := binary.ReadUvarint(r)
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("truncated frame length")
			}
			return err
		}

		compressed := make([]byte, compressedLen)
		if _, err := io.ReadFull(r, compressed); err != nil {
			return fmt.Errorf("truncated compressed payload")
		}
		var crcBytes [4]byte
		if _, err := io.ReadFull(r, crcBytes[:]); err != nil {
			return fmt.Errorf("truncated frame checksum")
		}

		var h PageHeader
		if err := h.Decode(bytes.NewReader(header[:])); err != nil {
			return fmt.Errorf("decode page header: %w", err)
		}
		if err := p.DecodeCompressedFrame(h, compressed, binary.LittleEndian.Uint32(crcBytes[:])); err != nil {
			return fmt.Errorf("decode page: %w", err)
		}
		if err := fn(&p); err != nil {
			return err
		}
	}
}

func buildMetricPagesFromDataFile(db *Database, dataPath string) ([]MetricFilePageInput, error) {
	out := make([]MetricFilePageInput, 0)
	err := walkDataPages(dataPath, func(p *Page) error {
		pageInputs, err := splitPageByMetric(db.catalog, p)
		if err != nil {
			return err
		}
		out = append(out, pageInputs...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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

	type metricTotals struct {
		valueType byte
		points    int
	}

	totals := make(map[MetricID]metricTotals, len(pages))
	order := make([]MetricID, 0, len(pages))
	for _, page := range pages {
		if page.MetricID == 0 {
			return nil, fmt.Errorf("metric id cannot be 0")
		}
		if len(page.Times) == 0 {
			return nil, fmt.Errorf("empty times for metric %d", page.MetricID)
		}

		total, ok := totals[page.MetricID]
		if !ok {
			totals[page.MetricID] = metricTotals{valueType: page.ValueType, points: len(page.Times)}
			order = append(order, page.MetricID)
			continue
		}
		if total.valueType != page.ValueType {
			return nil, fmt.Errorf("value type mismatch while merging metric %d", page.MetricID)
		}
		total.points += len(page.Times)
		totals[page.MetricID] = total
	}

	byMetric := make(map[MetricID]int, len(order))
	out := make([]MetricFilePageInput, 0, len(order))
	for _, mid := range order {
		total := totals[mid]
		merged := MetricFilePageInput{
			MetricID:  mid,
			ValueType: total.valueType,
			Times:     make([]Timestamp, 0, total.points),
		}
		switch total.valueType {
		case Int32Sample:
			merged.Int32 = make([]int32, 0, total.points)
		case Float32Sample:
			merged.Float32 = make([]float32, 0, total.points)
		default:
			return nil, fmt.Errorf("unsupported value type: %d", total.valueType)
		}
		byMetric[mid] = len(out)
		out = append(out, merged)
	}

	for _, page := range pages {
		idx := byMetric[page.MetricID]
		merged := &out[idx]

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
	for i := range out {
		normalizeMetricFilePageInputOrder(&out[i])
	}

	return out, nil
}

func normalizeMetricFilePageInputOrder(page *MetricFilePageInput) {
	if page == nil || len(page.Times) < 2 {
		return
	}
	alreadySorted := true
	for i := 1; i < len(page.Times); i++ {
		if page.Times[i-1] > page.Times[i] {
			alreadySorted = false
			break
		}
	}
	if alreadySorted {
		return
	}
	order := make([]int, len(page.Times))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		return page.Times[order[i]] < page.Times[order[j]]
	})

	sortedTimes := make([]Timestamp, len(page.Times))
	for i, idx := range order {
		sortedTimes[i] = page.Times[idx]
	}
	page.Times = sortedTimes

	switch page.ValueType {
	case Int32Sample:
		sortedValues := make([]int32, len(page.Int32))
		for i, idx := range order {
			sortedValues[i] = page.Int32[idx]
		}
		page.Int32 = sortedValues
	case Float32Sample:
		sortedValues := make([]float32, len(page.Float32))
		for i, idx := range order {
			sortedValues[i] = page.Float32[idx]
		}
		page.Float32 = sortedValues
	}
}

func normalizePartitionSamplePointOrder(points []partitionSamplePoint) {
	if len(points) < 2 {
		return
	}
	alreadySorted := true
	for i := 1; i < len(points); i++ {
		if points[i-1].TS > points[i].TS {
			alreadySorted = false
			break
		}
	}
	if alreadySorted {
		return
	}
	sort.SliceStable(points, func(i, j int) bool {
		return points[i].TS < points[j].TS
	})
}

func collectRawPartitionSamples(db *Database, dataPath string) (map[MetricID][]partitionSamplePoint, error) {
	out := make(map[MetricID][]partitionSamplePoint)
	err := walkDataPages(dataPath, func(p *Page) error {
		if len(p.Metrics) != len(p.Times) {
			return fmt.Errorf("page corruption: metrics/times length mismatch")
		}
		values := p.Values.Bytes()
		if len(values) < len(p.Metrics)*4 {
			return fmt.Errorf("page corruption: values blob too short")
		}
		for i, mid := range p.Metrics {
			_, entry, ok := db.catalog.GetMetricByID(mid)
			if !ok {
				return fmt.Errorf("unknown metric id in page: %d", mid)
			}
			out[mid] = append(out[mid], partitionSamplePoint{
				TS:        p.Times[i],
				ValueType: entry.ValueType,
				Raw:       binary.LittleEndian.Uint32(values[i*4 : i*4+4]),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for mid := range out {
		normalizePartitionSamplePointOrder(out[mid])
	}
	return out, nil
}
