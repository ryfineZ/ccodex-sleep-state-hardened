package main

import (
	"bytes"
	"encoding/json"
	"github.com/gylive/ccodex-sleep-state/internal/requestrecorder"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionAndCheckHaveNoRuntimeSideEffects(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"-version"}, &out, &errOut); err != nil || !strings.Contains(out.String(), version) {
		t.Fatal(err, out.String())
	}
	c := requestrecorder.Default()
	c.Directory = filepath.Join(t.TempDir(), "not-created")
	path := filepath.Join(t.TempDir(), "config.json")
	data, _ := json.Marshal(c)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-config", path, "-check"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Directory); !os.IsNotExist(err) {
		t.Fatal("check created runtime directory")
	}
	c.Mode = "full"
	data, _ = json.Marshal(c)
	os.WriteFile(path, data, 0600)
	if err := run([]string{"-config", path}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "allow-sensitive-recording") {
		t.Fatal("full mode not gated", err)
	}
	if _, err := os.Stat(c.Directory); !os.IsNotExist(err) {
		t.Fatal("unacknowledged full mode created files")
	}
}
func TestOfflineAnalysisNeedsNeitherConfigNorNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.json")
	data := `{"schema_version":1,"mode":"redacted","model_detection":{"requested_model":"astra","forwarded_model":"astra"},"response_body":{"observed_eof":true,"json":{"model":"luna"}}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := run([]string{"-config", "nonexistent.json", "-analyze", path}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Verdict string `json:"verdict"`
		Actual  bool   `json:"actual_execution_verified"`
	}
	if json.Unmarshal(out.Bytes(), &result) != nil || result.Verdict != "mismatch" || result.Actual {
		t.Fatal(out.String())
	}
}
