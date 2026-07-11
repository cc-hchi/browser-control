package nativemsg

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTripUsesLittleEndianLengthPrefix(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	var wire bytes.Buffer
	if err := WriteFrame(&wire, frame); err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}

	encoded := wire.Bytes()
	if got := binary.LittleEndian.Uint32(encoded[:4]); got != uint32(len(frame)) {
		t.Fatalf("length prefix = %d, want %d", got, len(frame))
	}
	if !bytes.Equal(encoded[4:], frame) {
		t.Fatalf("payload = %q, want %q", encoded[4:], frame)
	}

	got, err := ReadFrame(&wire, MaxChromeToHost)
	if err != nil {
		t.Fatalf("ReadFrame() error = %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("ReadFrame() = %q, want %q", got, frame)
	}
}

func TestFrameLimitsAndTruncation(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
		max  uint32
		want string
	}{
		{name: "zero length", wire: []byte{0, 0, 0, 0}, max: 10, want: "outside allowed range"},
		{name: "over configured maximum", wire: []byte{11, 0, 0, 0}, max: 10, want: "outside allowed range"},
		{name: "truncated payload", wire: []byte{3, 0, 0, 0, 'a'}, max: 10, want: io.ErrUnexpectedEOF.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(tt.wire), tt.max)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ReadFrame() error = %v, want containing %q", err, tt.want)
			}
		})
	}

	if err := WriteFrame(io.Discard, nil); err == nil {
		t.Fatal("WriteFrame(nil) unexpectedly succeeded")
	}
	if err := WriteFrame(io.Discard, make([]byte, MaxHostToChrome+1)); err == nil {
		t.Fatal("WriteFrame(over limit) unexpectedly succeeded")
	}
}

func TestEncodeSmallMessageReturnsIndependentFrame(t *testing.T) {
	raw := []byte(`{"ok":true}`)
	frames, err := Encode(raw, 8)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if len(frames) != 1 || !bytes.Equal(frames[0], raw) {
		t.Fatalf("Encode() = %q, want one unwrapped frame", frames)
	}
	frames[0][0] = 'x'
	if raw[0] != '{' {
		t.Fatal("Encode() returned a frame aliasing caller-owned input")
	}
}

func TestEncodeAndAssembleLargeMessageOutOfOrder(t *testing.T) {
	raw := largeJSON(MaxHostToChrome + 64*1024)
	frames, err := Encode(raw, 128*1024)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if len(frames) < 2 {
		t.Fatalf("Encode() produced %d frame(s), want chunking", len(frames))
	}
	for index, frame := range frames {
		if len(frame) > MaxHostToChrome {
			t.Fatalf("frame %d size = %d, exceeds %d", index, len(frame), MaxHostToChrome)
		}
	}

	assembler := NewAssembler()
	order := make([]int, 0, len(frames))
	for index := len(frames) - 1; index >= 0; index-- {
		order = append(order, index)
	}
	for position, index := range order {
		got, complete, err := assembler.Accept(frames[index])
		if err != nil {
			t.Fatalf("Accept(frame %d) error = %v", index, err)
		}
		if position < len(order)-1 {
			if complete || got != nil {
				t.Fatalf("Accept(frame %d) completed before all chunks arrived", index)
			}
			continue
		}
		if !complete || !bytes.Equal(got, raw) {
			t.Fatalf("final Accept() complete = %v, payload matches = %v", complete, bytes.Equal(got, raw))
		}
	}
}

func TestAssemblerRejectsChecksumMismatchAndDiscardsAssembly(t *testing.T) {
	raw := largeJSON(MaxHostToChrome + 1)
	frames, err := Encode(raw, DefaultChunkSize)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	var first chunkEnvelope
	if err := json.Unmarshal(frames[0], &first); err != nil {
		t.Fatalf("unmarshal first chunk: %v", err)
	}
	data, err := base64.StdEncoding.DecodeString(first.Chunk.Data)
	if err != nil {
		t.Fatalf("decode first chunk: %v", err)
	}
	data[0] ^= 1
	first.Chunk.Data = base64.StdEncoding.EncodeToString(data)
	frames[0], err = json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal tampered chunk: %v", err)
	}

	assembler := NewAssembler()
	for index, frame := range frames {
		_, complete, acceptErr := assembler.Accept(frame)
		if index < len(frames)-1 {
			if acceptErr != nil || complete {
				t.Fatalf("Accept(frame %d) = complete %v, error %v", index, complete, acceptErr)
			}
			continue
		}
		if acceptErr == nil || !strings.Contains(acceptErr.Error(), "checksum mismatch") {
			t.Fatalf("final Accept() error = %v, want checksum mismatch", acceptErr)
		}
	}

	// A failed assembly must not poison a later message with the same ID.
	validFrames := chunkFramesWithID(t, raw, first.Chunk.ID, DefaultChunkSize)
	for index, frame := range validFrames {
		got, complete, acceptErr := assembler.Accept(frame)
		if acceptErr != nil {
			t.Fatalf("retry Accept(frame %d) error = %v", index, acceptErr)
		}
		if index == len(validFrames)-1 && (!complete || !bytes.Equal(got, raw)) {
			t.Fatalf("retry assembly did not complete correctly")
		}
	}
}

