package server

import (
	"context"
	"time"
)

func (s *Server) cleanupExpiredFiles(ctx context.Context) (int, error) {
	files, err := s.store.ExpiredFiles(ctx, s.now(), 100)
	if err != nil {
		return 0, err
	}
	deleted := 0
	var firstErr error
	for _, file := range files {
		deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.objects.Delete(deleteCtx, file.ObjectKey)
		cancel()
		if err != nil {
			s.logger.Error("delete expired object", "file_id", file.ID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed, err := s.store.DeleteExpiredFileRow(ctx, file.ID, s.now())
		if err != nil {
			s.logger.Error("delete expired file row", "file_id", file.ID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if removed {
			deleted++
		}
	}
	return deleted, firstErr
}

// MaintenanceOnce performs a full maintenance pass. Expired files are deleted
// object-first, then their metadata is removed.
func (s *Server) MaintenanceOnce(ctx context.Context) {
	deleted, err := s.cleanupExpiredFiles(ctx)
	if err != nil {
		s.logger.Warn("expired file cleanup incomplete", "error", err)
	}
	if removed, err := s.store.ClearExpiredReservations(ctx, s.now()); err != nil {
		s.logger.Warn("expired reservation cleanup failed", "error", err)
	} else if removed > 0 {
		s.logger.Info("cleared expired upload reservations", "count", removed)
	}
	if removed, err := s.store.DeleteExpiredSessions(ctx, s.now()); err != nil {
		s.logger.Warn("expired session cleanup failed", "error", err)
	} else if removed > 0 {
		s.logger.Info("cleared expired admin sessions", "count", removed)
	}
	if deleted > 0 {
		s.logger.Info("deleted expired files", "count", deleted)
	}
}

func (s *Server) RunMaintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.MaintenanceOnce(ctx)
		}
	}
}
