// Package model2vec implements pure-Go static text embeddings compatible with
// MinishLab Model2Vec models (config.json, tokenizer.json, model.safetensors).
package model2vec

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

const maxSafetensorsHeader = 64 << 20 // 64 MiB sanity cap

type tensorMeta struct {
	DType       string   `json:"dtype"`
	Shape       []int    `json:"shape"`
	DataOffsets [2]int64 `json:"data_offsets"`
}

type safetensorsData struct {
	Embeddings []float32
	Rows       int
	Dim        int
	Weights    []float32 // optional per-token weights
	Mapping    []int     // optional token_id -> embedding row mapping
}

func loadSafetensors(path string) (*safetensorsData, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open safetensors: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat safetensors: %w", err)
	}
	fileSize := info.Size()
	if fileSize < 10 {
		return nil, errors.New("safetensors file too small")
	}

	var hdrLenBuf [8]byte
	if _, err := io.ReadFull(f, hdrLenBuf[:]); err != nil {
		return nil, fmt.Errorf("read safetensors header length: %w", err)
	}
	hdrLen := binary.LittleEndian.Uint64(hdrLenBuf[:])
	if hdrLen < 2 || hdrLen > maxSafetensorsHeader || int64(hdrLen)+8 > fileSize {
		return nil, fmt.Errorf("invalid safetensors header length %d", hdrLen)
	}

	hdrBytes := make([]byte, hdrLen)
	if _, err := io.ReadFull(f, hdrBytes); err != nil {
		return nil, fmt.Errorf("read safetensors header: %w", err)
	}

	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(hdrBytes, &rawMap); err != nil {
		return nil, fmt.Errorf("parse safetensors header: %w", err)
	}

	tensors := make(map[string]tensorMeta, len(rawMap))
	for k, v := range rawMap {
		if k == "__metadata__" {
			continue
		}
		var tm tensorMeta
		if err := json.Unmarshal(v, &tm); err != nil {
			return nil, fmt.Errorf("parse tensor %q metadata: %w", k, err)
		}
		tensors[k] = tm
	}

	embMeta, ok := tensors["embeddings"]
	if !ok {
		return nil, errors.New("safetensors missing \"embeddings\" tensor")
	}
	if len(embMeta.Shape) != 2 || embMeta.Shape[0] <= 0 || embMeta.Shape[1] <= 0 {
		return nil, fmt.Errorf("embeddings tensor must be 2D with positive shape, got %v", embMeta.Shape)
	}

	dataBase := int64(8) + int64(hdrLen)
	emb, err := readFloatTensor(f, dataBase, fileSize, "embeddings", embMeta)
	if err != nil {
		return nil, err
	}

	out := &safetensorsData{
		Embeddings: emb,
		Rows:       embMeta.Shape[0],
		Dim:        embMeta.Shape[1],
	}

	if wMeta, ok := tensors["weights"]; ok {
		if len(wMeta.Shape) != 1 {
			return nil, fmt.Errorf("weights tensor must be 1D, got %v", wMeta.Shape)
		}
		w, err := readFloatTensor(f, dataBase, fileSize, "weights", wMeta)
		if err != nil {
			return nil, err
		}
		out.Weights = w
	}

	if mMeta, ok := tensors["mapping"]; ok {
		if len(mMeta.Shape) != 1 {
			return nil, fmt.Errorf("mapping tensor must be 1D, got %v", mMeta.Shape)
		}
		m, err := readIntTensor(f, dataBase, fileSize, "mapping", mMeta)
		if err != nil {
			return nil, err
		}
		out.Mapping = m
	}

	return out, nil
}

func validateTensorBounds(dataBase, fileSize int64, name string, meta tensorMeta, elemSize int) (int, error) {
	if meta.DataOffsets[0] < 0 || meta.DataOffsets[1] < meta.DataOffsets[0] {
		return 0, fmt.Errorf("tensor %q has invalid offsets %v", name, meta.DataOffsets)
	}
	if dataBase+meta.DataOffsets[1] > fileSize {
		return 0, fmt.Errorf("tensor %q exceeds file size", name)
	}
	count := 1
	for _, d := range meta.Shape {
		if d < 0 {
			return 0, fmt.Errorf("tensor %q has negative dimension %d", name, d)
		}
		if d > 0 && count > math.MaxInt/d {
			return 0, fmt.Errorf("tensor %q shape overflows int", name)
		}
		count *= d
	}
	wantBytes := int64(count) * int64(elemSize)
	gotBytes := meta.DataOffsets[1] - meta.DataOffsets[0]
	if wantBytes != gotBytes {
		return 0, fmt.Errorf("tensor %q byte size %d does not match shape*dtype (%d)", name, gotBytes, wantBytes)
	}
	return count, nil
}

func dtypeSize(dtype string) (int, error) {
	switch dtype {
	case "F32", "I32", "U32":
		return 4, nil
	case "F16", "BF16", "I16", "U16":
		return 2, nil
	case "I8", "U8":
		return 1, nil
	case "I64", "U64", "F64":
		return 8, nil
	default:
		return 0, fmt.Errorf("unsupported tensor dtype %q", dtype)
	}
}

