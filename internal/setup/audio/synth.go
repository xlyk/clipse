// Package audio provides the wizard's optional, non-blocking soundtrack.
// The track is synthesized at runtime so no licensed audio asset is shipped.
package audio

import (
	"bytes"
	"encoding/binary"
	"math"
)

const (
	sampleRate      = 44100
	SoundtrackBPM   = 174
	SoundtrackBeats = 32
)

var (
	// F-minor with a chromatic E natural at the turnaround. The dissonance is
	// deliberate: it makes the loop reset feel like a tracker pattern jump.
	acidNotes = [...]float64{
		87.31, 87.31, 103.83, 87.31, 116.54, 103.83, 77.78, 82.41,
		87.31, 130.81, 116.54, 103.83, 82.41, 87.31, 77.78, 103.83,
	}
	reeseRoots = [...]float64{43.65, 43.65, 51.91, 38.89, 43.65, 58.27, 41.20, 43.65}
	arpNotes   = [...]float64{349.23, 415.30, 523.25, 622.25, 698.46, 523.25, 830.61, 622.25}
)

// SynthWAV returns an original deterministic gabber/DnB cracktro loop as
// stereo 16-bit PCM WAV. Eight bars move through boot-up, breakbeat pressure,
// an acid rise, a short fake-out, and a three-bar industrial drop. Every sound
// is generated below: no sample, recording, or licensed asset is embedded.
func SynthWAV() []byte {
	beatSeconds := 60.0 / float64(SoundtrackBPM)
	stepSeconds := beatSeconds / 4
	duration := beatSeconds * SoundtrackBeats
	frameCount := int(duration * sampleRate)
	pcm := make([]int16, frameCount*2)

	// Stateful filters keep the acid and reese layers muscular instead of
	// alias-heavy. Their inputs remain deterministic oscillators.
	var acidLow, acidBand float64
	var reeseLeftLow, reeseRightLow float64
	noise := uint32(0xC11F5EED)
	previousNoise := 0.0
	previousHighNoise := 0.0

	for frame := 0; frame < frameCount; frame++ {
		t := float64(frame) / sampleRate
		beatPosition := t / beatSeconds
		beatIndex := int(beatPosition)
		beatPhase := t - float64(beatIndex)*beatSeconds
		bar := minInt(beatIndex/4, 7)
		beatInBar := beatIndex % 4
		stepIndex := int(t / stepSeconds)
		stepInBar := stepIndex % 16
		stepPhase := t - float64(stepIndex)*stepSeconds

		noise = xorshift32(noise)
		noiseSample := float64(int32(noise)) / float64(math.MaxInt32)
		highNoise := noiseSample - 0.86*previousNoise
		metalNoise := highNoise - 0.78*previousHighNoise
		previousNoise = noiseSample
		previousHighNoise = highNoise

		// The spine is a phase-correct 174 BPM gabber kick: a 250 Hz pitch
		// dive, clipped 49 Hz tail, hard transient, and a short distorted
		// room return. It intentionally leaves essentially no empty beat.
		kick := gabberKick(beatPhase, highNoise)
		ghostKick := 0.0
		if ghostKickStep(bar, stepInBar) {
			ghostKick = gabberKick(stepPhase, highNoise) * 0.50
		}
		rumblePhase := beatPhase - 0.042
		rumble := 0.0
		if rumblePhase > 0 {
			rumble = math.Tanh(2.4*math.Sin(2*math.Pi*45.5*rumblePhase)) *
				math.Exp(-3.7*rumblePhase) * 0.22
		}

		// Reverse-bass pressure blooms on the eighth-note offbeat. It is
		// center-weighted so the low end survives mono terminal speakers.
		reversePhase := beatPhase - beatSeconds/2
		reverseBass := 0.0
		if reversePhase >= 0 {
			reverseEnvelope := math.Sin(math.Pi * minFloat(reversePhase/(beatSeconds/2), 1))
			reverseOscillator := 0.72*saw(reversePhase*52.0) +
				0.28*math.Sin(2*math.Pi*52.0*reversePhase)
			reverseBass = math.Tanh(2.8*reverseOscillator) * reverseEnvelope * 0.25
		}

		// The acid line retriggers tracker-style on every sixteenth. A moving
		// state-variable filter supplies the resonant vowel; sidechain makes
		// room for the kick without turning the bass polite.
		acidFrequency := acidNotes[stepInBar]
		acidOsc := 0.72*saw(stepPhase*acidFrequency) +
			0.28*pulse(stepPhase*acidFrequency*0.501, 0.38)
		cutoffSweep := 0.5 + 0.5*math.Sin(2*math.Pi*(0.31*t+0.07*float64(bar)))
		acidCutoff := 620.0 + 3800.0*cutoffSweep*cutoffSweep
		acidCoefficient := 2 * math.Sin(math.Pi*acidCutoff/sampleRate)
		acidHigh := acidOsc - acidLow - 0.34*acidBand
		acidBand += acidCoefficient * acidHigh
		acidLow += acidCoefficient * acidBand
		acidGate := math.Exp(-3.5 * stepPhase)
		sidechain := minFloat(1, 0.04+beatPhase/0.140)
		acidAmount := [...]float64{0.12, 0.38, 0.64, 0.82, 0.32, 1.00, 1.08, 0.96}[bar]
		acid := math.Tanh((acidLow+0.92*acidBand)*2.1) * acidGate * sidechain * 0.33 * acidAmount

		// Two differently detuned saw clusters form a stereo reese. A clean
		// sine remains centered below them, while slow opposing movement gives
		// the mid-bass the nauseous width expected from dark DnB.
		reeseFrequency := reeseRoots[bar]
		reeseLeftInput := 0.62*saw(t*reeseFrequency*1.006) + 0.38*saw(t*reeseFrequency*2.013)
		reeseRightInput := 0.62*saw(t*reeseFrequency*0.994) + 0.38*saw(t*reeseFrequency*1.987)
		reeseCoefficient := 1 - math.Exp(-2*math.Pi*(520+180*math.Sin(2*math.Pi*0.23*t))/sampleRate)
		reeseLeftLow += reeseCoefficient * (reeseLeftInput - reeseLeftLow)
		reeseRightLow += reeseCoefficient * (reeseRightInput - reeseRightLow)
		reeseAmount := [...]float64{0.00, 0.18, 0.44, 0.55, 0.20, 0.76, 0.84, 0.88}[bar]
		reeseSub := math.Sin(2*math.Pi*reeseFrequency*t) * sidechain * reeseAmount * 0.20
		reeseLeft := math.Tanh(reeseLeftLow*3.0) * sidechain * reeseAmount * 0.24
		reeseRight := math.Tanh(reeseRightLow*3.0) * sidechain * reeseAmount * 0.24

		// Sixteenth hats, offbeat open hats, and a metallic drop ride create
		// the upper wall. Alternating pans mimic classic tracker channel
		// placement rather than realistic drums.
		hatPan := -0.72
		if stepIndex%2 == 1 {
			hatPan = 0.72
		}
		hatAccent := 0.17
		if stepInBar%4 == 2 {
			hatAccent = 0.30
		}
		closedHat := metalNoise * math.Exp(-170*stepPhase) * hatAccent
		openHat := 0.0
		if stepInBar%4 == 2 {
			openHat = highNoise * math.Exp(-26*stepPhase) * 0.16
		}
		rideLeft, rideRight := 0.0, 0.0
		if bar >= 5 && stepInBar%2 == 0 {
			rideEnvelope := math.Exp(-31 * stepPhase)
			rideLeft = (0.55*metalNoise + 0.45*math.Sin(2*math.Pi*6917*stepPhase)) * rideEnvelope * 0.10
			rideRight = (0.55*metalNoise + 0.45*math.Sin(2*math.Pi*7349*stepPhase)) * rideEnvelope * 0.10
		}

		// The backbeat is deliberately hybrid: hardstyle claps on two/four,
		// DnB ghost snares between them, then 32nd-note rolls in rise bars.
		snare, clap := 0.0, 0.0
		if stepInBar == 4 || stepInBar == 12 || ghostSnareStep(bar, stepInBar) {
			snareScale := 1.0
			if stepInBar != 4 && stepInBar != 12 {
				snareScale = 0.46
			}
			snare = (0.48*highNoise*math.Exp(-24*stepPhase) +
				0.28*math.Sin(2*math.Pi*188*stepPhase)*math.Exp(-17*stepPhase)) * snareScale
			for _, delay := range [...]float64{0, 0.013, 0.029} {
				phase := stepPhase - delay
				if phase >= 0 {
					clap += highNoise * math.Exp(-37*phase) * 0.105 * snareScale
				}
			}
		}
		if (bar == 3 || bar == 7) && beatInBar == 3 {
			halfStep := stepSeconds / 2
			rollPhase := math.Mod(stepPhase, halfStep)
			snare += highNoise * math.Exp(-70*rollPhase) * (0.10 + 0.045*float64(stepInBar-12))
		}

		// Minor rave stabs and a square-edged cracktro arpeggio supply the
		// recognisable hacker-music hook. The chord rotates in stereo; the arp
		// answers it from the opposite side during the high-energy bars.
		stabLeft, stabRight := 0.0, 0.0
		if raveStabStep(bar, stepInBar) {
			stabEnvelope := math.Exp(-18 * stepPhase)
			stab := minorStab(stepPhase, 174.61) * stabEnvelope * 0.20
			stabPan := 0.78 * math.Sin(2*math.Pi*(0.18*t+float64(bar)*0.13))
			stabLeft = stab * (1 - stabPan)
			stabRight = stab * (1 + stabPan)
		}
		arpLeft, arpRight := 0.0, 0.0
		if bar >= 2 {
			arpFrequency := arpNotes[stepInBar%len(arpNotes)]
			arpEnvelope := math.Exp(-24 * stepPhase)
			arp := math.Tanh((0.72*pulse(stepPhase*arpFrequency, 0.31)+
				0.28*saw(stepPhase*arpFrequency*2.003))*2.2) * arpEnvelope * 0.12
			arpPan := 0.85 * math.Sin(2*math.Pi*(0.47*t)+float64(stepInBar))
			arpLeft = arp * (1 - arpPan)
			arpRight = arp * (1 + arpPan)
		}

		// End-of-phrase data corruption: bit-rate gating, a rising noise wall,
		// and an FM modem shriek. The final bar grows progressively less sane
		// before cutting cleanly back to the first kick.
		glitchLeft, glitchRight := 0.0, 0.0
		if bar == 3 || bar == 7 {
			barProgress := (float64(beatInBar) + beatPhase/beatSeconds) / 4
			riser := highNoise * barProgress * barProgress * 0.12
			glitchLeft += riser
			glitchRight -= riser * 0.72
			if beatInBar == 3 {
				thirtySecond := stepSeconds / 2
				slice := int(stepPhase / thirtySecond)
				gate := 1.0
				if (stepInBar+slice)%3 == 1 {
					gate = 0.0
				}
				shriekFrequency := 720.0 + 1900.0*(beatPhase/beatSeconds)
				shriek := math.Sin(2*math.Pi*shriekFrequency*beatPhase+
					4.8*math.Sin(2*math.Pi*93*beatPhase)) * 0.15 * gate
				glitchLeft += shriek
				glitchRight -= shriek * 0.88
			}
		}

		// A broad dynamic arc is applied before the master saturator. The fake
		// breakdown still pounds, but the last three bars visibly hit harder.
		intensity := [...]float64{0.76, 0.84, 0.94, 1.04, 0.78, 1.16, 1.22, 1.30}[bar]
		center := 0.90*kick + ghostKick + rumble + reverseBass + acid + reeseSub + snare + clap
		hat := closedHat + openHat
		left := intensity * (center + reeseLeft + hat*(1-hatPan)*0.70 + rideLeft + stabLeft + arpLeft + glitchLeft)
		right := intensity * (center + reeseRight + hat*(1+hatPan)*0.70 + rideRight + stabRight + arpRight + glitchRight)
		pcm[frame*2] = int16(masterClip(left) * 32767)
		pcm[frame*2+1] = int16(masterClip(right) * 32767)
	}

	return encodeWAV(pcm)
}

