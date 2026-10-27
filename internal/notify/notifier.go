package notify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chis/docksmith/internal/events"
	"github.com/chis/docksmith/internal/storage"
	"github.com/chis/docksmith/internal/update"
)

const settingsLoadTimeout = 5 * time.Second

// Notifier ties together change detection, operation events, the dispatcher
// and the summary scheduler. All methods are safe for concurrent use.
type Notifier struct {
	store         storage.NotifyStore
	cfg           ConfigStore
	checkInterval time.Duration

	settings   atomic.Pointer[Settings] // immutable once stored; Settings() hands out copies
	dispatcher *Dispatcher
	scheduler  *Scheduler
	watcher    *OperationWatcher

	ctxMu sync.RWMutex
	ctx   context.Context // set by Start

	checkMu sync.Mutex // serialises OnCheckResult

	lastMu       sync.RWMutex
	lastLabels   map[string]map[string]string // container -> labels from the most recent check
	lastVersions map[string]string            // container -> current version from the most recent check
}

// New creates a notifier. Settings are loaded from cfg (defaults on error).
func New(store storage.NotifyStore, cfg ConfigStore, ops OperationLookup, bus *events.Bus, sender Sender, hostname string, checkInterval time.Duration) *Notifier {
	n := &Notifier{store: store, cfg: cfg, checkInterval: checkInterval}

	ctx, cancel := context.WithTimeout(context.Background(), settingsLoadTimeout)
	defer cancel()
	s, err := LoadSettings(ctx, cfg, hostname)
	if err != nil {
		log.Printf("notify: %v; using defaults", err)
		s = DefaultSettings(hostname)
	}
	n.settings.Store(&s)

	n.dispatcher = NewDispatcher(store, sender, n.Settings)
	n.scheduler = NewScheduler(n.dispatcher, n.Settings, n.currentVersion)
	n.watcher = NewOperationWatcher(bus, ops, n.emit)
	return n
}

// Start launches the dispatcher, summary scheduler and operation watcher; they
// run until ctx is done. It does not block.
func (n *Notifier) Start(ctx context.Context) {
	n.ctxMu.Lock()
	n.ctx = ctx
	n.ctxMu.Unlock()
	n.dispatcher.Start(ctx)
	n.scheduler.Start(ctx)
	n.watcher.Start(ctx)
}

// runCtx returns the context passed to Start, or nil before Start.
func (n *Notifier) runCtx() context.Context {
	n.ctxMu.RLock()
	defer n.ctxMu.RUnlock()
	return n.ctx
}

// emit dispatches an operation event unless its container is muted. It never
// blocks, and drops events once the notifier has shut down.
func (n *Notifier) emit(e Event) {
	ctx := n.runCtx()
	if ctx == nil || ctx.Err() != nil {
		return
	}
	n.lastMu.RLock()
	muted := Muted(n.lastLabels[e.Container])
	n.lastMu.RUnlock()
	if muted {
		return
	}
	go n.dispatcher.Dispatch(ctx, []Event{e})
}

// currentVersion answers from the most recent check result.
func (n *Notifier) currentVersion(container string) (string, bool) {
	n.lastMu.RLock()
	defer n.lastMu.RUnlock()
	v, ok := n.lastVersions[container]
	return v, ok
}

