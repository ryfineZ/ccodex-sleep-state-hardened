package codexconfig

import (
	"bytes"
	"github.com/pelletier/go-toml/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecorderPreservesModelAndMissingModel(t *testing.T) {
	for _, original := range []string{"# keep\nmodel = 'my-custom-model'\nmodel_reasoning_effort = 'high'\n", "# no model configured\n", "profile='work'\nmodel='original-root'\n[profiles.work]\nmodel='original-profile'\n"} {
		b, err := PatchWithOptions([]byte(original), "http://127.0.0.1:17843/backend-api/codex", Options{PreserveModel: true, AuthMode: "chatgpt"})
		if err != nil {
			t.Fatal(err)
		}
		var before, after map[string]any
		_ = toml.Unmarshal([]byte(original), &before)
		_ = toml.Unmarshal(b, &after)
		if before["model"] != after["model"] {
			t.Fatal("model changed")
		}
		if strings.Contains(original, "original-profile") && !bytes.Contains(b, []byte("model='original-profile'")) {
			t.Fatal("profile model changed")
		}
	}
}
func TestRecorderTransactionRestoresAndKeepsUnrelatedEdits(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(home, "config.toml")
	original := []byte("# user's config\nmodel='custom-not-in-allowlist'\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := InstallWithOptions(dir, home, "http://127.0.0.1:17843/backend-api/codex", Options{PreserveModel: true, AuthMode: "chatgpt"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	b = append(b, []byte("\n# added while connected\n")...)
	_ = os.WriteFile(path, b, 0600)
	if err := CheckManaged(dir); err != nil {
		t.Fatal(err)
	}
	if err := Restore(dir); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if !bytes.Contains(b, original) || !bytes.Contains(b, []byte("# added while connected")) || bytes.Contains(b, []byte("17843")) {
		t.Fatal("restoration lost user configuration")
	}
}
