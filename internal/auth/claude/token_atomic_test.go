package claude

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func storedRefreshToken(t *testing.T, raw []byte) string {
	t.Helper()
	var stored ClaudeTokenStorage
	if errDecode := json.Unmarshal(raw, &stored); errDecode != nil {
		t.Fatalf("token file holds %d bytes that are not valid JSON: %v", len(raw), errDecode)
	}
	return stored.RefreshToken
}

// Anthropic rotates the refresh token on every refresh, so a token file left truncated or stale by
// a crash mid-save cannot be recovered: the account needs a new login. A save must therefore leave
// the old file whole and put a complete new one in its place. A reader that opened the file before
// the save shows it: it keeps reading the old token, where a rewrite in place would change or
// truncate what it sees.
func TestSaveTokenToFile_ReplacesFileInsteadOfRewritingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses to replace a file that another handle has open")
	}
	authFilePath := filepath.Join(t.TempDir(), "claude-test.json")
	storage := &ClaudeTokenStorage{Email: "user@example.com", AccessToken: "access-old", RefreshToken: "refresh-old"}
	if errSave := storage.SaveTokenToFile(authFilePath); errSave != nil {
		t.Fatalf("SaveTokenToFile() first save error = %v", errSave)
	}
	reader, errOpen := os.Open(authFilePath)
	if errOpen != nil {
		t.Fatalf("open token file: %v", errOpen)
	}
	defer reader.Close()

	storage.AccessToken, storage.RefreshToken = "access-new", "refresh-new"
	if errSave := storage.SaveTokenToFile(authFilePath); errSave != nil {
		t.Fatalf("SaveTokenToFile() second save error = %v", errSave)
	}

	seenByReader, errRead := io.ReadAll(reader)
	if errRead != nil {
		t.Fatalf("read through the handle opened before the save: %v", errRead)
	}
	if got := storedRefreshToken(t, seenByReader); got != "refresh-old" {
		t.Errorf("the reader that opened the file before the save sees refresh token %q, want %q: the save rewrote the file in place", got, "refresh-old")
	}
	live, errRead := os.ReadFile(authFilePath)
	if errRead != nil {
		t.Fatalf("read token file: %v", errRead)
	}
	if got := storedRefreshToken(t, live); got != "refresh-new" {
		t.Errorf("token file holds refresh token %q after the save, want %q", got, "refresh-new")
	}
}
