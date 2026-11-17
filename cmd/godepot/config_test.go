package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func good() map[string]string {
	return map[string]string{
		"GODEPOT_RQLITE_URL":      "http://rq:4001",
		"GODEPOT_BLOBGATE_URL":    "http://bg:18090",
		"GODEPOT_BLOBGATE_TENANT": "godepot",
		"GODEPOT_BLOBGATE_SECRET": "a-secret-of-16-chars",
	}
}

func TestDefaults(t *testing.T) {
	c, err := loadConfig(envOf(good()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Store != "_default" || c.Port != 18100 || c.MetricsPort != 0 || c.Grace != 24*time.Hour || c.UploadTTL != 24*time.Hour ||
		c.CollectEvery != time.Minute || c.CollectBatch != 200 || c.LeaseTTL != 2*time.Minute {
		t.Fatalf("%+v", c)
	}
}

func TestEveryProblemAtOnce(t *testing.T) {
	_, err := loadConfig(envOf(map[string]string{"GODEPOT_PORT": "x", "GODEPOT_GRACE_SECONDS": "5", "GODEPOT_RQLITE_URL": "rq:4001"}))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"GODEPOT_RQLITE_URL is \"rq:4001\"", "GODEPOT_BLOBGATE_URL is required", "GODEPOT_BLOBGATE_TENANT is required", "GODEPOT_BLOBGATE_SECRET", "GODEPOT_PORT is \"x\"", "GODEPOT_GRACE_SECONDS is \"5\""} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no word about %q in:\n%v", want, err)
		}
	}
}

func TestSecretFromAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(f, []byte("a-secret-from-a-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := good()
	delete(m, "GODEPOT_BLOBGATE_SECRET")
	m["GODEPOT_BLOBGATE_SECRET_FILE"] = f
	c, err := loadConfig(envOf(m))
	if err != nil || c.Secret != "a-secret-from-a-file" {
		t.Fatalf("%v %+v", err, c)
	}
	m["GODEPOT_BLOBGATE_SECRET"] = "a-secret-of-16-chars"
	if _, err = loadConfig(envOf(m)); err == nil || !strings.Contains(err.Error(), "both set") || strings.Contains(err.Error(), "at least 16") {
		t.Fatalf("both set: %v", err)
	}
	m["GODEPOT_BLOBGATE_SECRET_FILE"] = filepath.Join(t.TempDir(), "none")
	delete(m, "GODEPOT_BLOBGATE_SECRET")
	if _, err = loadConfig(envOf(m)); err == nil || !strings.Contains(err.Error(), "GODEPOT_BLOBGATE_SECRET_FILE") {
		t.Fatalf("no file: %v", err)
	}
}

func TestTheLeaseMustOutlastTheRenewals(t *testing.T) {
	m := good()
	m["GODEPOT_COLLECT_SECONDS"], m["GODEPOT_LEASE_SECONDS"] = "60", "100"
	if _, err := loadConfig(envOf(m)); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("%v", err)
	}
	m["GODEPOT_LEASE_SECONDS"] = "120"
	if _, err := loadConfig(envOf(m)); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsPortMayNotBeThePort(t *testing.T) {
	m := good()
	m["GODEPOT_PORT"], m["GODEPOT_METRICS_PORT"] = "9000", "9000"
	if _, err := loadConfig(envOf(m)); err == nil {
		t.Fatal("accepted")
	}
}