func gabberKick(phase, noise float64) float64 {
	// Integral of 49 + 250*exp(-42t), keeping the pitch drop phase-continuous.
	cycles := 49*phase + (250.0/42.0)*(1-math.Exp(-42*phase))
	punch := math.Sin(2*math.Pi*cycles) * math.Exp(-8.4*phase)
	tail := math.Sin(2*math.Pi*49*phase+0.52*math.Sin(2*math.Pi*98*phase)) * math.Exp(-3.1*phase)
	body := math.Tanh((1.42*punch+0.86*tail)*3.4) * math.Exp(-4.4*phase)
	attack := 1 + 1.40*math.Exp(-35*phase)
	click := 0.0
	if phase < 0.018 {
		click = noise * math.Exp(-210*phase) * 0.44
	}
	return body*attack*0.72 + click
}

func ghostKickStep(bar, step int) bool {
	if bar < 2 {
		return false
	}
	switch step {
	case 3, 10:
		return bar == 2 || bar >= 5
	case 15:
		return bar == 3 || bar == 6 || bar == 7
	default:
		return false
	}
}

func ghostSnareStep(bar, step int) bool {
	if bar < 2 {
		return false
	}
	return step == 7 || step == 10 || (bar >= 5 && step == 15)
}

func raveStabStep(bar, step int) bool {
	if bar == 0 || bar == 4 {
		return step == 0 || step == 10
	}
	switch step {
	case 0, 3, 6, 10, 14:
		return true
	default:
		return false
	}
}

