// Wake-word detection via local Vosk speech recognition.
//
// The DTW approach (wake_dtw) cannot reliably separate "Барон" from
// similar-sounding phrases ("Лето в разгаре", "Да здравствуйте") using
// spectral features alone. Vosk is a real local speech recognizer: it
// listens to the raw PCM stream and returns recognized words, so a wake
// triggers only when "барон" is actually spoken. It runs ~5x faster than
// real time on CPU and needs no cloud calls while asleep.
//
// Implementation: a persistent Python subprocess reads 16-bit mono PCM
// (48kHz) from stdin and writes recognized text lines to stdout. The Go
// side feeds every decoded frame and checks the returned text for the
// wake word.
package voicekit

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// WakeVosk wraps a persistent Vosk recognizer subprocess.
type WakeVosk struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	mu     sync.Mutex
	word   string // wake word to look for (lowercase)
	frames chan []byte
	hitCh  chan struct{}
}

// voskPython is the python interpreter used to launch the recognizer.
const voskPython = "python3"

// voskScript is the inline Python that reads PCM from stdin and emits
// recognized text lines (one per utterance) to stdout.
const voskScript = `
import sys, json
sys.path.insert(0, '/home/kirill/.local/lib/python3.14/site-packages')
from vosk import Model, KaldiRecognizer

model = Model("/home/kirill/models/vosk-model-small-ru-0.22")
rec = KaldiRecognizer(model, 48000)

while True:
    chunk = sys.stdin.buffer.read(960 * 2)  # 20ms frame, 16-bit mono
    if not chunk:
        break
    if rec.AcceptWaveform(chunk):
        res = json.loads(rec.FinalResult())
        text = res.get("text", "")
        if text:
            print(text, flush=True)
`

// WakeTimeoutSec returns the active-mode inactivity timeout in seconds
// (env GOV_WAKE_TIMEOUT, default 45).
func WakeTimeoutSec() int {
	v := strings.TrimSpace(os.Getenv("GOV_WAKE_TIMEOUT"))
	if v == "" {
		return 45
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 45
	}
	return n
}

// NewWakeVosk starts the Vosk recognizer subprocess.
func NewWakeVosk(word string) (*WakeVosk, error) {
	w := &WakeVosk{
		word:   strings.ToLower(strings.TrimSpace(word)),
		frames: make(chan []byte, 256),
		hitCh:  make(chan struct{}, 1),
	}
	if w.word == "" {
		w.word = "барон"
	}
	cmd := exec.Command(voskPython, "-c", voskScript)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("vosk start: %w", err)
	}
	w.cmd = cmd
	w.stdin = stdin
	w.stdout = bufio.NewScanner(stdout)
	go w.writeLoop()
	go w.run()
	return w, nil
}

// Feed sends a 20ms PCM frame (960 samples, 16-bit mono) to Vosk. It never
// blocks: the frame goes into a buffered channel, a writer goroutine sends
// it to the subprocess, and recognized text is collected asynchronously.
func (w *WakeVosk) Feed(frame []int16) bool {
	if len(frame) != 960 {
		return false
	}
	buf := make([]byte, len(frame)*2)
	for i, s := range frame {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	select {
	case w.frames <- buf:
	default:
		// Pipe is full — drop (Vosk is momentarily slower than real time).
	}
	select {
	case <-w.hitCh:
		return true
	default:
		return false
	}
}

// writeLoop forwards buffered frames to the Vosk subprocess stdin.
func (w *WakeVosk) writeLoop() {
	for buf := range w.frames {
		w.mu.Lock()
		_, _ = w.stdin.Write(buf)
		w.mu.Unlock()
	}
}

// run reads recognized text lines from Vosk and reports the wake word.
func (w *WakeVosk) run() {
	for w.stdout.Scan() {
		text := strings.ToLower(strings.TrimSpace(w.stdout.Text()))
		if strings.Contains(text, w.word) {
			select {
			case w.hitCh <- struct{}{}:
			default:
			}
		}
	}
}

// Close stops the subprocess.
func (w *WakeVosk) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd != nil && w.cmd.Process != nil {
		w.cmd.Process.Kill()
		w.cmd.Wait()
	}
}
