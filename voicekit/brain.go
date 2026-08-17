package voicekit

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// sessionFile stores the persistent voice-chat session ID.
var sessionFile = filepath.Join(os.Getenv("HOME"), ".config", "mcp-gateway", "voice_session.json")

// loadVoiceSessionID reads the persisted session ID, if any.
func loadVoiceSessionID() string {
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return ""
	}
	var v struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &v) != nil {
		return ""
	}
	return v.ID
}

// saveVoiceSessionID persists the session ID across bot restarts.
func saveVoiceSessionID(id string) {
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o755); err != nil {
		return
	}
	data, _ := json.Marshal(map[string]string{"id": id})
	os.WriteFile(sessionFile, data, 0o600)
}

// persistedSession returns the saved session, or nil.
func persistedSession() *opencodeclient.Session {
	if id := loadVoiceSessionID(); id != "" {
		return &opencodeclient.Session{ID: id}
	}
	return nil
}

// newVoiceSession creates a fresh session and sends the startup message.
func newVoiceSession(cl *opencodeclient.Client) (*opencodeclient.Session, error) {
	sess, err := cl.CreateSession("voice-chat", "baron", "", "", "voicekit", nil)
	if err != nil {
		return nil, fmt.Errorf("brain: create session: %w", err)
	}
	saveVoiceSessionID(sess.ID)
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

// BrainAsk sends the user text to Baron and returns his reply.
func BrainAsk(text string) (string, error) {
	return sendWithRetry(text)
}
