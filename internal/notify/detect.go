package notify

import (
	"fmt"
	"time"

	"github.com/chis/docksmith/internal/scripts"
	"github.com/chis/docksmith/internal/storage"
	"github.com/chis/docksmith/internal/version"
)

// Status strings mirror internal/update (kept local to avoid an import cycle).
const (
	statusUpdateAvailable        = "UPDATE_AVAILABLE"
	statusUpdateAvailableBlocked = "UPDATE_AVAILABLE_BLOCKED"
	statusCheckFailed            = "CHECK_FAILED"
	statusMetadataUnavailable    = "METADATA_UNAVAILABLE"
	statusComposeMismatch        = "COMPOSE_MISMATCH"
)

// checkFailThreshold is the number of consecutive failed checks before notifying.
const checkFailThreshold = 3

// Snapshot is the part of a check result that Detect needs.
type Snapshot struct {
	Name, Stack, Status, CurrentVersion, LatestVersion, LatestResolvedVersion, CurrentDigest, LatestDigest string
	Labels                                                                                                 map[string]string
}

// Muted reports whether notifications are disabled for the container labels.
func Muted(labels map[string]string) bool {
	return labels[scripts.NotifyLabel] == "false"
}

// Detect diffs check results against prev and returns the events to emit plus the new state.
// It performs no I/O and does not modify prev. Muted containers are tracked but emit nothing.
func Detect(prev map[string]storage.NotifyContainerState, snaps []Snapshot, s Settings, now time.Time) (events []Event, next map[string]storage.NotifyContainerState) {
	next = make(map[string]storage.NotifyContainerState, len(snaps))
	for _, sn := range snaps {
		st := prev[sn.Name]
		st.ContainerName = sn.Name
		muted := Muted(sn.Labels)
		emit := func(e Event) {
			if muted {
				return
			}
			e.Container, e.Stack, e.At = sn.Name, sn.Stack, now
			events = append(events, e)
		}
		blockedStatus := sn.Status == statusUpdateAvailableBlocked

		target := sn.LatestResolvedVersion
		if target == "" {
			target = sn.LatestVersion
		}

		if sn.Status == statusUpdateAvailable || blockedStatus {
			if lvl, ok := Classify(sn.CurrentVersion, target); ok {
				if lvl >= effectiveMinLevel(sn.Labels, s) && target != st.LastNotifiedVersion {
					emit(Event{Kind: EventNewVersion, FromVersion: sn.CurrentVersion, ToVersion: target, Level: lvl})
					st.LastNotifiedVersion = target
				}
			} else if comparisonUnknown(sn.CurrentVersion, target) && digestEnabled(sn.Labels, s) && sn.LatestDigest != "" &&
				sn.LatestDigest != sn.CurrentDigest && sn.LatestDigest != st.LastNotifiedDigest {
				emit(Event{Kind: EventDigestChanged, FromVersion: sn.CurrentVersion, ToVersion: target})
				st.LastNotifiedDigest = sn.LatestDigest
			}
		}

		if blockedStatus && !st.Blocked {
			emit(Event{Kind: EventUpdateBlocked, FromVersion: sn.CurrentVersion, ToVersion: target, Message: "Update blocked by pre-update check"})
		}
		st.Blocked = blockedStatus

		if sn.Status == statusCheckFailed || sn.Status == statusMetadataUnavailable {
			st.CheckFailCount++
			if st.CheckFailCount >= checkFailThreshold && !st.CheckFailNotified {
				emit(Event{Kind: EventCheckFailing, Message: fmt.Sprintf("Version check failing for %d consecutive checks", st.CheckFailCount)})
				st.CheckFailNotified = true
			}
		} else {
			st.CheckFailCount, st.CheckFailNotified = 0, false
		}

		mismatch := sn.Status == statusComposeMismatch
		if mismatch && !st.ComposeMismatch {
			emit(Event{Kind: EventComposeMismatch, Message: "Running image does not match compose file"})
		}
		st.ComposeMismatch = mismatch

		next[sn.Name] = st
	}
	return events, next
}

// comparisonUnknown reports whether current and target cannot be compared as versions
// (empty or unparseable tags). Downgrades and equal versions are known comparisons.
func comparisonUnknown(current, target string) bool {
	p := version.NewParser()
	from, to := p.ParseTag(current), p.ParseTag(target)
	if from == nil || to == nil {
		return true
	}
	return version.NewComparator().GetChangeType(from, to) == version.UnknownChange
}

func effectiveMinLevel(labels map[string]string, s Settings) Level {
	if l, err := ParseLevel(labels[scripts.NotifyLevelLabel]); err == nil {
		return l
	}
	if l, err := ParseLevel(s.MinLevel); err == nil {
		return l
	}
	return LevelPatch
}

func digestEnabled(labels map[string]string, s Settings) bool {
	switch labels[scripts.NotifyDigestLabel] {
	case "true":
		return true
	case "false":
		return false
	}
	return s.NotifyDigest
}
