package voicekit

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kirill-scherba/opencode-client"
)

// opencodeBaseURL is the opencode-serve instance for voice sessions.
// Override with env BRAIN_URL (e.g. https://bmat.uk:7712 with auth) when
// the Govorilka server runs on a different host than opencode.
func brainBaseURL() string {
	if u := os.Getenv("BRAIN_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:7712"
}

// brainBasicAuth returns optional HTTP Basic Auth credentials for the
// brain endpoint (env BRAIN_USER / BRAIN_PASS) — used when Govorilka runs
// on a remote host and the opencode-serve endpoint is behind nginx auth.
func brainBasicAuth() (string, string) {
	u := os.Getenv("BRAIN_USER")
	p := os.Getenv("BRAIN_PASS")
	if u != "" {
		return u, p
	}
	return "", ""
}

var (
	brainMu   sync.Mutex
	brainCl   *opencodeclient.Client
	brainSess *opencodeclient.Session
)

// sessionFile stores the persistent voice-chat session state.
//
// The path must not depend on HOME being set: the RU govorilka service runs
// from systemd without HOME, and the old filepath.Join(os.Getenv("HOME"), ...)
// silently produced a RELATIVE path inside the working directory
// (/opt/.config/mcp-gateway/voice_session.json). Use GOV_STATE_DIR when set,
// otherwise $HOME/.config/mcp-gateway, otherwise /var/lib/govorilka.
var sessionFile = voiceSessionFile()

func voiceSessionFile() string {
	if d := os.Getenv("GOV_STATE_DIR"); d != "" {
		return filepath.Join(d, "voice_session.json")
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "mcp-gateway", "voice_session.json")
	}
	return "/var/lib/govorilka/voice_session.json"
}

// voiceSessionState is what we persist about the voice session. Turns and
// Created exist so the session can be rotated BEFORE it turns into a swamp:
// the 2026-10-24 incident was a 3580-message, 7.9 MB session accumulated
// since August, which made every reply take 8-15 seconds and occasionally
// made the model echo the whole transcript back (and TTS read it aloud).
type voiceSessionState struct {
	ID      string `json:"id"`
	Created int64  `json:"created,omitempty"` // unix seconds
	Turns   int    `json:"turns,omitempty"`
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// loadSessionState reads the persisted voice session, if any.
func loadSessionState() (voiceSessionState, bool) {
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return voiceSessionState{}, false
	}
	var st voiceSessionState
	if json.Unmarshal(data, &st) != nil || st.ID == "" {
		return voiceSessionState{}, false
	}
	return st, true
}

// saveSessionState persists the session state across restarts.
func saveSessionState(st voiceSessionState) {
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o755); err != nil {
		log.Printf("brain: state dir: %v", err)
		return
	}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(sessionFile, data, 0o600); err != nil {
		log.Printf("brain: write session state: %v", err)
	}
}

// sessionRotten reports why the persisted session must be retired, or "".
// A session with no creation time (the old format) counts as rotten: we
// cannot tell how big it is, and in practice it is the August swamp.
func sessionRotten(st voiceSessionState) string {
	maxTurns := envInt("GOV_SESSION_MAX_TURNS", 150)
	maxHours := envInt("GOV_SESSION_MAX_HOURS", 24)
	if st.Created == 0 {
		return "legacy session file (no creation time)"
	}
	if st.Turns >= maxTurns {
		return fmt.Sprintf("%d turns (limit %d)", st.Turns, maxTurns)
	}
	age := time.Since(time.Unix(st.Created, 0))
	if age > time.Duration(maxHours)*time.Hour {
		return fmt.Sprintf("age %s (limit %dh)", age.Round(time.Minute), maxHours)
	}
	return ""
}

// persistedSession returns the saved session, or nil when it must be rotated.
func persistedSession() *opencodeclient.Session {
	st, ok := loadSessionState()
	if !ok {
		return nil
	}
	if reason := sessionRotten(st); reason != "" {
		log.Printf("brain: persisted session %s is stale: %s -> creating a fresh one",
			st.ID, reason)
		rotateSessionLocked(reason)
		return nil
	}
	return &opencodeclient.Session{ID: st.ID}
}

// RotateSession drops the in-memory session and its persisted state, so the
// next BrainAsk creates a fresh one. Safe to call from anywhere.
func RotateSession(reason string) {
	brainMu.Lock()
	rotateSessionLocked(reason)
	brainMu.Unlock()
}

// rotateSessionLocked does the work of RotateSession with brainMu already
// held (persistedSession runs under that lock — a plain Lock there would
// deadlock).
func rotateSessionLocked(reason string) {
	old := ""
	if brainSess != nil {
		old = brainSess.ID
	}
	brainSess = nil
	if err := os.Remove(sessionFile); err != nil && !os.IsNotExist(err) {
		log.Printf("brain: remove session state: %v", err)
	}
	log.Printf("brain: voice session rotated (was %q): %s", old, reason)
}

// bumpTurns increments the persisted turn counter of the current session.
func bumpTurns(id string) {
	st, ok := loadSessionState()
	if !ok || st.ID != id {
		st = voiceSessionState{ID: id, Created: time.Now().Unix()}
	}
	st.Turns++
	saveSessionState(st)
}


