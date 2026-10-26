package notify

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chis/docksmith/internal/storage"
)

// These tests avoid relying on -race (unavailable without cgo): concurrency is
// checked through deadlines and explicit synchronisation on channels.

type sendCall struct {
	id  string // cfg["id"], identifies the service
	msg Message
}

type fakeSender struct {
	mu      sync.Mutex
	calls   []sendCall
	err     error
	block   chan struct{} // when non-nil, Send blocks until it is closed (ignoring ctx)
	entered chan struct{} // receives once per call, if non-nil
}

func (f *fakeSender) Send(ctx context.Context, _ string, cfg map[string]string, msg Message) error {
	f.mu.Lock()
	f.calls = append(f.calls, sendCall{cfg["id"], msg})
	err, block, entered := f.err, f.block, f.entered
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	return err
}

func (f *fakeSender) callsFor(id string) []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sendCall
	for _, c := range f.calls {
		if c.id == id {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSender) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newDispatchStore(t *testing.T) *storage.SQLiteStorage {
	t.Helper()
	st, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("init storage: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func addService(t *testing.T, st storage.NotifyStore, id, delivery string, enabled bool, kinds ...EventKind) int64 {
	t.Helper()
	evs := make([]string, len(kinds))
	for i, k := range kinds {
		evs[i] = string(k)
	}
	sid, err := st.SaveNotifyService(context.Background(), storage.NotifyServiceRow{
		Name: id, Type: "webhook", Delivery: delivery, Enabled: enabled, Events: evs,
		Config: map[string]string{"id": id},
	})
	if err != nil {
		t.Fatalf("save service: %v", err)
	}
	return sid
}

func pending(t *testing.T, st storage.NotifyStore, sid int64) []storage.NotifyPendingRow {
	t.Helper()
	rows, err := st.ListNotifyPending(context.Background(), sid)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	return rows
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func staticSettings(s Settings) func() Settings { return func() Settings { return s } }

func newVersion(c, from, to string) Event {
	return Event{Kind: EventNewVersion, Container: c, FromVersion: from, ToVersion: to, Level: LevelPatch, At: time.Now()}
}

func TestDispatch_ImmediateVsSummary(t *testing.T) {
	st := newDispatchStore(t)
	imm := addService(t, st, "imm", "immediate", true, EventNewVersion, EventUpdateFailed)
	sum := addService(t, st, "sum", "summary", true, EventNewVersion, EventUpdateFailed)
	fs := &fakeSender{}
	d := NewDispatcher(st, fs, staticSettings(DefaultSettings("engager")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	d.Dispatch(ctx, []Event{newVersion("sonarr", "4.0.9", "4.0.10")})
	waitFor(t, "immediate send", func() bool { return len(fs.callsFor("imm")) == 1 })
	if got := fs.callsFor("imm")[0].msg.Title; got != "[engager] 1 container update available" {
		t.Fatalf("title %q", got)
	}
	rows := pending(t, st, sum)
	if len(rows) != 1 || rows[0].DedupKey != "new_version|sonarr" || rows[0].Kind != "new_version" || rows[0].ContainerName != "sonarr" {
		t.Fatalf("summary pending rows %+v", rows)
	}
	if len(pending(t, st, imm)) != 0 {
		t.Fatal("immediate service got a pending row")
	}

	d.Dispatch(ctx, []Event{{Kind: EventUpdateFailed, Container: "sonarr", OperationID: "op1", At: time.Now()}})
	waitFor(t, "update_failed to summary service", func() bool { return len(fs.callsFor("sum")) == 1 })
	if got := fs.callsFor("sum")[0].msg.Title; got != "[engager] Update failed: sonarr" {
		t.Fatalf("title %q", got)
	}
	waitFor(t, "update_failed to immediate service", func() bool { return len(fs.callsFor("imm")) == 2 })
	if n := len(pending(t, st, sum)); n != 1 {
		t.Fatalf("summary pending rows = %d, want 1", n)
	}
}

func TestDispatch_GroupsEventsPerService(t *testing.T) {
	st := newDispatchStore(t)
	addService(t, st, "imm", "immediate", true, EventNewVersion)
	fs := &fakeSender{}
	d := NewDispatcher(st, fs, staticSettings(DefaultSettings("engager")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	d.Dispatch(ctx, []Event{newVersion("a", "1.0.0", "1.0.1"), newVersion("b", "1.0.0", "1.0.1")})
	waitFor(t, "send", func() bool { return fs.total() == 1 })
	if got := fs.callsFor("imm")[0].msg.Title; got != "[engager] 2 container updates available" {
		t.Fatalf("title %q", got)
	}
}

func TestDispatch_PendingDedupKeys(t *testing.T) {
	st := newDispatchStore(t)
	sum := addService(t, st, "sum", "summary", true, EventDigestChanged, EventUpdateSucceeded)
	d := NewDispatcher(st, &fakeSender{}, staticSettings(DefaultSettings("h")))
	at := time.Unix(1700000000, 0)
	d.Dispatch(context.Background(), []Event{
		{Kind: EventDigestChanged, Container: "a", At: at},
		{Kind: EventUpdateSucceeded, Container: "a", OperationID: "op1", At: at},
		{Kind: EventUpdateSucceeded, Container: "b", At: at},
	})
	keys := map[string]bool{}
	for _, r := range pending(t, st, sum) {
		keys[r.DedupKey] = true
	}
	for _, k := range []string{"digest_changed|a", "update_succeeded|a|op1", "update_succeeded|b|1700000000"} {
		if !keys[k] {
			t.Errorf("missing dedup key %q (have %v)", k, keys)
		}
	}
}

func TestDispatch_EventFilterAndDisabled(t *testing.T) {
	st := newDispatchStore(t)
	off := addService(t, st, "off", "immediate", false, AllEventKinds...)
	offSum := addService(t, st, "offsum", "summary", false, AllEventKinds...)
	other := addService(t, st, "other", "immediate", true, EventUpdateFailed)
	otherSum := addService(t, st, "othersum", "summary", true, EventUpdateFailed)
	d := NewDispatcher(st, &fakeSender{}, staticSettings(DefaultSettings("h")))

	// Not started: routing is synchronous, so the queue shows exactly what was enqueued.
	d.Dispatch(context.Background(), []Event{newVersion("sonarr", "1.0.0", "1.0.1")})
	if n := len(d.work); n != 0 {
		t.Fatalf("enqueued %d sends, want 0", n)
	}
	for _, sid := range []int64{off, offSum, other, otherSum} {
		if n := len(pending(t, st, sid)); n != 0 {
			t.Fatalf("service %d has %d pending rows", sid, n)
		}
	}
}

func TestFlush_PrunesAppliedAndSuperseded(t *testing.T) {
	st := newDispatchStore(t)
	sum := addService(t, st, "sum", "summary", true, EventNewVersion)
	fs := &fakeSender{}
	d := NewDispatcher(st, fs, staticSettings(DefaultSettings("engager")))
	ctx := context.Background()

	d.Dispatch(ctx, []Event{newVersion("sonarr", "4.0.9", "4.0.10"), newVersion("radarr", "5.0.0", "5.1.0")})
	d.Dispatch(ctx, []Event{newVersion("sonarr", "4.0.9", "4.0.11"), newVersion("gone", "1.0.0", "1.0.1")})
	current := func(c string) (string, bool) {
		switch c {
		case "sonarr":
			return "4.0.9", true
		case "radarr":
			return "5.1.0", true // already applied
		}
		return "", false
	}
	now := time.Now()
	d.FlushSummaries(ctx, current, now)

	calls := fs.callsFor("sum")
	if len(calls) != 1 {
		t.Fatalf("sends = %d, want 1", len(calls))
	}
	msg := calls[0].msg
	if msg.Title != "[engager] Docksmith summary: 1 item" {
		t.Fatalf("title %q", msg.Title)
	}
	var lines []string
	for _, l := range strings.Split(msg.Text, "\n") {
		if strings.Contains(l, "sonarr") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "4.0.11") || strings.Contains(msg.Text, "4.0.10") {
		t.Fatalf("want one sonarr line with 4.0.11, got:\n%s", msg.Text)
	}
	if strings.Contains(msg.Text, "radarr") || strings.Contains(msg.Text, "gone") {
		t.Fatalf("pruned containers in summary:\n%s", msg.Text)
	}
	if n := len(pending(t, st, sum)); n != 0 {
		t.Fatalf("pending rows after flush = %d", n)
	}
	row, _, _ := st.GetNotifyService(ctx, sum)
	if row.LastSummaryAt == nil || row.LastSuccessAt == nil {
		t.Fatalf("summary/success time not recorded: %+v", row)
	}

	// Only an already-applied update pending: nothing is sent.
	d.Dispatch(ctx, []Event{newVersion("radarr", "5.0.0", "5.1.0")})
	d.FlushSummaries(ctx, current, now)
	if n := fs.total(); n != 1 {
		t.Fatalf("sends = %d, want still 1", n)
	}
	if n := len(pending(t, st, sum)); n != 0 {
		t.Fatalf("stale pending rows left: %d", n)
	}
}

func TestFlush_EmptySendsNothing(t *testing.T) {
	st := newDispatchStore(t)
	sum := addService(t, st, "sum", "summary", true, AllEventKinds...)
	fs := &fakeSender{}
	d := NewDispatcher(st, fs, staticSettings(DefaultSettings("h")))
	d.FlushSummaries(context.Background(), func(string) (string, bool) { return "", false }, time.Now())
	if n := fs.total(); n != 0 {
		t.Fatalf("sends = %d", n)
	}
	row, _, _ := st.GetNotifyService(context.Background(), sum)
	if row.LastSummaryAt != nil {
		t.Fatal("summary time recorded for empty flush")
	}
}

func TestFlush_FailureKeepsPending(t *testing.T) {
	defer setRetryDelays(time.Millisecond, time.Millisecond)()
	st := newDispatchStore(t)
	sum := addService(t, st, "sum", "summary", true, EventUpdateSucceeded)
	fs := &fakeSender{err: errors.New("smtp down")}
	d := NewDispatcher(st, fs, staticSettings(DefaultSettings("h")))
	ctx := context.Background()
	d.Dispatch(ctx, []Event{{Kind: EventUpdateSucceeded, Container: "a", OperationID: "op", At: time.Now()}})
	d.FlushSummaries(ctx, nil, time.Now())
	if n := len(pending(t, st, sum)); n != 1 {
		t.Fatalf("pending rows = %d, want 1 kept", n)
	}
	row, _, _ := st.GetNotifyService(ctx, sum)
	if row.LastSummaryAt != nil || row.LastError != "smtp down" {
		t.Fatalf("unexpected service state: %+v", row)
	}
}

func TestDispatch_DoesNotBlockOnHungSender(t *testing.T) {
	st := newDispatchStore(t)
	addService(t, st, "imm", "immediate", true, AllEventKinds...)
	release := make(chan struct{})
	defer close(release)
	fs := &fakeSender{block: release, entered: make(chan struct{}, 1)}
	d := NewDispatcher(st, fs, staticSettings(DefaultSettings("h")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	d.Dispatch(ctx, []Event{newVersion("a", "1.0.0", "1.0.1")})
	select {
	case <-fs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("sender never called")
	}
	// Every worker is now hung or about to be; flood well past the queue size.
	for i := 0; i < 3*workQueueSize; i++ {
		start := time.Now()
		d.Dispatch(ctx, []Event{newVersion("a", "1.0.0", "1.0.1")})
		if el := time.Since(start); el > 100*time.Millisecond {
			t.Fatalf("Dispatch #%d took %v", i, el)
		}
	}
}

func TestDispatch_SafeAfterShutdown(t *testing.T) {
	st := newDispatchStore(t)
	addService(t, st, "imm", "immediate", true, AllEventKinds...)
	d := NewDispatcher(st, &fakeSender{}, staticSettings(DefaultSettings("h")))
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 2*workQueueSize; j++ {
					d.Dispatch(context.Background(), []Event{newVersion("a", "1.0.0", "1.0.1")})
				}
			}()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Dispatch blocked after shutdown")
	}
}

func TestSend_FailureRecorded(t *testing.T) {
	defer setRetryDelays(time.Millisecond, time.Millisecond)()
	st := newDispatchStore(t)
	imm := addService(t, st, "imm", "immediate", true, EventUpdateFailed)
	fs := &fakeSender{err: errors.New("connection refused")}
	d := NewDispatcher(st, fs, staticSettings(DefaultSettings("h")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	d.Dispatch(ctx, []Event{{Kind: EventUpdateFailed, Container: "a", At: time.Now()}})
	waitFor(t, "last_error recorded", func() bool {
		row, _, _ := st.GetNotifyService(ctx, imm)
		return row.LastError == "connection refused" && row.LastErrorAt != nil
	})
	if n := fs.total(); n != 3 {
		t.Fatalf("attempts = %d, want 3 (1 + 2 retries)", n)
	}
}

func TestSendTest(t *testing.T) {
	st := newDispatchStore(t)
	imm := addService(t, st, "imm", "summary", false, EventNewVersion)
	fs := &fakeSender{}
	d := NewDispatcher(st, fs, staticSettings(DefaultSettings("engager")))
	if err := d.SendTest(context.Background(), imm); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if c := fs.callsFor("imm"); len(c) != 1 || !strings.HasPrefix(c[0].msg.Title, "[engager] ") {
		t.Fatalf("calls %+v", c)
	}
	if err := d.SendTest(context.Background(), 9999); err == nil {
		t.Fatal("expected error for unknown service")
	}
	fs.err = errors.New("bad token")
	if err := d.SendTest(context.Background(), imm); err == nil || err.Error() != "bad token" {
		t.Fatalf("err = %v", err)
	}
	row, _, _ := st.GetNotifyService(context.Background(), imm)
	if row.LastError != "bad token" {
		t.Fatalf("last_error %q", row.LastError)
	}
}

func TestScheduler_FiresWhenDueAndFollowsCronChanges(t *testing.T) {
	set := DefaultSettings("h") // "0 8 * * *"
	var mu sync.Mutex
	settings := func() Settings { mu.Lock(); defer mu.Unlock(); return set }
	s := NewScheduler(nil, settings, nil)
	var fired []time.Time
	s.flush = func(_ context.Context, _ CurrentVersions, now time.Time) { fired = append(fired, now) }

	at := func(h, m int) time.Time { return time.Date(2026, 10, 26, h, m, 0, 0, time.Local) }
	ctx := context.Background()
	s.tick(ctx, at(7, 58)) // computes next = 08:00
	s.tick(ctx, at(7, 59))
	if len(fired) != 0 {
		t.Fatalf("fired early: %v", fired)
	}
	s.tick(ctx, at(8, 0))
	s.tick(ctx, at(8, 1))
	if len(fired) != 1 || !fired[0].Equal(at(8, 0)) {
		t.Fatalf("fired = %v, want once at 08:00", fired)
	}

	mu.Lock()
	set.SummaryCron = "30 8 * * *"
	mu.Unlock()
	s.tick(ctx, at(8, 2)) // recompute: next = 08:30
	s.tick(ctx, at(8, 29))
	s.tick(ctx, at(8, 31)) // late tick still fires
	if len(fired) != 2 || !fired[1].Equal(at(8, 31)) {
		t.Fatalf("fired = %v, want second fire at 08:31", fired)
	}

	mu.Lock()
	set.SummaryCron = "not a cron"
	mu.Unlock()
	s.tick(ctx, at(9, 0))
	s.tick(ctx, time.Date(2026, 10, 27, 9, 0, 0, 0, time.Local))
	if len(fired) != 2 {
		t.Fatalf("invalid cron fired: %v", fired)
	}
}

// setRetryDelays shortens the retry backoff for a test and returns a restore func.
func setRetryDelays(d ...time.Duration) func() {
	old := retryDelays
	retryDelays = d
	return func() { retryDelays = old }
}
