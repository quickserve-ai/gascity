package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func freezeMarker(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploy-freeze.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const validFreeze = `{"bead":"ga-test01","commit":"332750f46","frozen_by":"katya","reason":"gated batch"}`

func TestDeployFreezeAbsentProceeds(t *testing.T) {
	var out, errw bytes.Buffer
	if rc := checkDeployFreeze(filepath.Join(t.TempDir(), "none.json"), "", &out, &errw); rc != 0 {
		t.Fatalf("no marker must proceed, got rc=%d stderr=%s", rc, errw.String())
	}
	if rc := checkDeployFreeze("", "", &out, &errw); rc != 0 {
		t.Fatalf("empty path (no home) must proceed, got rc=%d", rc)
	}
}

func TestDeployFreezeBlocksWithoutAck(t *testing.T) {
	path := freezeMarker(t, validFreeze)
	var out, errw bytes.Buffer
	if rc := checkDeployFreeze(path, "", &out, &errw); rc != 1 {
		t.Fatalf("standing freeze must block, got rc=%d", rc)
	}
	for _, want := range []string{"ga-test01", "katya", "--acknowledge-freeze"} {
		if !strings.Contains(errw.String(), want) {
			t.Errorf("block message missing %q: %s", want, errw.String())
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("blocked install must leave the marker in place")
	}
}

func TestDeployFreezeWrongAckBlocks(t *testing.T) {
	path := freezeMarker(t, validFreeze)
	var out, errw bytes.Buffer
	if rc := checkDeployFreeze(path, "ga-other", &out, &errw); rc != 1 {
		t.Fatalf("mismatched ack must block, got rc=%d", rc)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("mismatched ack must leave the marker in place")
	}
}

func TestDeployFreezeAckedSupersedesDurably(t *testing.T) {
	path := freezeMarker(t, validFreeze)
	var out, errw bytes.Buffer
	if rc := checkDeployFreeze(path, "ga-test01", &out, &errw); rc != 0 {
		t.Fatalf("matching ack must proceed, got rc=%d stderr=%s", rc, errw.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("acked supersede must consume the marker")
	}
	matches, _ := filepath.Glob(path + ".superseded-*")
	if len(matches) != 1 {
		t.Fatalf("expected one superseded record, got %v", matches)
	}
	if !strings.Contains(out.String(), "STAMP THE BEAD") {
		t.Errorf("supersede message must demand the bead stamp: %s", out.String())
	}
}

func TestDeployFreezeMalformedFailsClosed(t *testing.T) {
	for _, content := range []string{"{not json", `{"reason":"no bead field"}`} {
		path := freezeMarker(t, content)
		var out, errw bytes.Buffer
		if rc := checkDeployFreeze(path, "ga-test01", &out, &errw); rc != 1 {
			t.Errorf("malformed marker %q must fail closed even with an ack, got rc=%d", content, rc)
		}
	}
}
