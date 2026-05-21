package engine

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/s2"
)

// DataFrameHeader describes one page frame parsed from a .dat file without decompression.
type DataFrameHeader struct {
	Index           int
	Offset          int64
	StartTime       Timestamp
	EndTime         Timestamp
	NumRecords      uint16
	CompressedLen   uint64
	UncompressedLen uint64
	FrameBytes      int64
}

// DataFileStats summarizes one .dat file.
type DataFileStats struct {
	Path            string
	FileBytes       int64
	Frames          int
	TotalRecords    int64
	TotalCompressed int64
	TotalFrameBytes int64
	MinStart        Timestamp
	MaxEnd          Timestamp
}

type DataFrameCallback func(DataFrameHeader) error

// WalkDataFileHeaders walks page headers in a .dat file and invokes fn for each decoded frame header.
// It does not decode payload pages.
func WalkDataFileHeaders(path string, fn DataFrameCallback) (DataFileStats, error) {
	st, err := os.Stat(path)
	if err != nil {
		return DataFileStats{}, err
	}

	f, err := os.Open(path)
	if err != nil {
		return DataFileStats{}, err
	}
	defer f.Close()

	stats := DataFileStats{Path: path, FileBytes: st.Size()}
	r := bufio.NewReaderSize(f, 64*1024)

	var offset int64
	for {
		frameOffset := offset

		var hdr [HeaderSize]byte
		nRead, err := io.ReadFull(r, hdr[:])
		if err == io.EOF && nRead == 0 {
			break
		}
		if err == io.ErrUnexpectedEOF {
			return stats, fmt.Errorf("truncated frame header at offset %d", frameOffset)
		}
		if err != nil {
			return stats, fmt.Errorf("read frame header at offset %d: %w", frameOffset, err)
		}
		offset += HeaderSize

		start := Timestamp(binary.LittleEndian.Uint64(hdr[0:8]))
		end := Timestamp(binary.LittleEndian.Uint64(hdr[8:16]))
		numRecords := binary.LittleEndian.Uint16(hdr[16:18])

		compressedLen, varintLen, err := readUvarintCount(r)
		if err != nil {
			return stats, fmt.Errorf("read compressed length at offset %d: %w", offset, err)
		}
		offset += int64(varintLen)

		if compressedLen > uint64((1<<63)-1)-4 {
			return stats, fmt.Errorf("compressed payload too large at offset %d", frameOffset)
		}
		compressed := make([]byte, compressedLen)
		if _, err := io.ReadFull(r, compressed); err != nil {
			return stats, fmt.Errorf("truncated frame payload at offset %d", frameOffset)
		}
		var crc [4]byte
		if _, err := io.ReadFull(r, crc[:]); err != nil {
			return stats, fmt.Errorf("truncated frame checksum at offset %d", frameOffset)
		}
		payload, err := s2.Decode(nil, compressed)
		if err != nil {
			return stats, fmt.Errorf("decode frame payload at offset %d: %w", frameOffset, err)
		}
		offset += int64(compressedLen) + 4

		frameBytes := int64(HeaderSize) + int64(varintLen) + int64(compressedLen) + 4
		frame := DataFrameHeader{
			Index:           stats.Frames,
			Offset:          frameOffset,
			StartTime:       start,
			EndTime:         end,
			NumRecords:      numRecords,
			CompressedLen:   compressedLen,
			UncompressedLen: uint64(len(payload)),
			FrameBytes:      frameBytes,
		}
		if fn != nil {
			if err := fn(frame); err != nil {
				return stats, err
			}
		}

		stats.Frames++
		stats.TotalRecords += int64(numRecords)
		stats.TotalCompressed += int64(compressedLen)
		stats.TotalFrameBytes += frameBytes
		if stats.Frames == 1 || start < stats.MinStart {
			stats.MinStart = start
		}
		if stats.Frames == 1 || end > stats.MaxEnd {
			stats.MaxEnd = end
		}
	}

	return stats, nil
}

// ScanDataFileStats reads only frame headers and compressed lengths for a .dat file.
// It does not decompress payloads or populate per-frame page details.
func ScanDataFileStats(path string) (DataFileStats, error) {
	st, err := os.Stat(path)
	if err != nil {
		return DataFileStats{}, err
	}

	f, err := os.Open(path)
	if err != nil {
		return DataFileStats{}, err
	}
	defer f.Close()

	stats := DataFileStats{Path: path, FileBytes: st.Size()}
	r := bufio.NewReaderSize(f, 64*1024)

	for {
		var hdr [HeaderSize]byte
		nRead, err := io.ReadFull(r, hdr[:])
		if err == io.EOF && nRead == 0 {
			break
		}
		if err == io.ErrUnexpectedEOF {
			return stats, fmt.Errorf("truncated frame header")
		}
		if err != nil {
			return stats, err
		}

		start := Timestamp(binary.LittleEndian.Uint64(hdr[0:8]))
		end := Timestamp(binary.LittleEndian.Uint64(hdr[8:16]))
		numRecords := binary.LittleEndian.Uint16(hdr[16:18])

		compressedLen, varintLen, err := readUvarintCount(r)
		if err != nil {
			return stats, err
		}
		if _, err := io.CopyN(io.Discard, r, int64(compressedLen)+4); err != nil {
			return stats, fmt.Errorf("truncated frame payload: %w", err)
		}

		frameBytes := int64(HeaderSize) + int64(varintLen) + int64(compressedLen) + 4
		stats.Frames++
		stats.TotalRecords += int64(numRecords)
		stats.TotalCompressed += int64(compressedLen)
		stats.TotalFrameBytes += frameBytes
		if stats.Frames == 1 || start < stats.MinStart {
			stats.MinStart = start
		}
		if stats.Frames == 1 || end > stats.MaxEnd {
			stats.MaxEnd = end
		}
	}

	return stats, nil
}

// ScanDataFileHeaders walks page headers in a .dat file and returns per-frame headers plus aggregate stats.
// It does not decode payload pages.
func ScanDataFileHeaders(path string) (DataFileStats, []DataFrameHeader, error) {
	frames := make([]DataFrameHeader, 0, 64)
	stats, err := WalkDataFileHeaders(path, func(frame DataFrameHeader) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil {
		return DataFileStats{}, nil, err
	}
	return stats, frames, nil
}

func readUvarintCount(r *bufio.Reader) (uint64, int, error) {
	var x uint64
	for i := 0; i < binary.MaxVarintLen64; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, i, err
		}
		if b < 0x80 {
			if i == binary.MaxVarintLen64-1 && b > 1 {
				return 0, i + 1, fmt.Errorf("varint overflow")
			}
			return x | uint64(b)<<uint(7*i), i + 1, nil
		}
		x |= uint64(b&0x7f) << uint(7*i)
	}
	return 0, binary.MaxVarintLen64, fmt.Errorf("varint too long")
}
