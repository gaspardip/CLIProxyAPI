package auth

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func storedAccessToken(t *testing.T, raw []byte) string {
	t.Helper()
	var stored struct {
		AccessToken string `json:"access_token"`
	}
	if errDecode := json.Unmarshal(raw, &stored); errDecode != nil {
		t.Fatalf("auth file holds %d bytes that are not valid JSON: %v", len(raw), errDecode)
	}
	return stored.AccessToken
}

// Credentials that live only in metadata (Antigravity, for one) are rewritten whenever a token
// refreshes. A save must leave the old file whole and put a complete new one in its place: a reader
// that opened the file before the save keeps reading the old token, where a rewrite in place would
// change or truncate what it sees, and a crash would leave the file that way for good.
func TestFileTokenStoreSaveMetadataReplacesFileInsteadOfRewritingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses to replace a file that another handle has open")
	}
	baseDir := t.TempDir()
	fileName := "antigravity-user.json"
	path := filepath.Join(baseDir, fileName)
	if errWrite := os.WriteFile(path, []byte(`{"type":"antigravity","access_token":"token-old","disabled":false}`), 0o600); errWrite != nil {
		t.Fatalf("write existing auth file: %v", errWrite)
	}
	reader, errOpen := os.Open(path)
	if errOpen != nil {
		t.Fatalf("open auth file: %v", errOpen)
	}
	defer reader.Close()

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	auth := &cliproxyauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Metadata: map[string]any{"type": "antigravity", "access_token": "token-new"},
	}
	if _, errSave := store.Save(context.Background(), auth); errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}

	seenByReader, errRead := io.ReadAll(reader)
	if errRead != nil {
		t.Fatalf("read through the handle opened before the save: %v", errRead)
	}
	if got := storedAccessToken(t, seenByReader); got != "token-old" {
		t.Errorf("the reader that opened the file before the save sees access token %q, want %q: the save rewrote the file in place", got, "token-old")
	}
	live, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read auth file: %v", errRead)
	}
	if got := storedAccessToken(t, live); got != "token-new" {
		t.Errorf("auth file holds access token %q after the save, want %q", got, "token-new")
	}
}
