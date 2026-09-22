package main

import (
	"bytes"
	"encoding/json"
	"github.com/gylive/ccodex-sleep-state/internal/reliableproxy"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckAndVersionDoNotStartService(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	b, _ := json.Marshal(reliableproxy.DefaultConfig())
	os.WriteFile(p, b, 0600)
	var out bytes.Buffer
	if err := run([]string{"-config", p, "-check"}, &out, &out); err != nil || !strings.Contains(out.String(), "Configuration valid") {
		t.Fatal(err, out.String())
	}
	out.Reset()
	if err := run([]string{"-version"}, &out, &out); err != nil || !strings.Contains(out.String(), version) {
		t.Fatal(err)
	}
}
