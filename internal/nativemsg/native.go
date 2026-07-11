package nativemsg

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	MaxChromeToHost  = 64 * 1024 * 1024
	MaxHostToChrome  = 1024 * 1024
	DefaultChunkSize = 512 * 1024
	MaxAssembledSize = 128 * 1024 * 1024
)

type chunkEnvelope struct {
	Chunk *chunk `json:"__bc_chunk"`
}

type chunk struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Index   int    `json:"index"`
	Total   int    `json:"total"`
	SHA256  string `json:"sha256"`
	Data    string `json:"data"`
}

type assembly struct {
	total     int
	sha256    string
	parts     map[int][]byte
	size      int
	updatedAt time.Time
}

type Assembler struct {
	mu    sync.Mutex
	items map[string]*assembly
}

func NewAssembler() *Assembler {
	return &Assembler{items: make(map[string]*assembly)}
}

// Accept returns complete=true for ordinary native messages and for the final
// chunk of a reassembled message. Intermediate chunks return complete=false.
func (a *Assembler) Accept(frame []byte) ([]byte, bool, error) {
	if !json.Valid(frame) {
		return nil, false, fmt.Errorf("native message is not valid JSON")
	}
	var envelope chunkEnvelope
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return nil, false, err
	}
	if envelope.Chunk == nil {
		return append([]byte(nil), frame...), true, nil
	}
	c := envelope.Chunk
	if c.Version != 1 || c.ID == "" || c.Total < 1 || c.Total > 4096 || c.Index < 0 || c.Index >= c.Total || c.SHA256 == "" {
		return nil, false, fmt.Errorf("invalid native message chunk metadata")
	}
	part, err := base64.StdEncoding.DecodeString(c.Data)
	if err != nil {
		return nil, false, fmt.Errorf("decode native message chunk: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for id, item := range a.items {
		if now.Sub(item.updatedAt) > time.Minute {
			delete(a.items, id)
		}
	}
	item := a.items[c.ID]
	if item == nil {
		item = &assembly{total: c.Total, sha256: c.SHA256, parts: make(map[int][]byte), updatedAt: now}
		a.items[c.ID] = item
	}
	if item.total != c.Total || item.sha256 != c.SHA256 {
		delete(a.items, c.ID)
		return nil, false, fmt.Errorf("inconsistent native message chunks")
	}
	if _, exists := item.parts[c.Index]; !exists {
		item.parts[c.Index] = part
		item.size += len(part)
	}
	item.updatedAt = now
	if item.size > MaxAssembledSize {
		delete(a.items, c.ID)
		return nil, false, fmt.Errorf("reassembled native message exceeds %d bytes", MaxAssembledSize)
	}
	if len(item.parts) != item.total {
		return nil, false, nil
	}
	result := make([]byte, 0, item.size)
	for index := 0; index < item.total; index++ {
		part, ok := item.parts[index]
		if !ok {
			return nil, false, nil
		}
		result = append(result, part...)
	}
	delete(a.items, c.ID)
	hash := sha256.Sum256(result)
	if hex.EncodeToString(hash[:]) != c.SHA256 {
		return nil, false, fmt.Errorf("native message chunk checksum mismatch")
	}
	if !json.Valid(result) {
		return nil, false, fmt.Errorf("reassembled native message is not valid JSON")
	}
	return result, true, nil
}

func Encode(raw []byte, chunkSize int) ([][]byte, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("outbound native message is not valid JSON")
	}
	if len(raw) <= MaxHostToChrome {
		return [][]byte{append([]byte(nil), raw...)}, nil
	}
	if chunkSize <= 0 || chunkSize > DefaultChunkSize {
		chunkSize = DefaultChunkSize
	}
	hash := sha256.Sum256(raw)
	hashText := hex.EncodeToString(hash[:])
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, fmt.Errorf("create chunk id: %w", err)
	}
	id := hex.EncodeToString(idBytes)
	total := (len(raw) + chunkSize - 1) / chunkSize
	frames := make([][]byte, 0, total)
	for index := 0; index < total; index++ {
		start := index * chunkSize
		end := start + chunkSize
		if end > len(raw) {
			end = len(raw)
		}
		envelope := chunkEnvelope{Chunk: &chunk{Version: 1, ID: id, Index: index, Total: total, SHA256: hashText, Data: base64.StdEncoding.EncodeToString(raw[start:end])}}
		frame, err := json.Marshal(envelope)
		if err != nil {
			return nil, err
		}
		if len(frame) > MaxHostToChrome {
			return nil, fmt.Errorf("encoded chunk exceeds native messaging limit")
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

func ReadFrame(reader io.Reader, maxBytes uint32) ([]byte, error) {
	var length uint32
	if err := binary.Read(reader, binary.LittleEndian, &length); err != nil {
		return nil, err
	}
	if length == 0 || length > maxBytes {
		return nil, fmt.Errorf("native message length %d is outside allowed range", length)
	}
	frame := make([]byte, length)
	if _, err := io.ReadFull(reader, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func WriteFrame(writer io.Writer, frame []byte) error {
	if len(frame) == 0 || len(frame) > MaxHostToChrome {
		return fmt.Errorf("native message size %d is outside allowed range", len(frame))
	}
	if err := binary.Write(writer, binary.LittleEndian, uint32(len(frame))); err != nil {
		return err
	}
	_, err := writer.Write(frame)
	return err
}

func Relay(ctx context.Context, chromeIn io.Reader, chromeOut io.Writer, daemon net.Conn, chunkSize int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() {
		assembler := NewAssembler()
		writer := bufio.NewWriter(daemon)
		for {
			frame, err := ReadFrame(chromeIn, MaxChromeToHost)
			if err != nil {
				errCh <- err
				return
			}
			message, complete, err := assembler.Accept(frame)
			if err != nil {
				errCh <- err
				return
			}
			if !complete {
				continue
			}
			if _, err := writer.Write(message); err != nil {
				errCh <- err
				return
			}
			if err := writer.WriteByte('\n'); err != nil {
				errCh <- err
				return
			}
			if err := writer.Flush(); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		decoder := json.NewDecoder(daemon)
		for {
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				errCh <- err
				return
			}
			frames, err := Encode(raw, chunkSize)
			if err != nil {
				errCh <- err
				return
			}
			for _, frame := range frames {
				if err := WriteFrame(chromeOut, frame); err != nil {
					errCh <- err
					return
				}
			}
		}
	}()
	select {
	case <-ctx.Done():
		_ = daemon.Close()
		return ctx.Err()
	case err := <-errCh:
		_ = daemon.Close()
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}
