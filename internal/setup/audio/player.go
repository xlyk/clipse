package audio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
)

var ErrNoPlayer = errors.New("no supported host audio player found")

type Options struct {
	LookPath func(string) (string, error)
	Run      func(context.Context, string, ...string) error
}

// Player owns one cancel-safe looping host audio process. Playback is an
// optional UI effect: failures are returned to the wizard but never affect
// configuration readiness.
type Player struct {
	mu       sync.Mutex
	lookPath func(string) (string, error)
	run      func(context.Context, string, ...string) error
	cancel   context.CancelFunc
	done     chan struct{}
	active   bool
	tempPath string
}

func New(opts Options) *Player {
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	run := opts.Run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run()
		}
	}
	return &Player{lookPath: lookPath, run: run}
}

func (p *Player) Start(parent context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active {
		return nil
	}
	player, err := p.findPlayer()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "clipse-config-synth-*.wav")
	if err != nil {
		return fmt.Errorf("creating soundtrack: %w", err)
	}
	path := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(path)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("protecting soundtrack: %w", err)
	}
	if _, err := tmp.Write(SynthWAV()); err != nil {
		cleanup()
		return fmt.Errorf("writing soundtrack: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("closing soundtrack: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	p.cancel = cancel
	p.done = done
	p.active = true
	p.tempPath = path
	go p.loop(ctx, player, path, done)
	return nil
}

func (p *Player) loop(ctx context.Context, player, path string, done chan struct{}) {
	defer close(done)
	defer os.Remove(path)
	for ctx.Err() == nil {
		if err := p.run(ctx, player, path); err != nil {
			break
		}
	}
	p.mu.Lock()
	p.active = false
	p.cancel = nil
	p.done = nil
	p.tempPath = ""
	p.mu.Unlock()
}

func (p *Player) Stop() error {
	p.mu.Lock()
	if !p.active {
		p.mu.Unlock()
		return nil
	}
	cancel := p.cancel
	done := p.done
	p.mu.Unlock()
	cancel()
	<-done
	return nil
}

func (p *Player) Active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

func (p *Player) findPlayer() (string, error) {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{"afplay"}
	case "linux":
		candidates = []string{"pw-play", "paplay", "aplay"}
	default:
		return "", ErrNoPlayer
	}
	for _, candidate := range candidates {
		path, err := p.lookPath(candidate)
		if err == nil {
			return path, nil
		}
	}
	return "", ErrNoPlayer
}
