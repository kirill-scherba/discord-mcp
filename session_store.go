// session_store.go — persistent voice-chat session reuse across bot restarts.
//
// Unlike mail-mcp (per-sender entries), the voice bot keeps exactly ONE
// opencode session that must live forever: it is the conversational memory of
// the voice channel. The session ID is persisted to a JSON file so a bot
// restart reuses the same session instead of creating a fresh one.
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	opencodeclient "github.com/kirill-scherba/opencode-client"
)

// voiceSessionFile stores the single voice-chat session ID.
const voiceSessionFile = "/home/kirill/.config/discord-mcp/voice_session.json"

// loadVoiceSessionID reads the persisted session ID, or "" if none.
func loadVoiceSessionID() string {
	data, err := os.ReadFile(voiceSessionFile)
	if err != nil {
		return ""
	}
	var s struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("brain: session store decode: %v", err)
		return ""
	}
	return s.SessionID
}

// saveVoiceSessionID persists the session ID so it survives bot restarts.
func saveVoiceSessionID(id string) {
	if err := os.MkdirAll(filepath.Dir(voiceSessionFile), 0o755); err != nil {
		log.Printf("brain: session store mkdir: %v", err)
		return
	}
	data, err := json.Marshal(map[string]string{"session_id": id})
	if err != nil {
		log.Printf("brain: session store encode: %v", err)
		return
	}
	if err := os.WriteFile(voiceSessionFile, data, 0o644); err != nil {
		log.Printf("brain: session store save: %v", err)
	}
}

// persistedSession returns an opencodeclient.Session from the store if a
// session ID was saved earlier. The session may be dead on the server; the
// caller detects that via SendMessage and recreates.
func persistedSession() *opencodeclient.Session {
	if id := loadVoiceSessionID(); id != "" {
		return &opencodeclient.Session{ID: id, Agent: "baron"}
	}
	return nil
}

// newVoiceSession creates a fresh session, persists its ID, and returns it.
func newVoiceSession(cl *opencodeclient.Client) (*opencodeclient.Session, error) {
	sess, err := createBrainSession(cl)
	if err != nil {
		return nil, err
	}
	saveVoiceSessionID(sess.ID)
	return sess, nil
}