func TestAssemblerRejectsMalformedAndInconsistentChunks(t *testing.T) {
	assembler := NewAssembler()
	if _, _, err := assembler.Accept([]byte("not-json")); err == nil {
		t.Fatal("Accept(invalid JSON) unexpectedly succeeded")
	}

	hash := sha256.Sum256([]byte(`{"ok":true}`))
	hashText := hex.EncodeToString(hash[:])
	invalid := []chunk{
		{Version: 2, ID: "id", Index: 0, Total: 1, SHA256: hashText, Data: "e30="},
		{Version: 1, ID: "id", Index: 0, Total: 4097, SHA256: hashText, Data: "e30="},
		{Version: 1, ID: "id", Index: 1, Total: 1, SHA256: hashText, Data: "e30="},
		{Version: 1, ID: "id", Index: 0, Total: 1, SHA256: hashText, Data: "%%%"},
	}
	for index, value := range invalid {
		frame, err := json.Marshal(chunkEnvelope{Chunk: &value})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := assembler.Accept(frame); err == nil {
			t.Fatalf("Accept(invalid chunk %d) unexpectedly succeeded", index)
		}
	}

	first := chunk{Version: 1, ID: "same", Index: 0, Total: 2, SHA256: hashText, Data: "ew=="}
	frame, _ := json.Marshal(chunkEnvelope{Chunk: &first})
	if _, complete, err := assembler.Accept(frame); err != nil || complete {
		t.Fatalf("Accept(first chunk) = complete %v, error %v", complete, err)
	}
	second := first
	second.Index = 1
	second.Total = 3
	frame, _ = json.Marshal(chunkEnvelope{Chunk: &second})
	if _, _, err := assembler.Accept(frame); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("Accept(inconsistent chunk) error = %v", err)
	}
}

func TestEncodeRejectsInvalidJSON(t *testing.T) {
	if _, err := Encode([]byte("not-json"), DefaultChunkSize); err == nil {
		t.Fatal("Encode(invalid JSON) unexpectedly succeeded")
	}
}

func TestRelayReassemblesChromeInputAndChunksDaemonOutput(t *testing.T) {
	const chunkSize = 96 * 1024
	chromeMessage := largeJSON(MaxHostToChrome + 17*1024)
	daemonMessage := largeJSON(MaxHostToChrome + 33*1024)

	chromeInput, chromeWriter := io.Pipe()
	chromeOutput, relayChromeWriter := io.Pipe()
	relayDaemon, daemon := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- Relay(ctx, chromeInput, relayChromeWriter, relayDaemon, chunkSize)
	}()
	t.Cleanup(func() {
		_ = chromeWriter.Close()
		_ = chromeOutput.Close()
		_ = daemon.Close()
		_ = relayChromeWriter.Close()
	})

	inputFrames, err := Encode(chromeMessage, chunkSize)
	if err != nil {
		t.Fatalf("Encode(chrome input) error = %v", err)
	}
	inputWriteDone := make(chan error, 1)
	go func() {
		for _, frame := range inputFrames {
			if err := WriteFrame(chromeWriter, frame); err != nil {
				inputWriteDone <- err
				return
			}
		}
		inputWriteDone <- nil
	}()

	daemonDone := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(daemon).ReadBytes('\n')
		if err != nil {
			daemonDone <- err
			return
		}
		if !bytes.Equal(bytes.TrimSuffix(line, []byte{'\n'}), chromeMessage) {
			daemonDone <- errors.New("daemon received wrong reassembled message")
			return
		}
		if _, err := daemon.Write(append(append([]byte(nil), daemonMessage...), '\n')); err != nil {
			daemonDone <- err
			return
		}
		daemonDone <- nil
	}()

	assembler := NewAssembler()
	var got []byte
	for {
		frame, err := ReadFrame(chromeOutput, MaxHostToChrome)
		if err != nil {
			t.Fatalf("ReadFrame(relay output) error = %v", err)
		}
		var complete bool
		got, complete, err = assembler.Accept(frame)
		if err != nil {
			t.Fatalf("Accept(relay output) error = %v", err)
		}
		if complete {
			break
		}
	}
	if !bytes.Equal(got, daemonMessage) {
		t.Fatal("relay output did not match daemon message")
	}

	select {
	case err := <-inputWriteDone:
		if err != nil {
			t.Fatalf("write Chrome input: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out writing Chrome input")
	}
	select {
	case err := <-daemonDone:
		if err != nil {
			t.Fatalf("daemon exchange: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for daemon exchange")
	}

	cancel()
	select {
	case err := <-relayDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Relay() shutdown error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Relay() shutdown")
	}
}

func TestReadFrameReturnsUnderlyingEOF(t *testing.T) {
	_, err := ReadFrame(bytes.NewReader(nil), MaxChromeToHost)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrame(empty) error = %v, want EOF", err)
	}
}

func largeJSON(size int) []byte {
	if size < 2 {
		size = 2
	}
	return []byte(`"` + strings.Repeat("a", size-2) + `"`)
}

func chunkFramesWithID(t *testing.T, raw []byte, id string, chunkSize int) [][]byte {
	t.Helper()
	hash := sha256.Sum256(raw)
	hashText := hex.EncodeToString(hash[:])
	total := (len(raw) + chunkSize - 1) / chunkSize
	frames := make([][]byte, 0, total)
	for index := 0; index < total; index++ {
		start := index * chunkSize
		end := start + chunkSize
		if end > len(raw) {
			end = len(raw)
		}
		frame, err := json.Marshal(chunkEnvelope{Chunk: &chunk{
			Version: 1,
			ID:      id,
			Index:   index,
			Total:   total,
			SHA256:  hashText,
			Data:    base64.StdEncoding.EncodeToString(raw[start:end]),
		}})
		if err != nil {
			t.Fatalf("marshal chunk %d: %v", index, err)
		}
		frames = append(frames, frame)
	}
	return frames
}
