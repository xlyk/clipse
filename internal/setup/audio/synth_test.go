package audio_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
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
	if got := binary.LittleEndian.Uint16(first[22:24]); got != 2 {
		t.Errorf("channels = %d, want stereo", got)
	}
	if got := binary.LittleEndian.Uint32(first[24:28]); got != 44100 {
		t.Errorf("sample rate = %d, want 44100", got)
	}
	for offset := 44; offset+2 <= len(first); offset += 2 {
		sample := int16(binary.LittleEndian.Uint16(first[offset : offset+2]))
		if sample > 31000 || sample < -31000 {
			t.Fatalf("sample clips at offset %d: %d", offset, sample)
		}
	}
}

func TestSynthWAVHasHardTechnoShape(t *testing.T) {
	if audio.SoundtrackBPM < 170 || audio.SoundtrackBPM > 180 {
		t.Fatalf("SoundtrackBPM = %d, want DnB/hardstyle tempo in [170, 180]", audio.SoundtrackBPM)
	}
	wav := audio.SynthWAV()
	channels := int(binary.LittleEndian.Uint16(wav[22:24]))
	rate := int(binary.LittleEndian.Uint32(wav[24:28]))
	bits := int(binary.LittleEndian.Uint16(wav[34:36]))
	dataBytes := int(binary.LittleEndian.Uint32(wav[40:44]))
	duration := float64(dataBytes) / float64(rate*channels*(bits/8))
	wantDuration := 60.0 / float64(audio.SoundtrackBPM) * float64(audio.SoundtrackBeats)
	if delta := duration - wantDuration; delta < -0.02 || delta > 0.02 {
		t.Fatalf("duration = %.3fs, want %.3fs", duration, wantDuration)
	}

	var loudSamples int
	for offset := 44; offset+2 <= len(wav); offset += 2 {
		sample := int16(binary.LittleEndian.Uint16(wav[offset : offset+2]))
		if sample > 12000 || sample < -12000 {
			loudSamples++
		}
	}
	if ratio := float64(loudSamples) / float64((len(wav)-44)/2); ratio < 0.16 {
		t.Fatalf("high-energy sample ratio = %.3f, want at least 0.16", ratio)
	}
}

func TestSynthWAVHasArrangementAndStereoMotion(t *testing.T) {
	wav := audio.SynthWAV()
	left, right, rate := decodeStereo16(t, wav)
	framesPerBar := int((60.0 / float64(audio.SoundtrackBPM)) * 4 * float64(rate))
	barCount := min(len(left)/framesPerBar, audio.SoundtrackBeats/4)
	if barCount < 8 {
		t.Fatalf("bar count = %d, want at least 8", barCount)
	}

	barRMS := make([]float64, barCount)
	for bar := range barCount {
		start := bar * framesPerBar
		end := min(start+framesPerBar, len(left))
		barRMS[bar] = stereoRMS(left[start:end], right[start:end])
		if barRMS[bar] < 0.20 {
			t.Fatalf("bar %d RMS = %.3f, want a continuously forceful mix", bar, barRMS[bar])
		}
	}
	minRMS, maxRMS := barRMS[0], barRMS[0]
	for _, rms := range barRMS[1:] {
		minRMS = math.Min(minRMS, rms)
		maxRMS = math.Max(maxRMS, rms)
	}
	if contrast := maxRMS / minRMS; contrast < 1.08 {
		t.Fatalf("bar-level RMS contrast = %.3f, want an arranged rise/drop arc", contrast)
	}

	var stereoDifference, totalEnergy float64
	for i := range left {
		difference := left[i] - right[i]
		stereoDifference += difference * difference
		totalEnergy += left[i]*left[i] + right[i]*right[i]
	}
	if width := math.Sqrt(stereoDifference / totalEnergy); width < 0.18 {
		t.Fatalf("normalized stereo width = %.3f, want obvious cracktro motion", width)
	}
}

func TestSynthWAVHasAHeavyTransientEveryBeat(t *testing.T) {
	wav := audio.SynthWAV()
	left, right, rate := decodeStereo16(t, wav)
	framesPerBeat := int((60.0 / float64(audio.SoundtrackBPM)) * float64(rate))
	attackFrames := rate * 28 / 1000
	bodyStart := rate * 75 / 1000
	bodyEnd := rate * 125 / 1000
	heavyBeats := 0
	for beat := 0; beat < audio.SoundtrackBeats; beat++ {
		start := beat * framesPerBeat
		if start+bodyEnd > len(left) {
			break
		}
		attack := stereoRMS(left[start:start+attackFrames], right[start:start+attackFrames])
		body := stereoRMS(left[start+bodyStart:start+bodyEnd], right[start+bodyStart:start+bodyEnd])
		if attack > body*1.08 && attack > 0.34 {
			heavyBeats++
		}
	}
	if heavyBeats < audio.SoundtrackBeats-2 {
		t.Fatalf("heavy kick transients = %d/%d beats, want nearly every beat", heavyBeats, audio.SoundtrackBeats)
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

func decodeStereo16(t *testing.T, wav []byte) ([]float64, []float64, int) {
	t.Helper()
	if len(wav) < 44 {
		t.Fatal("WAV is shorter than its header")
	}
	if channels := binary.LittleEndian.Uint16(wav[22:24]); channels != 2 {
		t.Fatalf("channels = %d, want 2", channels)
	}
	rate := int(binary.LittleEndian.Uint32(wav[24:28]))
	frames := (len(wav) - 44) / 4
	left := make([]float64, frames)
	right := make([]float64, frames)
	for frame := 0; frame < frames; frame++ {
		offset := 44 + frame*4
		left[frame] = float64(int16(binary.LittleEndian.Uint16(wav[offset:offset+2]))) / 32768
		right[frame] = float64(int16(binary.LittleEndian.Uint16(wav[offset+2:offset+4]))) / 32768
	}
	return left, right, rate
}

func stereoRMS(left, right []float64) float64 {
	var sum float64
	for i := range left {
		sum += left[i]*left[i] + right[i]*right[i]
	}
	return math.Sqrt(sum / float64(len(left)*2))
}