// OnCheckResult detects notifiable changes in a check result, persists the new
// per-container state and dispatches the events. The first result ever seen
// only seeds the state, without notifying.
func (n *Notifier) OnCheckResult(ctx context.Context, r *update.DiscoveryResult) {
	if r == nil {
		return
	}
	if rc := n.runCtx(); rc != nil && rc.Err() != nil {
		return
	}

	snaps := make([]Snapshot, 0, len(r.Containers))
	labels := make(map[string]map[string]string, len(r.Containers))
	versions := make(map[string]string, len(r.Containers))
	for _, c := range r.Containers {
		snaps = append(snaps, Snapshot{
			Name:                  c.ContainerName,
			Stack:                 c.Stack,
			Status:                string(c.Status),
			CurrentVersion:        c.CurrentVersion,
			LatestVersion:         c.LatestVersion,
			LatestResolvedVersion: c.LatestResolvedVersion,
			CurrentDigest:         c.CurrentDigest,
			LatestDigest:          c.LatestDigest,
			Labels:                c.Labels,
		})
		labels[c.ContainerName] = c.Labels
		versions[c.ContainerName] = c.CurrentVersion
	}
	n.lastMu.Lock()
	n.lastLabels, n.lastVersions = labels, versions
	n.lastMu.Unlock()

	n.checkMu.Lock()
	defer n.checkMu.Unlock()

	prev, err := n.store.LoadNotifyContainerStates(ctx)
	if err != nil {
		log.Printf("notify: load container states: %v", err)
		return
	}
	evs, next := Detect(prev, snaps, n.Settings(), time.Now())
	if err := n.store.ReplaceNotifyContainerStates(ctx, next); err != nil {
		log.Printf("notify: save container states: %v", err)
		return // nothing recorded, so the same events are detected next time
	}
	for name := range prev {
		if _, ok := next[name]; ok {
			continue
		}
		if err := n.store.DeleteNotifyPendingForContainer(ctx, name); err != nil {
			log.Printf("notify: delete pending for removed container %q: %v", name, err)
		}
	}

	_, seeded, err := n.cfg.GetConfig(ctx, storage.NotifySeededKey)
	if err != nil {
		log.Printf("notify: read seeded flag: %v", err)
		return
	}
	if !seeded {
		if err := n.cfg.SetConfig(ctx, storage.NotifySeededKey, "true"); err != nil {
			log.Printf("notify: set seeded flag: %v", err)
		}
		log.Printf("notify: seeded state for %d containers without notifying", len(next))
		return
	}
	n.dispatcher.Dispatch(ctx, evs)
}

// Settings returns a copy of the current settings.
func (n *Notifier) Settings() Settings {
	return cloneSettings(*n.settings.Load())
}

func cloneSettings(s Settings) Settings {
	t := make(map[EventKind]Timing, len(s.Timing))
	for k, v := range s.Timing {
		t[k] = v
	}
	s.Timing = t
	return s
}

// UpdateSettings validates s and saves it; nothing is saved when invalid.
// Missing timing entries are filled from the defaults.
func (n *Notifier) UpdateSettings(ctx context.Context, s Settings) error {
	s = cloneSettings(s)
	if err := n.validate(&s); err != nil {
		return err
	}
	for k, v := range defaultTiming() {
		if _, ok := s.Timing[k]; !ok {
			s.Timing[k] = v
		}
	}
	if err := SaveSettings(ctx, n.cfg, s); err != nil {
		return fmt.Errorf("save notify settings: %w", err)
	}
	n.settings.Store(&s)
	return nil
}

func (n *Notifier) validate(s *Settings) error {
	s.SummaryCron = strings.TrimSpace(s.SummaryCron)
	if s.SummaryCron == "" {
		return errors.New("summary schedule is required")
	}
	if err := ValidateSchedule(s.SummaryCron, n.checkInterval, time.Local, time.Now()); err != nil {
		return err
	}
	if _, err := ParseLevel(s.MinLevel); err != nil {
		return fmt.Errorf("invalid minimum level: %w", err)
	}
	if err := validateBaseURL(s.BaseURL); err != nil {
		return err
	}
	known := make(map[EventKind]bool, len(AllEventKinds))
	for _, k := range AllEventKinds {
		known[k] = true
	}
	for k, v := range s.Timing {
		if !known[k] {
			return fmt.Errorf("unknown event kind %q", k)
		}
		if v != TimingImmediate && v != TimingSummary {
			return fmt.Errorf("invalid timing %q for %s: must be %q or %q", v, k, TimingImmediate, TimingSummary)
		}
	}
	return nil
}

func validateBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("base URL must be an absolute http(s) URL: %q", raw)
	}
	if strings.ContainsAny(raw, "?#") {
		return fmt.Errorf("base URL must not contain a query or fragment: %q", raw)
	}
	return nil
}

// CheckInterval returns the background check interval used to validate schedules.
func (n *Notifier) CheckInterval() time.Duration { return n.checkInterval }

// Dispatcher returns the notifier's dispatcher.
func (n *Notifier) Dispatcher() *Dispatcher { return n.dispatcher }
