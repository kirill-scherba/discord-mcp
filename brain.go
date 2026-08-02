// brain.go — connection to Baron via opencode-serve.
//
// A single long-lived session is kept so the bot has context across the whole
// voice conversation. The session is created EMPTY — no prior history is
// injected, the conversation context lives inside the session itself.
// Right after creation Baron gets a startup message telling him he is in a
// voice chat; when a participant joins, Baron is notified with the matching
// contact card from /srv/contacts/ (same contact book as mail-mcp).
package main

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	opencodeclient "github.com/kirill-scherba/opencode-client"
)

// contactsDir is the shared contact book directory (same as mail-mcp).
const contactsDir = "/srv/contacts"

var (
	brainMu   sync.Mutex
	brainCl   *opencodeclient.Client
	brainSess *opencodeclient.Session
)

// Contact is a contact card from the shared contact book.
type Contact struct {
	Email     string `json:"email"`
	Name      string `json:"name,omitempty"`
	Who       string `json:"who,omitempty"`
	Summary   string `json:"summary,omitempty"`
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
	TotalMail int    `json:"total_mail"`
}

// contactKey mirrors mail-mcp: md5 hash of the lowercased email.
func contactKey(email string) string {
	hash := fmt.Sprintf("%x", md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email)))))
	return contactsDir + "/" + hash + ".json"
}

// findContactByName returns the first contact whose Name matches the given
// Discord display name (case-insensitive exact or substring match).
func findContactByName(name string) *Contact {
	if name == "" {
		return nil
	}
	entries, err := os.ReadDir(contactsDir)
	if err != nil {
		return nil
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(contactsDir, e.Name()))
		if err != nil {
			continue
		}
		var c Contact
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}
		if c.Name == "" {
			continue
		}
		cl := strings.ToLower(c.Name)
		if cl == lower || strings.Contains(lower, cl) || strings.Contains(cl, lower) {
			return &c
		}
	}
	return nil
}

// isSessionGone reports whether the error means the opencode-serve session
// disappeared (server restart, TTL expiry, manual close). In that case a
// fresh session must be created. The old session is never closed by us.
func isSessionGone(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Session not found") || strings.Contains(msg, "404")
}

// createBrainSession creates a fresh empty session and sends the startup
// message telling Baron he is in a voice chat.
func createBrainSession(cl *opencodeclient.Client) (*opencodeclient.Session, error) {
	sess, err := cl.CreateSession("voice-chat", "baron", "", "", nil)
	if err != nil {
		return nil, fmt.Errorf("brain: create session: %w", err)
	}
	log.Printf("brain: session created %s", sess.ID)

	startup := "Ты в голосовом чате Discord. Отвечай кратко и по делу — твои " +
		"ответы синтезируются в речь и озвучиваются в канале. Обращайся к " +
		"Кириллу «Ваше Величество». Когда в канал заходит участник, я сообщу " +
		"тебе, кто это."
	if _, err := cl.SendMessage(sess, startup); err != nil {
		return nil, fmt.Errorf("brain: startup message: %w", err)
	}
	log.Printf("brain: startup message sent")
	return sess, nil
}

// getBrainSession returns the shared session, creating it lazily on first use.
// The session ID is persisted across bot restarts (see session_store.go): the
// voice-chat session is meant to live forever, so a restart reuses it.
func getBrainSession() (*opencodeclient.Session, error) {
	brainMu.Lock()
	defer brainMu.Unlock()
	if brainCl == nil {
		brainCl = opencodeclient.New("", 0)
	}
	if brainSess == nil {
		if ps := persistedSession(); ps != nil {
			// Reuse the saved session; sendWithRetry recreates it if dead.
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

// resetBrainSession drops the current session so the next call recreates it.
// The old session is left untouched on the server.
func resetBrainSession() {
	brainMu.Lock()
	brainSess = nil
	brainMu.Unlock()
}

// sendWithRetry sends a message to Baron, recreating the session once if it
// was lost (Session not found / 404). The message itself is preserved.
func sendWithRetry(text string) (string, error) {
	sess, err := getBrainSession()
	if err != nil {
		return "", err
	}
	reply, err := brainCl.SendMessage(sess, text)
	if err != nil && isSessionGone(err) {
		log.Printf("brain: session lost, recreating: %v", err)
		resetBrainSession()
		sess, err = newVoiceSession(brainCl)
		if err != nil {
			return "", err
		}
		brainSess = sess
		reply, err = brainCl.SendMessage(sess, text)
	}
	if err != nil {
		return "", fmt.Errorf("send message: %w", err)
	}
	return reply, nil
}

// brainAsk sends a user utterance to Baron and returns the reply text.
func brainAsk(text string) (string, error) {
	return sendWithRetry(text)
}

// brainNotifyContact tells Baron who joined the voice channel, using the
// shared contact card when the user is known. Baron's reply is spoken aloud
// into the voice channel.
func brainNotifyContact(displayName, userID string) {
	msg := fmt.Sprintf("В голосовой канал зашёл участник: %s (discord id %s).",
		displayName, userID)
	if c := findContactByName(displayName); c != nil {
		msg = fmt.Sprintf("В голосовой канал зашёл участник: %s. Это %s.", displayName, c.Name)
		if c.Who != "" {
			msg += " " + c.Who + "."
		}
		if c.Summary != "" {
			msg += " Ранее обсуждали: " + c.Summary
		}
	}
	reply, err := sendWithRetry(msg)
	if err != nil {
		log.Printf("brain: notify failed: %v", err)
		return
	}
	log.Printf("brain: notified Baron about %s", displayName)

	reply = trimWhitespace(reply)
	log.Printf("brain: notify reply (%d chars): %q", len(reply), reply)
	if reply == "" {
		return
	}
	// Speak Baron's reply into the voice channel so the participant hears it.
	theBot.mu.Lock()
	vc := theBot.vc
	theBot.mu.Unlock()
	if vc != nil && vc.Ready {
		theBot.speak(vc, reply)
	}
}