func readFloatTensor(rs io.ReadSeeker, dataBase, fileSize int64, name string, meta tensorMeta) ([]float32, error) {
	elemSize, err := dtypeSize(meta.DType)
	if err != nil {
		return nil, fmt.Errorf("tensor %q: %w", name, err)
	}
	count, err := validateTensorBounds(dataBase, fileSize, name, meta, elemSize)
	if err != nil {
		return nil, err
	}
	if _, err := rs.Seek(dataBase+meta.DataOffsets[0], io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek tensor %q: %w", name, err)
	}

	out := make([]float32, count)
	br := bufio.NewReaderSize(rs, 256*1024)
	var buf [8]byte

	switch meta.DType {
	case "F32":
		for i := range count {
			if _, err := io.ReadFull(br, buf[:4]); err != nil {
				return nil, fmt.Errorf("read tensor %q: %w", name, err)
			}
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[:4]))
		}
	case "F16":
		for i := range count {
			if _, err := io.ReadFull(br, buf[:2]); err != nil {
				return nil, fmt.Errorf("read tensor %q: %w", name, err)
			}
			out[i] = float16ToFloat32(binary.LittleEndian.Uint16(buf[:2]))
		}
	case "BF16":
		for i := range count {
			if _, err := io.ReadFull(br, buf[:2]); err != nil {
				return nil, fmt.Errorf("read tensor %q: %w", name, err)
			}
			bits := uint32(binary.LittleEndian.Uint16(buf[:2])) << 16
			out[i] = math.Float32frombits(bits)
		}
	case "F64":
		for i := range count {
			if _, err := io.ReadFull(br, buf[:8]); err != nil {
				return nil, fmt.Errorf("read tensor %q: %w", name, err)
			}
			out[i] = float32(math.Float64frombits(binary.LittleEndian.Uint64(buf[:8])))
		}
	case "I8":
		for i := range count {
			if _, err := io.ReadFull(br, buf[:1]); err != nil {
				return nil, fmt.Errorf("read tensor %q: %w", name, err)
			}
			out[i] = float32(int8(buf[0]))
		}
	default:
		return nil, fmt.Errorf("tensor %q has unsupported float dtype %q", name, meta.DType)
	}
	return out, nil
}

func readIntTensor(rs io.ReadSeeker, dataBase, fileSize int64, name string, meta tensorMeta) ([]int, error) {
	elemSize, err := dtypeSize(meta.DType)
	if err != nil {
		return nil, fmt.Errorf("tensor %q: %w", name, err)
	}
	count, err := validateTensorBounds(dataBase, fileSize, name, meta, elemSize)
	if err != nil {
		return nil, err
	}
	if _, err := rs.Seek(dataBase+meta.DataOffsets[0], io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek tensor %q: %w", name, err)
	}

	out := make([]int, count)
	br := bufio.NewReaderSize(rs, 64*1024)
	var buf [8]byte

	switch meta.DType {
	case "I64":
		for i := range count {
			if _, err := io.ReadFull(br, buf[:8]); err != nil {
				return nil, fmt.Errorf("read tensor %q: %w", name, err)
			}
			out[i] = int(int64(binary.LittleEndian.Uint64(buf[:8])))
		}
	case "U64":
		for i := range count {
			if _, err := io.ReadFull(br, buf[:8]); err != nil {
				return nil, fmt.Errorf("read tensor %q: %w", name, err)
			}
			out[i] = int(binary.LittleEndian.Uint64(buf[:8]))
		}
	case "I32":
		for i := range count {
			if _, err := io.ReadFull(br, buf[:4]); err != nil {
				return nil, fmt.Errorf("read tensor %q: %w", name, err)
			}
			out[i] = int(int32(binary.LittleEndian.Uint32(buf[:4])))
		}
	case "U32":
		for i := range count {
			if _, err := io.ReadFull(br, buf[:4]); err != nil {
				return nil, fmt.Errorf("read tensor %q: %w", name, err)
			}
			out[i] = int(binary.LittleEndian.Uint32(buf[:4]))
		}
	default:
		return nil, fmt.Errorf("tensor %q has unsupported int dtype %q", name, meta.DType)
	}
	return out, nil
}

// float16ToFloat32 converts an IEEE 754 binary16 value to float32.
func float16ToFloat32(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32((h >> 10) & 0x1F)
	mant := uint32(h & 0x3FF)

	switch exp {
	case 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		// Subnormal half -> normalized float32.
		exp = 127 - 15 + 1
		for (mant & 0x400) == 0 {
			mant <<= 1
			exp--
		}
		mant &= 0x3FF
		return math.Float32frombits(sign | (exp << 23) | (mant << 13))
	case 0x1F:
		// Inf or NaN.
		return math.Float32frombits(sign | 0x7F800000 | (mant << 13))
	default:
		return math.Float32frombits(sign | ((exp + 127 - 15) << 23) | (mant << 13))
	}
}
