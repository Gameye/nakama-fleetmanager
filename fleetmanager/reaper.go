package fleetmanager

import (
	"context"
	"fmt"
	"time"

	"github.com/Gameye/nakama-fleetmanager/gameye"
)

// startReaper starts the reaper on the fleet manager's context (the
// InitModule context, never a hook's) unless it is disabled or running.
func (fm *GameyeFleetManager) startReaper() {
	if fm.config.ReapInterval <= 0 || fm.reaperDone != nil {
		return
	}

	ctx := fm.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	done := make(chan struct{})
	fm.reaperDone = done
	go fm.runReaper(ctx, fm.config.ReapInterval, done)
}

func (fm *GameyeFleetManager) runReaper(ctx context.Context, interval time.Duration, done chan<- struct{}) {
	defer close(done)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := fm.reap(ctx); err != nil && ctx.Err() == nil {
				fm.logger.Warn("gameye reaper: %v", err)
			}
		}
	}
}

// reap deletes stored instances whose session Gameye no longer runs. Storage
// is read before Gameye is listed, so a session stored mid-cycle (after the
// read) is never compared against a listing taken before it started.
func (fm *GameyeFleetManager) reap(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, fm.config.CreateTimeout)
	defer cancel()

	stored, err := fm.storage.ListIds(ctx)
	if err != nil {
		return fmt.Errorf("error listing stored instances: %w", err)
	}
	if len(stored) == 0 {
		return nil
	}

	// No filters: rows may belong to sessions started under an earlier
	// region, image or version.
	sessions, err := fm.apiClient.SessionList(ctx, gameye.SessionList{})
	if err != nil {
		return fmt.Errorf("error listing sessions: %w", err)
	}

	live := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		if isLive(session.Status) {
			live[session.ID] = true
		}
	}

	var gone []string
	for _, id := range stored {
		if !live[id] {
			gone = append(gone, id)
		}
	}
	if len(gone) == 0 {
		return nil
	}

	if err := fm.storage.Delete(ctx, gone); err != nil {
		return fmt.Errorf("error deleting %d ended sessions from storage: %w", len(gone), err)
	}
	fm.logger.Info("gameye reaper removed %d ended sessions from storage", len(gone))
	return nil
}
