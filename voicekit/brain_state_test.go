package voicekit

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withStateDir points the voice session state at a temporary directory.
func withStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, had := os.LookupEnv("GOV_STATE_DIR")
	os.Setenv("GOV_STATE_DIR", dir)
	oldFile := sessionFile
	sessionFile = filepath.Join(dir, "voice_session.json")
	t.Cleanup(func() {
		sessionFile = oldFile
		if had {
			os.Setenv("GOV_STATE_DIR", old)
		} else {
			os.Unsetenv("GOV_STATE_DIR")
		}
	})
	return dir
}

func TestSessionStateRoundTrip(t *testing.T) {
	withStateDir(t)

	if _, ok := loadSessionState(); ok {
		t.Fatal("empty dir must report no session state")
	}

	st := voiceSessionState{ID: "ses_test", Created: time.Now().Unix(), Turns: 3}
	saveSessionState(st)

	got, ok := loadSessionState()
	if !ok {
		t.Fatal("saved state is not readable")
	}
	if got.ID != st.ID || got.Turns != st.Turns || got.Created != st.Created {
		t.Fatalf("state mismatch: got %+v want %+v", got, st)
	}
}

// The old file format carried only the ID. Such a session has unknown age
// and size, and is exactly what had grown into the 3580-message swamp, so it
// must be retired instead of reused.
func TestLegacySessionIsRotten(t *testing.T) {
	dir := withStateDir(t)
	path := filepath.Join(dir, "voice_session.json")
	if err := os.WriteFile(path, []byte(`{"id":"ses_legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, ok := loadSessionState()
	if !ok {
		t.Fatal("legacy file must still parse")
	}
	if reason := sessionRotten(st); reason == "" {
		t.Fatal("legacy session must be reported as rotten")
	}
}

func TestSessionRottenByTurnsAndAge(t *testing.T) {
	withStateDir(t)
	os.Setenv("GOV_SESSION_MAX_TURNS", "5")
	os.Setenv("GOV_SESSION_MAX_HOURS", "2")
	defer os.Unsetenv("GOV_SESSION_MAX_TURNS")
	defer os.Unsetenv("GOV_SESSION_MAX_HOURS")

	fresh := voiceSessionState{ID: "a", Created: time.Now().Unix(), Turns: 1}
	if reason := sessionRotten(fresh); reason != "" {
		t.Fatalf("fresh session must be reusable, got %q", reason)
	}

	manyTurns := voiceSessionState{ID: "b", Created: time.Now().Unix(), Turns: 5}
	if reason := sessionRotten(manyTurns); reason == "" {
		t.Fatal("session over the turn limit must be rotten")
	}

	old := voiceSessionState{ID: "c", Created: time.Now().Add(-3 * time.Hour).Unix(), Turns: 1}
	if reason := sessionRotten(old); reason == "" {
		t.Fatal("session older than the age limit must be rotten")
	}
}

func TestBumpTurns(t *testing.T) {
	withStateDir(t)

	bumpTurns("ses_x")
	st, ok := loadSessionState()
	if !ok || st.ID != "ses_x" || st.Turns != 1 {
		t.Fatalf("first bump: got %+v ok=%v", st, ok)
	}
	bumpTurns("ses_x")
	st, _ = loadSessionState()
	if st.Turns != 2 {
		t.Fatalf("second bump: turns=%d", st.Turns)
	}
	// A different session id must reset the counter, not inherit it.
	bumpTurns("ses_y")
	st, _ = loadSessionState()
	if st.ID != "ses_y" || st.Turns != 1 {
		t.Fatalf("bump on new session: got %+v", st)
	}
}

func TestSanitizeReply(t *testing.T) {
	if _, ok := SanitizeReply(`Да, слышу вас.`); !ok {
		t.Fatal("a normal reply must pass")
	}
	// A transcript echo is what the swamp produced on 2026-10-23.
	echo := "Да, слушаю.\n\nuser[ТЕХНИЧЕСКАЯ ИНФОРМАЦИЯ: 2026-10-23 13:04 UTC] а приложение..."
	if _, ok := SanitizeReply(echo); ok {
		t.Fatal("a transcript echo must be rejected")
	}
}

func TestTruncateForSpeech(t *testing.T) {
	long := "Первое предложение здесь. Второе предложение тоже здесь. Третье лишнее."
	got := TruncateForSpeech(long, 45)
	if len([]rune(got)) > 45 {
		t.Fatalf("truncated text is too long: %q", got)
	}
	if got[len(got)-1] != '.' {
		t.Fatalf("expected a sentence boundary, got %q", got)
	}
	short := "Коротко."
	if TruncateForSpeech(short, 45) != short {
		t.Fatal("short reply must not change")
	}
}
