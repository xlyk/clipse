// Package audio provides the wizard's optional, non-blocking soundtrack.
// The track is synthesized at runtime so no licensed audio asset is shipped.
package audio

import (
	"bytes"
	"encoding/binary"
	"math"
)

const sampleRate = 22050

// SynthWAV returns an original deterministic tracker-style techno loop as
// mono 16-bit PCM WAV: 16 beats at 125 BPM (about 7.68 seconds).
func SynthWAV() []byte {
	const bpm = 125.0
	beatSeconds := 60.0 / bpm
	duration := beatSeconds * 16
	sampleCount := int(duration * sampleRate)
	pcm := make([]int16, sampleCount)
	notes := []float64{55.00, 65.41, 73.42, 82.41, 55.00, 73.42, 65.41, 49.00}
	noise := uint32(0xC11F5EED)

	for i := range pcm {
		t := float64(i) / sampleRate
		beatPhase := math.Mod(t, beatSeconds)
		eighth := beatSeconds / 2
		eighthPhase := math.Mod(t, eighth)

		// Four-on-the-floor kick with a fast descending pitch and exponential
		// decay. Keeping it synthesized makes the loop small and license-free.
		kick := 0.0
		if beatPhase < 0.18 {
			freq := 95.0 - 240.0*beatPhase
			if freq < 42 {
				freq = 42
			}
			kick = math.Sin(2*math.Pi*freq*beatPhase) * math.Exp(-22*beatPhase) * 0.58
		}

		step := int(t/eighth) % len(notes)
		bassPhase := math.Mod(t, eighth)
		bassEnv := math.Exp(-3.2 * bassPhase)
		bassSine := math.Sin(2 * math.Pi * notes[step] * t)
		bassSquare := 1.0
		if bassSine < 0 {
			bassSquare = -1
		}
		bass := (0.18*bassSine + 0.07*bassSquare) * bassEnv

		// Deterministic LCG noise produces a short off-beat hi-hat.
		noise = 1664525*noise + 1013904223
		noiseSample := (float64((noise>>16)&0xffff)/32767.5 - 1)
		hat := 0.0
		if eighthPhase < 0.035 && beatPhase >= eighth-0.001 {
			hat = noiseSample * math.Exp(-95*eighthPhase) * 0.16
		}

		sample := kick + bass + hat
		if sample > 0.9 {
			sample = 0.9
		} else if sample < -0.9 {
			sample = -0.9
		}
		pcm[i] = int16(sample * 32767)
	}

	var out bytes.Buffer
	dataBytes := uint32(len(pcm) * 2)
	out.WriteString("RIFF")
	binary.Write(&out, binary.LittleEndian, uint32(36)+dataBytes)
	out.WriteString("WAVEfmt ")
	binary.Write(&out, binary.LittleEndian, uint32(16))
	binary.Write(&out, binary.LittleEndian, uint16(1))
	binary.Write(&out, binary.LittleEndian, uint16(1))
	binary.Write(&out, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&out, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(&out, binary.LittleEndian, uint16(2))
	binary.Write(&out, binary.LittleEndian, uint16(16))
	out.WriteString("data")
	binary.Write(&out, binary.LittleEndian, dataBytes)
	for _, sample := range pcm {
		binary.Write(&out, binary.LittleEndian, sample)
	}
	return out.Bytes()
}