func minorStab(phase, root float64) float64 {
	minorThird := root * math.Pow(2, 3.0/12.0)
	fifth := root * math.Pow(2, 7.0/12.0)
	return 0.38*saw(phase*root) + 0.31*saw(phase*minorThird*1.004) + 0.31*saw(phase*fifth*0.997)
}

func saw(cycles float64) float64 {
	return 2*(cycles-math.Floor(cycles)) - 1
}

func pulse(cycles, width float64) float64 {
	if cycles-math.Floor(cycles) < width {
		return 1
	}
	return -1
}

func masterClip(sample float64) float64 {
	// Two nonlinear stages mimic an overdriven mixer into a brickwall while
	// mathematically keeping the PCM peak below the test's 31k safety limit.
	driven := math.Tanh(sample * 1.92)
	return math.Tanh(driven*1.18) * 0.93
}

func xorshift32(value uint32) uint32 {
	value ^= value << 13
	value ^= value >> 17
	value ^= value << 5
	return value
}

func encodeWAV(pcm []int16) []byte {
	var out bytes.Buffer
	dataBytes := uint32(len(pcm) * 2)
	out.WriteString("RIFF")
	writeLE(&out, uint32(36)+dataBytes)
	out.WriteString("WAVEfmt ")
	writeLE(&out, uint32(16))
	writeLE(&out, uint16(1))
	writeLE(&out, uint16(2))
	writeLE(&out, uint32(sampleRate))
	writeLE(&out, uint32(sampleRate*4))
	writeLE(&out, uint16(4))
	writeLE(&out, uint16(16))
	out.WriteString("data")
	writeLE(&out, dataBytes)
	for _, sample := range pcm {
		writeLE(&out, sample)
	}
	return out.Bytes()
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func writeLE(out *bytes.Buffer, value any) {
	_ = binary.Write(out, binary.LittleEndian, value)
}
