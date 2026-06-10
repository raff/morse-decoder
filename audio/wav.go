package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

type WAV struct {
	SampleRate  int
	NumChannels int
	BitDepth    int
	Samples     []float64 // mono, normalized to [-1, 1]
}

func LoadWAV(path string) (*WAV, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return decode(f)
}

func decode(r io.Reader) (*WAV, error) {
	var riffID [4]byte
	if _, err := io.ReadFull(r, riffID[:]); err != nil {
		return nil, fmt.Errorf("read RIFF: %w", err)
	}
	if string(riffID[:]) != "RIFF" {
		return nil, fmt.Errorf("not a RIFF file")
	}

	var chunkSize uint32
	if err := binary.Read(r, binary.LittleEndian, &chunkSize); err != nil {
		return nil, err
	}

	var waveID [4]byte
	if _, err := io.ReadFull(r, waveID[:]); err != nil {
		return nil, err
	}
	if string(waveID[:]) != "WAVE" {
		return nil, fmt.Errorf("not a WAVE file")
	}

	var (
		audioFormat   uint16
		numChannels   uint16
		sampleRate    uint32
		bitsPerSample uint16
		dataFound     bool
		wav           WAV
	)

	for {
		var id [4]byte
		if _, err := io.ReadFull(r, id[:]); err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		} else if err != nil {
			return nil, err
		}

		var size uint32
		if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
			return nil, err
		}

		switch string(id[:]) {
		case "fmt ":
			if err := binary.Read(r, binary.LittleEndian, &audioFormat); err != nil {
				return nil, err
			}
			if err := binary.Read(r, binary.LittleEndian, &numChannels); err != nil {
				return nil, err
			}
			if err := binary.Read(r, binary.LittleEndian, &sampleRate); err != nil {
				return nil, err
			}
			var byteRate uint32
			binary.Read(r, binary.LittleEndian, &byteRate)
			var blockAlign uint16
			binary.Read(r, binary.LittleEndian, &blockAlign)
			if err := binary.Read(r, binary.LittleEndian, &bitsPerSample); err != nil {
				return nil, err
			}
			if size > 16 {
				io.CopyN(io.Discard, r, int64(size-16))
			}

		case "data":
			if audioFormat != 1 {
				return nil, fmt.Errorf("only PCM (format 1) supported, got format %d", audioFormat)
			}
			// Some recorders write 0xFFFFFFFF as the data size when they can't
			// seek back to fix the header (e.g. piped or interrupted recordings).
			// In that case read until EOF and use whatever arrived.
			var raw []byte
			if size == 0xFFFFFFFF {
				var err error
				raw, err = io.ReadAll(r)
				if err != nil {
					return nil, fmt.Errorf("read data chunk: %w", err)
				}
			} else {
				raw = make([]byte, size)
				if _, err := io.ReadFull(r, raw); err != nil {
					// Tolerate truncated files: use what we got
					if len(raw) == 0 {
						return nil, fmt.Errorf("read data chunk: %w", err)
					}
				}
			}
			samples, err := decodePCM(raw, uint32(numChannels), uint32(bitsPerSample))
			if err != nil {
				return nil, err
			}
			wav = WAV{
				SampleRate:  int(sampleRate),
				NumChannels: int(numChannels),
				BitDepth:    int(bitsPerSample),
				Samples:     samples,
			}
			dataFound = true

		default:
			// skip unknown chunk (pad to even size per RIFF spec)
			skip := int64(size)
			if size%2 != 0 {
				skip++
			}
			io.CopyN(io.Discard, r, skip)
		}
	}

	if !dataFound {
		return nil, fmt.Errorf("no data chunk found")
	}
	return &wav, nil
}

func decodePCM(raw []byte, numChannels, bitsPerSample uint32) ([]float64, error) {
	bytesPerSample := bitsPerSample / 8
	if bytesPerSample == 0 {
		return nil, fmt.Errorf("invalid bits per sample: %d", bitsPerSample)
	}
	totalSamples := uint32(len(raw)) / bytesPerSample
	frames := totalSamples / numChannels
	out := make([]float64, frames)

	for i := uint32(0); i < frames; i++ {
		var sum float64
		for ch := uint32(0); ch < numChannels; ch++ {
			off := (i*numChannels + ch) * bytesPerSample
			switch bitsPerSample {
			case 8:
				// 8-bit PCM is unsigned
				sum += (float64(raw[off]) - 128.0) / 128.0
			case 16:
				v := int16(binary.LittleEndian.Uint16(raw[off:]))
				sum += float64(v) / 32768.0
			case 24:
				v := int32(raw[off]) | int32(raw[off+1])<<8 | int32(raw[off+2])<<16
				if v&0x800000 != 0 {
					v |= ^int32(0xFFFFFF) // sign-extend
				}
				sum += float64(v) / 8388608.0
			case 32:
				v := int32(binary.LittleEndian.Uint32(raw[off:]))
				sum += float64(v) / 2147483648.0
			default:
				return nil, fmt.Errorf("unsupported bit depth: %d", bitsPerSample)
			}
		}
		out[i] = sum / float64(numChannels)
	}
	return out, nil
}
