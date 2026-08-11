// brain.go — connection to Baron via opencode-serve (Discord-specific part).
//
// Shared pipeline (sessions, brainAsk) lives in voicekit; this file keeps the
// contact book lookup and participant join/leave notifications used only by
// the Discord bot.
package main

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/kirill-scherba/discord-mcp/voicekit"
)

// contactsDir is the shared contact book directory (same as mail-mcp).
const contactsDir = "/srv/contacts"

// brainAsk sends the user text to Baron and returns his reply.
func brainAsk(text string) (string, error) {
	return voicekit.BrainAsk(text)
}

// Contact is a contact card from the shared contact book.
type Contact struct {
	Email     string `json:"email"`
	Name      string `json:"name,omitempty"`
	Who       string `json:"who,omitempty"`
	Summary   string `json:"summary,omitempty"`
	DiscordID string `json:"discord_id,omitempty"`
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
	TotalMail int    `json:"total_mail"`
}

// contactKey mirrors mail-mcp: md5 hash of the lowercased email.
func contactKey(email string) string {
	hash := fmt.Sprintf("%x", md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email)))))
	return filepath.Join(contactsDir, hash+".json")
}

// findContactByName returns the first contact whose Name matches.
func findContactByName(name string) *Contact {
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
		if json.Unmarshal(data, &c) != nil {
			continue
		}
		if strings.ToLower(c.Name) == lower {
			return &c
		}
	}
	return nil
}

// findContactByDiscordID returns the contact whose discord_id matches.
func findContactByDiscordID(userID string) *Contact {
	if userID == "" {
		return nil
	}
	entries, err := os.ReadDir(contactsDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(contactsDir, e.Name()))
		if err != nil {
			continue
		}
		var c Contact
		if json.Unmarshal(data, &c) != nil {
			continue
		}
		if c.DiscordID == userID {
			return &c
		}
	}
	return nil
}

// brainNotifyState tells Baron about a voice-channel participant change
// (join/leave) and who is currently in the channel. Silent — not spoken.
func brainNotifyState(action, displayName, userID string, present []string) {
	var c *Contact
	if c = findContactByDiscordID(userID); c == nil {
		c = findContactByName(displayName)
	}

	who := "Неизвестный пользователь"
	if c != nil {
		who = c.Name
	}

	actionRu := "зашёл в голосовой канал"
	if action == "leave" {
		actionRu = "покинул голосовой канал"
	}
	msg := fmt.Sprintf("[ТЕХНИЧЕСКОЕ] Участник %s: %s (discord id %s). Это %s.",
		actionRu, displayName, userID, who)
	if c != nil && c.Who != "" {
		msg += " " + c.Who + "."
	}
	msg += " Сейчас в канале: " + formatPresent(present)

	if _, err := voicekit.BrainAsk(msg); err != nil {
		log.Printf("brain: notify failed: %v", err)
		return
	}
	log.Printf("brain: notified Baron: %s %s (silent), present=%d", action, displayName, len(present))
}

// formatPresent renders the current channel roster as a human-readable list.
func formatPresent(ids []string) string {
	if len(ids) == 0 {
		return "никого, кроме бота"
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if c := findContactByDiscordID(id); c != nil {
			names = append(names, c.Name)
			continue
		}
		names = append(names, "пользователь "+id)
	}
	return strings.Join(names, ", ")
}
