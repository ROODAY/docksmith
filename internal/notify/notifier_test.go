package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chis/docksmith/internal/events"
	"github.com/chis/docksmith/internal/storage"
	"github.com/chis/docksmith/internal/update"
)

func checkResult(cur, latest string, labels map[string]string) *update.DiscoveryResult {
	return &update.DiscoveryResult{Containers: []update.ContainerInfo{{
		ContainerUpdate: update.ContainerUpdate{
			ContainerName:  "web",
			Status:         update.UpdateAvailable,
			CurrentVersion: cur,
			LatestVersion:  latest,
		},
		Labels: labels,
	}}}
}

func startNotifier(t *testing.T, st *storage.SQLiteStorage, fs Sender) (*Notifier, *events.Bus) {
	t.Helper()
	bus := events.NewBus()
	n := New(st, st, fakeOps{ops: map[string]storage.UpdateOperation{}}, bus, fs, "engager", 5*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	n.Start(ctx)
	return n, bus
}

func TestNotifier_SeedsSilentlyThenNotifies(t *testing.T) {
	st := newDispatchStore(t)
	addService(t, st, "imm", "immediate", true, EventNewVersion)
	fs := &fakeSender{}
	ctx := context.Background()

	n, _ := startNotifier(t, st, fs)
	n.OnCheckResult(ctx, checkResult("1.0.0", "1.0.1", nil), 1)
	time.Sleep(100 * time.Millisecond)
	if got := fs.total(); got != 0 {
		t.Fatalf("seeding run sent %d messages, want 0", got)
	}
	if v, ok, err := st.GetConfig(ctx, storage.NotifySeededKey); err != nil || !ok || v == "" {
		t.Fatalf("notify_seeded = %q, %v, %v; want set", v, ok, err)
	}

	n.OnCheckResult(ctx, checkResult("1.0.0", "1.0.2", nil), 2)
	waitFor(t, "one send", func() bool { return fs.total() == 1 })

	fs2 := &fakeSender{}
	n2, _ := startNotifier(t, st, fs2)
	n2.OnCheckResult(ctx, checkResult("1.0.0", "1.0.2", nil), 3)
	time.Sleep(100 * time.Millisecond)
	if got := fs2.total(); got != 0 {
		t.Fatalf("restarted notifier sent %d messages, want 0", got)
	}
	if got := fs.total(); got != 1 {
		t.Fatalf("first sender total = %d, want 1", got)
	}
}

func TestNotifier_DropsPendingForRemovedContainers(t *testing.T) {
	st := newDispatchStore(t)
	sid := addService(t, st, "sum", "summary", true, EventNewVersion)
	n, _ := startNotifier(t, st, &fakeSender{})
	ctx := context.Background()

	n.OnCheckResult(ctx, checkResult("1.0.0", "1.0.1", nil), 4)
	n.OnCheckResult(ctx, checkResult("1.0.0", "1.0.2", nil), 5)
	if got := len(pending(t, st, sid)); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	n.OnCheckResult(ctx, &update.DiscoveryResult{}, 6)
	if got := len(pending(t, st, sid)); got != 0 {
		t.Fatalf("pending after removal = %d, want 0", got)
	}
}

func TestNotifier_CurrentVersionsAndMutedOperations(t *testing.T) {
	st := newDispatchStore(t)
	addService(t, st, "imm", "immediate", true, EventUpdateFailed)
	fs := &fakeSender{}
	n, _ := startNotifier(t, st, fs)
	ctx := context.Background()

	n.OnCheckResult(ctx, checkResult("1.0.0", "1.0.1", map[string]string{"docksmith.notify": "false"}), 7)
	if v, ok := n.currentVersion("web"); !ok || v != "1.0.0" {
		t.Fatalf("currentVersion(web) = %q, %v", v, ok)
	}
	if _, ok := n.currentVersion("gone"); ok {
		t.Fatal("currentVersion(gone) reported existing")
	}

	n.emit(Event{Kind: EventUpdateFailed, Container: "web", At: time.Now()})
	n.emit(Event{Kind: EventUpdateFailed, Container: "other", At: time.Now()})
	waitFor(t, "one send", func() bool { return fs.total() == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := fs.total(); got != 1 {
		t.Fatalf("sends = %d, want 1 (muted container must be dropped)", got)
	}
	if c := fs.callsFor("imm"); !strings.Contains(c[0].msg.Text, "other") {
		t.Fatalf("sent message %q is not about the unmuted container", c[0].msg.Text)
	}
}

func TestNotifier_EmitAfterShutdownDoesNotBlock(t *testing.T) {
	st := newDispatchStore(t)
	n := New(st, st, fakeOps{}, events.NewBus(), &fakeSender{}, "h", 5*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	n.Start(ctx)
	cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			n.emit(Event{Kind: EventUpdateFailed, Container: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emit blocked after shutdown")
	}
}

func TestNotifier_UpdateSettings(t *testing.T) {
	st := newDispatchStore(t)
	n := New(st, st, fakeOps{}, events.NewBus(), &fakeSender{}, "h", time.Hour)
	ctx := context.Background()

	good := DefaultSettings("h")
	good.BaseURL = "https://docksmith.example.com/app"
	good.MinLevel = "minor"
	good.Timing = map[EventKind]Timing{EventNewVersion: TimingImmediate}

	bad := map[string]func(*Settings){
		"empty cron":       func(s *Settings) { s.SummaryCron = " " },
		"bad cron":         func(s *Settings) { s.SummaryCron = "nope" },
		"cron too often":   func(s *Settings) { s.SummaryCron = "*/5 * * * *" },
		"bad level":        func(s *Settings) { s.MinLevel = "huge" },
		"relative url":     func(s *Settings) { s.BaseURL = "/docksmith" },
		"ftp url":          func(s *Settings) { s.BaseURL = "ftp://host/x" },
		"url with query":   func(s *Settings) { s.BaseURL = "https://host/x?a=1" },
		"url with frag":    func(s *Settings) { s.BaseURL = "https://host/x#f" },
		"bad timing value": func(s *Settings) { s.Timing = map[EventKind]Timing{EventNewVersion: "later"} },
		"unknown kind":     func(s *Settings) { s.Timing = map[EventKind]Timing{"bogus": TimingSummary} },
	}
	for name, mut := range bad {
		s := DefaultSettings("h")
		mut(&s)
		if err := n.UpdateSettings(ctx, s); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, ok, _ := st.GetConfig(ctx, storage.NotifySettingsKey); ok {
		t.Fatal("invalid settings were saved")
	}

	if err := n.UpdateSettings(ctx, good); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	got := n.Settings()
	if got.MinLevel != "minor" || got.BaseURL != good.BaseURL || got.Timing[EventNewVersion] != TimingImmediate ||
		got.Timing[EventUpdateFailed] != TimingImmediate || got.Timing[EventDigestChanged] != TimingSummary {
		t.Fatalf("settings not applied/filled: %+v", got)
	}
	got.Timing[EventNewVersion] = TimingSummary
	if n.Settings().Timing[EventNewVersion] != TimingImmediate {
		t.Fatal("Settings() returned a shared Timing map")
	}
	loaded, err := LoadSettings(ctx, st, "h")
	if err != nil || loaded.MinLevel != "minor" {
		t.Fatalf("settings not persisted: %+v, %v", loaded, err)
	}
	if n.CheckInterval() != time.Hour || n.Dispatcher() == nil {
		t.Fatal("accessors wrong")
	}
}

func TestNotifier_IgnoresOutOfOrderResults(t *testing.T) {
	st := newDispatchStore(t)
	addService(t, st, "imm", "immediate", true, EventNewVersion)
	fs := &fakeSender{}
	n, _ := startNotifier(t, st, fs)
	ctx := context.Background()

	n.OnCheckResult(ctx, checkResult("1.0.0", "1.0.0", nil), 1) // seed
	n.OnCheckResult(ctx, checkResult("1.0.0", "1.0.2", nil), 3)
	waitFor(t, "one send", func() bool { return fs.total() == 1 })
	n.OnCheckResult(ctx, checkResult("0.9.0", "1.0.1", nil), 2) // older, delivered late
	if v, _ := n.currentVersion("web"); v != "1.0.0" {
		t.Fatalf("stale result updated current version to %q", v)
	}
	n.OnCheckResult(ctx, checkResult("1.0.0", "1.0.2", nil), 4)
	time.Sleep(100 * time.Millisecond)
	if got := fs.total(); got != 1 {
		t.Fatalf("sends = %d, want 1 (no backwards or repeated notification)", got)
	}
}

func TestNotifier_EmptyFirstResultDoesNotSeed(t *testing.T) {
	st := newDispatchStore(t)
	addService(t, st, "imm", "immediate", true, EventNewVersion)
	fs := &fakeSender{}
	n, _ := startNotifier(t, st, fs)
	ctx := context.Background()

	n.OnCheckResult(ctx, &update.DiscoveryResult{}, 1)
	if _, ok, _ := st.GetConfig(ctx, storage.NotifySeededKey); ok {
		t.Fatal("empty result seeded the state")
	}
	n.OnCheckResult(ctx, checkResult("1.0.0", "1.0.1", nil), 2)
	time.Sleep(100 * time.Millisecond)
	if got := fs.total(); got != 0 {
		t.Fatalf("first non-empty result sent %d messages, want 0", got)
	}
	if _, ok, _ := st.GetConfig(ctx, storage.NotifySeededKey); !ok {
		t.Fatal("first non-empty result did not seed")
	}
}
