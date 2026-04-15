package circuit

import (
	"testing"
	"time"
)

func TestBreakerTripsOnConsecutive429s(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 3, Cooldown: 60})

	m.Record429("backend-a", 0)
	m.Record429("backend-a", 0)
	if m.IsOpen("backend-a") {
		t.Fatal("should not trip after 2 failures with threshold 3")
	}

	m.Record429("backend-a", 0)
	if !m.IsOpen("backend-a") {
		t.Fatal("should trip after 3 consecutive 429s")
	}

	s := m.State("backend-a")
	if s.State != "open" {
		t.Fatalf("expected state 'open', got %q", s.State)
	}
	if s.Failures != 3 {
		t.Fatalf("expected 3 failures, got %d", s.Failures)
	}
}

func TestBreakerResetsOnSuccess(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 2, Cooldown: 60})

	m.Record429("b", 0)
	m.Record429("b", 0)
	if !m.IsOpen("b") {
		t.Fatal("should be open")
	}

	m.RecordSuccess("b")
	if m.IsOpen("b") {
		t.Fatal("should be closed after success")
	}

	s := m.State("b")
	if s.State != "closed" {
		t.Fatalf("expected closed, got %q", s.State)
	}
	if s.Failures != 0 {
		t.Fatalf("expected 0 failures, got %d", s.Failures)
	}
}

func TestBreakerCooldownTransitionsToHalfOpen(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 1, Cooldown: 1})

	m.Record429("b", 0)
	if !m.IsOpen("b") {
		t.Fatal("should be open")
	}

	time.Sleep(1100 * time.Millisecond)

	if m.IsOpen("b") {
		t.Fatal("should transition to half-open after cooldown")
	}

	s := m.State("b")
	if s.State != "half-open" {
		t.Fatalf("expected half-open, got %q", s.State)
	}
}

func TestHalfOpenProbeSuccess(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 1, Cooldown: 1})

	m.Record429("b", 0)
	time.Sleep(1100 * time.Millisecond)
	_ = m.IsOpen("b")

	m.RecordSuccess("b")
	s := m.State("b")
	if s.State != "closed" {
		t.Fatalf("expected closed after probe success, got %q", s.State)
	}
}

func TestHalfOpenProbeFailure(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 1, Cooldown: 1})

	m.Record429("b", 0)
	time.Sleep(1100 * time.Millisecond)
	_ = m.IsOpen("b")

	m.Record429("b", 0)
	s := m.State("b")
	if s.State != "open" {
		t.Fatalf("expected open after probe failure, got %q", s.State)
	}
}

func TestRetryAfterHint(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 1, Cooldown: 300})

	m.Record429("b", 10*time.Second)
	if !m.IsOpen("b") {
		t.Fatal("should be open")
	}

	s := m.State("b")
	if s.RetryAfter.IsZero() {
		t.Fatal("retry_after should be set")
	}
	until := time.Until(s.RetryAfter)
	if until < 5*time.Second || until > 15*time.Second {
		t.Fatalf("retry_after hint not respected, got %v until retry", until)
	}
}

func TestRetryAfterHintCapped(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 1, Cooldown: 60})

	m.Record429("b", 3*time.Hour)
	s := m.State("b")
	until := time.Until(s.RetryAfter)
	if until > 2*time.Minute {
		t.Fatalf("huge hint should be capped, got %v until retry", until)
	}
}

func TestManualReset(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 1, Cooldown: 300})

	m.Record429("b", 0)
	if !m.IsOpen("b") {
		t.Fatal("should be open")
	}

	m.Reset("b")
	if m.IsOpen("b") {
		t.Fatal("should be closed after reset")
	}

	s := m.State("b")
	if s.State != "closed" {
		t.Fatalf("expected closed, got %q", s.State)
	}
}

func TestResetAll(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 1, Cooldown: 300})

	m.Record429("a", 0)
	m.Record429("b", 0)

	m.ResetAll()

	for _, name := range []string{"a", "b"} {
		if m.IsOpen(name) {
			t.Fatalf("%s should be closed after reset all", name)
		}
	}
}

func TestNonConsecutiveFailuresReset(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 3, Cooldown: 60})

	m.Record429("b", 0)
	m.Record429("b", 0)
	m.RecordSuccess("b")
	m.Record429("b", 0)
	m.Record429("b", 0)
	if m.IsOpen("b") {
		t.Fatal("should not trip — failures were not consecutive")
	}
}

func TestFilterEntries(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 1, Cooldown: 300})

	m.Record429("a", 0)

	filtered := m.FilterEntries([]string{"a", "b", "c"})
	if len(filtered) != 2 {
		t.Fatalf("expected 2 active, got %d: %v", len(filtered), filtered)
	}
	for _, name := range filtered {
		if name == "a" {
			t.Fatal("tripped backend 'a' should be filtered out")
		}
	}
}

func TestFilterEntriesNeverBlocksAll(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 1, Cooldown: 300})

	m.Record429("a", 0)
	m.Record429("b", 0)

	names := []string{"a", "b"}
	filtered := m.FilterEntries(names)
	if len(filtered) != 2 {
		t.Fatalf("when all tripped, should return all: got %v", filtered)
	}
}

func TestDisabledManager(t *testing.T) {
	m := NewManager(Config{Enabled: false, Threshold: 1, Cooldown: 300})

	m.Record429("b", 0)
	if m.IsOpen("b") {
		t.Fatal("disabled manager should never trip")
	}

	filtered := m.FilterEntries([]string{"b"})
	if len(filtered) != 1 {
		t.Fatal("disabled manager should not filter")
	}
}

func TestUpdateConfig(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 3, Cooldown: 60})

	m.UpdateConfig(Config{Enabled: true, Threshold: 5, Cooldown: 120})

	s := m.State("b")
	if s.Threshold != 5 {
		t.Fatalf("expected threshold 5, got %d", s.Threshold)
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"", 0},
		{"30", 30 * time.Second},
		{"0", 0},
		{"-5", 0},
	}

	for _, tt := range tests {
		got := ParseRetryAfter(tt.input)
		if got != tt.want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Enabled {
		t.Error("default should be enabled")
	}
	if cfg.Threshold != 3 {
		t.Errorf("default threshold should be 3, got %d", cfg.Threshold)
	}
	if cfg.Cooldown != 300 {
		t.Errorf("default cooldown should be 300, got %d", cfg.Cooldown)
	}
}

func TestEnsureBackend(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 3, Cooldown: 60})
	m.EnsureBackend("test-backend")

	s := m.State("test-backend")
	if s.State != "closed" {
		t.Fatalf("new backend should be closed, got %q", s.State)
	}
}

func TestAllStates(t *testing.T) {
	m := NewManager(Config{Enabled: true, Threshold: 1, Cooldown: 300})

	m.Record429("a", 0)
	m.EnsureBackend("b")

	states := m.AllStates()
	if len(states) != 2 {
		t.Fatalf("expected 2 states, got %d", len(states))
	}

	found := map[string]string{}
	for _, s := range states {
		found[s.Name] = s.State
	}
	if found["a"] != "open" {
		t.Errorf("a should be open, got %q", found["a"])
	}
	if found["b"] != "closed" {
		t.Errorf("b should be closed, got %q", found["b"])
	}
}
