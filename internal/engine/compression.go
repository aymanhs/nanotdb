package engine

import (
	"fmt"
	"strings"
	"sync"

	"github.com/klauspost/compress/s2"
	"github.com/klauspost/compress/zstd"
)

const (
	CompressionCodecS2ID          uint16 = 1
	CompressionCodecS2BetterID    uint16 = 2
	CompressionCodecZstdFastestID uint16 = 3
	CompressionCodecZstdDefaultID uint16 = 4

	CompressionCodecS2Name          = "s2"
	CompressionCodecS2BetterName    = "s2_better"
	CompressionCodecZstdFastestName = "zstd_fastest"
	CompressionCodecZstdDefaultName = "zstd_default"
)

type BlockCompressionCodec interface {
	ID() uint16
	Name() string
	Encode(src []byte) ([]byte, error)
	Decode(src []byte) ([]byte, error)
}

type statelessBlockCompressionCodec struct {
	id     uint16
	name   string
	encode func([]byte) ([]byte, error)
	decode func([]byte) ([]byte, error)
}

func (c statelessBlockCompressionCodec) ID() uint16 { return c.id }

func (c statelessBlockCompressionCodec) Name() string { return c.name }

func (c statelessBlockCompressionCodec) Encode(src []byte) ([]byte, error) {
	return c.encode(src)
}

func (c statelessBlockCompressionCodec) Decode(src []byte) ([]byte, error) {
	return c.decode(src)
}

type pooledZstdCodec struct {
	id         uint16
	name       string
	encPool    sync.Pool
	decodePool sync.Pool
}

func newPooledZstdCodec(id uint16, name string, opts ...zstd.EOption) BlockCompressionCodec {
	codec := &pooledZstdCodec{id: id, name: name}
	codec.encPool.New = func() any {
		enc, err := zstd.NewWriter(nil, opts...)
		if err != nil {
			panic(err)
		}
		return enc
	}
	codec.decodePool.New = func() any {
		dec, err := zstd.NewReader(nil)
		if err != nil {
			panic(err)
		}
		return dec
	}
	return codec
}

func (c *pooledZstdCodec) ID() uint16 { return c.id }

func (c *pooledZstdCodec) Name() string { return c.name }

func (c *pooledZstdCodec) Encode(src []byte) ([]byte, error) {
	enc := c.encPool.Get().(*zstd.Encoder)
	defer c.encPool.Put(enc)
	return enc.EncodeAll(src, nil), nil
}

func (c *pooledZstdCodec) Decode(src []byte) ([]byte, error) {
	dec := c.decodePool.Get().(*zstd.Decoder)
	defer c.decodePool.Put(dec)
	return dec.DecodeAll(src, nil)
}

var blockCompressionCodecByName = map[string]BlockCompressionCodec{}
var blockCompressionCodecByID = map[uint16]BlockCompressionCodec{}
var knownBlockCompressionCodecNames []string

func init() {
	registerBlockCompressionCodec(statelessBlockCompressionCodec{
		id:   CompressionCodecS2ID,
		name: CompressionCodecS2Name,
		encode: func(src []byte) ([]byte, error) {
			return s2.Encode(nil, src), nil
		},
		decode: func(src []byte) ([]byte, error) {
			return s2.Decode(nil, src)
		},
	})
	registerBlockCompressionCodec(statelessBlockCompressionCodec{
		id:   CompressionCodecS2BetterID,
		name: CompressionCodecS2BetterName,
		encode: func(src []byte) ([]byte, error) {
			return s2.EncodeBetter(nil, src), nil
		},
		decode: func(src []byte) ([]byte, error) {
			return s2.Decode(nil, src)
		},
	})
	registerBlockCompressionCodec(newPooledZstdCodec(
		CompressionCodecZstdFastestID,
		CompressionCodecZstdFastestName,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
	))
	registerBlockCompressionCodec(newPooledZstdCodec(
		CompressionCodecZstdDefaultID,
		CompressionCodecZstdDefaultName,
	))
}

func registerBlockCompressionCodec(codec BlockCompressionCodec) {
	name := strings.ToLower(strings.TrimSpace(codec.Name()))
	if name == "" {
		panic("compression codec name cannot be empty")
	}
	if _, exists := blockCompressionCodecByName[name]; exists {
		panic(fmt.Sprintf("duplicate compression codec name: %s", name))
	}
	if _, exists := blockCompressionCodecByID[codec.ID()]; exists {
		panic(fmt.Sprintf("duplicate compression codec id: %d", codec.ID()))
	}
	blockCompressionCodecByName[name] = codec
	blockCompressionCodecByID[codec.ID()] = codec
	knownBlockCompressionCodecNames = append(knownBlockCompressionCodecNames, name)
}

func BlockCompressionCodecByName(name string) (BlockCompressionCodec, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	codec, ok := blockCompressionCodecByName[name]
	if !ok {
		return nil, fmt.Errorf("unknown compression codec %q (valid: %s)", name, strings.Join(knownBlockCompressionCodecNames, ", "))
	}
	return codec, nil
}

func BlockCompressionCodecByID(id uint16) (BlockCompressionCodec, error) {
	codec, ok := blockCompressionCodecByID[id]
	if !ok {
		return nil, fmt.Errorf("unknown compression codec id %d", id)
	}
	return codec, nil
}

func DefaultMetricFileCompressionCodec() BlockCompressionCodec {
	codec, err := BlockCompressionCodecByName(CompressionCodecZstdFastestName)
	if err != nil {
		panic(err)
	}
	return codec
}
