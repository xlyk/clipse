package audio_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/xlyk/clipse/internal/setup/audio"
)

func TestSynthWAVIsDeterministicPCM(t *testing.T) {
	first := audio.SynthWAV()
	second := audio.SynthWAV()
	if !bytes.Equal(first, second) {
		t.Fatal("SynthWAV is not deterministic")
	}
	if len(first) < 44 || string(first[:4]) != "RIFF" || string(first[8:12]) != "WAVE" {
		t.Fatalf("invalid WAV header: %q", first[:min(len(first), 12)])
	}
	if got := binary.LittleEndian.Uint16(first[22:24]); got != 1 {
		t.Errorf("channels = %d, want mono", got)
	}
	if got := binary.LittleEndian.Uint32(first[24:28]); got != 22050 {
		t.Errorf("sample rate = %d, want 22050", got)
	}
	for offset := 44; offset+2 <= len(first); offset += 2 {
		sample := int16(binary.LittleEndian.Uint16(first[offset : offset+2]))
		if sample > 30000 || sample < -30000 {
			t.Fatalf("sample clips at offset %d: %d", offset, sample)
		}
	}
}

func TestPlayerMissingBackendIsNonFatal(t *testing.T) {
	player := audio.New(audio.Options{LookPath: func(string) (string, error) {
		return "", errors.New("missing")
	}})
	if err := player.Start(t.Context()); !errors.Is(err, audio.ErrNoPlayer) {
		t.Fatalf("Start error = %v, want ErrNoPlayer", err)
	}
	if player.Active() {
		t.Fatal("player is active without a backend")
	}
}

func TestPlayerStopsBackgroundLoop(t *testing.T) {
	started := make(chan struct{})
	player := audio.New(audio.Options{
		LookPath: func(string) (string, error) { return "/fake/player", nil },
		Run: func(ctx context.Context, _ string, _ ...string) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err := player.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-started
	if !player.Active() {
		t.Fatal("player is not active after backend start")
	}
	if err := player.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if player.Active() {
		t.Fatal("player remains active after Stop")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