// newVoiceSession creates a fresh session and sends the startup message.
func newVoiceSession(cl *opencodeclient.Client) (*opencodeclient.Session, error) {
	sess, err := cl.CreateSession("voice-chat", "baron", "", "", "voicekit", nil)
	if err != nil {
		return nil, fmt.Errorf("brain: create session: %w", err)
	}
	saveSessionState(voiceSessionState{ID: sess.ID, Created: time.Now().Unix()})
	log.Printf("brain: session created %s", sess.ID)

	startup := "Ты находишься в голосовом чате. Это НЕ текстовый чат с " +
		"Кириллом — ты агент, подключённый к войс-каналу через программу-посредника.\n\n" +
		"Используй SKILL voice-chat для правил работы в голосовом чате."
	if _, err := cl.SendMessage(sess, startup); err != nil {
		return nil, fmt.Errorf("brain: startup message: %w", err)
	}
	log.Printf("brain: startup message sent")
	return sess, nil
}

// getBrainSession returns the shared session, creating it lazily.
func getBrainSession() (*opencodeclient.Session, error) {
	brainMu.Lock()
	defer brainMu.Unlock()
	if brainCl == nil {
		// On the RU server the path to the brain goes through a stateful
		// firewall/DPI (reg.ru → Cloudflare) that silently kills idle
		// keep-alive connections — reusing them made requests hang.
		// Open a fresh connection per request (like nginx/curl) and cap
		// the idle pool short as well.
		brainCl = opencodeclient.New(brainBaseURL(), 0, opencodeclient.Options{
			DisableKeepAlives: true,
			IdleConnTimeout:   30 * time.Second,
		})
		if u, p := brainBasicAuth(); u != "" {
			brainCl.SetBasicAuth(u, p)
			log.Printf("brain: basic auth configured for %s", brainBaseURL())
		}
	}
	if brainSess == nil {
		if ps := persistedSession(); ps != nil {
			brainSess = ps
			log.Printf("brain: reusing persisted session %s", ps.ID)
			return brainSess, nil
		}
		sess, err := newVoiceSession(brainCl)
		if err != nil {
			return nil, err
		}
		brainSess = sess
	}
	return brainSess, nil
}

// isSessionGone reports whether the error means the session disappeared or
// became unusable (e.g. the opencode-serve was restarted, the session state
// was lost, and requests time out). Treat timeouts and any transport error
// as "session gone" so we recreate it instead of hanging forever.
func isSessionGone(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if len(s) == 0 {
		return false
	}
	if contains(s, "404") || contains(s, "Session not found") ||
		contains(s, "NotFoundError") || contains(s, "session not found") {
		return true
	}
	// Transport errors: timeouts, connection refused/reset, EOF, etc.
	if contains(s, "deadline exceeded") || contains(s, "timeout") ||
		contains(s, "connection refused") || contains(s, "connection reset") ||
		contains(s, "EOF") || contains(s, "unexpected end of JSON") ||
		contains(s, "empty reply") {
		return true
	}
	return false
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// sendWithRetry sends a message, recreating the session once if lost.
func sendWithRetry(text string) (string, error) {
	sess, err := getBrainSession()
	if err != nil {
		return "", err
	}
	reply, err := brainCl.SendMessage(sess, text)
	if err != nil && isSessionGone(err) {
		log.Printf("brain: session lost, recreating: %v", err)
		brainMu.Lock()
		if brainSess != nil && brainSess.ID == sess.ID {
			brainSess = nil
		}
		if brainSess == nil {
			newSess, createErr := newVoiceSession(brainCl)
			if createErr != nil {
				brainMu.Unlock()
				return "", createErr
			}
			brainSess = newSess
		}
		sess = brainSess
		brainMu.Unlock()
		reply, err = brainCl.SendMessage(sess, text)
	}
	if err != nil {
		return "", fmt.Errorf("send message: %w", err)
	}
	return reply, nil
}

// BrainAsk sends the user text to Baron and returns his reply. After a
// successful turn the session is checked for old age / too many turns and
// rotated if needed (the answer itself is already in hand).
func BrainAsk(text string) (string, error) {
	reply, err := sendWithRetry(text)
	if err != nil {
		return "", err
	}
	brainMu.Lock()
	id := ""
	if brainSess != nil {
		id = brainSess.ID
	}
	brainMu.Unlock()
	if id != "" {
		bumpTurns(id)
		if st, ok := loadSessionState(); ok && st.ID == id {
			if reason := sessionRotten(st); reason != "" {
				RotateSession(reason)
			}
		}
	}
	return reply, nil
}

// SanitizeReply cleans a brain answer before it goes to TTS. It returns
// ok=false when the reply is not an answer at all but the model echoing the
// conversation transcript — a symptom of an over-grown session, seen on
// 2026-10-23 when the voice client spoke a wall of raw "user[...]" turns.
func SanitizeReply(s string) (string, bool) {
	s = TrimWhitespace(s)
	if s == "" {
		return "", true
	}
	if strings.Contains(s, "ТЕХНИЧЕСКАЯ ИНФОРМАЦИЯ") || strings.Contains(s, "user[") {
		return "", false
	}
	return s, true
}

// ReplyMaxChars is the maximum reply length spoken aloud (env GOV_REPLY_MAX,
// default 500). A voice answer longer than that is a monologue: the user has
// to sit through it, and TTS latency grows with length.
func ReplyMaxChars() int {
	return envInt("GOV_REPLY_MAX", 500)
}

// TruncateForSpeech cuts a reply to max runes on a sentence boundary so TTS
// does not read a minute-long wall of text.
func TruncateForSpeech(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := string(r[:max])
	if i := strings.LastIndexAny(cut, ".!?\n"); i > max/2 {
		return strings.TrimSpace(cut[:i+1])
	}
	return strings.TrimSpace(cut) + "…"
}
