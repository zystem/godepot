package upstream

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestPrivate(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true, "169.254.169.254": true, "fe80::1": true, "fd00::1": true,
		"0.0.0.0": true, "100.64.0.1": true, "224.0.0.1": true, "::ffff:127.0.0.1": true,
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700:4700::1111": false, "100.128.0.1": false, "172.32.0.1": false,
	} {
		if got := Private(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Private(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestParseParams(t *testing.T) {
	p := parseParams(`realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull", error=invalid`)
	if p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" || p["scope"] != "repository:library/alpine:pull" || p["error"] != "invalid" {
		t.Fatalf("%v", p)
	}
	if len(parseParams(`realm="unterminated`)) != 0 || len(parseParams(`nonsense`)) != 0 {
		t.Fatal("garbage gave params")
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if retryAfter("30", now) != 30*time.Second || retryAfter("", now) != 0 || retryAfter("soon", now) != 0 || retryAfter("-5", now) != 0 {
		t.Fatal("seconds")
	}
	if d := retryAfter(now.Add(90*time.Second).Format("Mon, 02 Jan 2006 15:04:05 GMT"), now); d != 90*time.Second {
		t.Fatalf("a date: %v", d)
	}
}

func TestFlightsJoin(t *testing.T) {
	var f Flights
	lead, end, _ := f.Join("k")
	if !lead {
		t.Fatal("the first is the leader")
	}
	lead2, _, wait := f.Join("k")
	if lead2 {
		t.Fatal("the second joins")
	}
	if other, _, _ := f.Join("other"); !other {
		t.Fatal("another key has its own leader")
	}
	done := make(chan error, 1)
	go func() { done <- wait(context.Background()) }()
	want := errors.New("fetch failed")
	end(want)
	if err := <-done; err != want {
		t.Fatalf("the follower was told %v", err)
	}
	// After the end the next is a leader again: an error is not remembered here.
	if again, _, _ := f.Join("k"); !again {
		t.Fatal("a finished flight stayed")
	}
	// A follower whose own request ends stops waiting.
	_, _, w2 := f.Join("k")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w2(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}
