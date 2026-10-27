package server

import (
	"context"
	"errors"
	"time"

	"interim/internal/store"
)

const (
	janitorInterval         = time.Minute
	deepSweepInterval       = 30 * time.Minute
	orphanObjectGrace       = 15 * time.Minute
	terminalIntentRetention = 24 * time.Hour
	janitorBatchSize        = 100
	janitorBatchesPerPass   = 10
)

func (s *Server) cleanupExpiredUploadIntents(ctx context.Context) (int, error) {
	now := s.now().UTC()
	total := 0
	var firstErr error
	for batch := 0; batch < janitorBatchesPerPass; batch++ {
		intents, err := s.store.ExpiredPendingUploadIntents(ctx, now, janitorBatchSize)
		if err != nil {
			return total, err
		}
		if len(intents) == 0 {
			break
		}
		for _, intent := range intents {
			if err := s.abortMultipartObject(intent.ObjectKey, intent.MultipartUploadID); err != nil {
				s.logger.Error("janitor could not abort expired multipart upload", "upload_id", intent.ID, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := s.objects.Delete(deleteCtx, intent.ObjectKey)
			cancel()
			if err != nil {
				s.logger.Error("janitor could not delete expired upload object", "upload_id", intent.ID, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if err := s.store.CloseUploadIntent(ctx, intent.ID, "expired", now); err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					s.logger.Error("janitor could not close expired upload intent", "upload_id", intent.ID, "error", err)
					if firstErr == nil {
						firstErr = err
					}
				}
				continue
			}
			total++
		}
		if len(intents) < janitorBatchSize {
			break
		}
	}
	return total, firstErr
}

func (s *Server) cleanupTerminalUploadIntents(ctx context.Context) (int64, error) {
	before := s.now().UTC().Add(-terminalIntentRetention)
	var total int64
	for batch := 0; batch < janitorBatchesPerPass; batch++ {
		removed, err := s.store.DeleteTerminalUploadIntents(ctx, before, janitorBatchSize)
		if err != nil {
			return total, err
		}
		total += removed
		if removed < janitorBatchSize {
			break
		}
	}
	return total, nil
}

// deepSweep is the safety net for objects that no longer have a live database
// owner. At startup no browser upload can be in flight, so unknown objects are
// deleted immediately. During runtime a grace period protects the tiny window
// between an S3 PUT completing and the completion transaction committing.
func (s *Server) deepSweep(ctx context.Context, startup bool) (int, error) {
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	objectsList, err := s.objects.List(listCtx, s.cfg.S3.Prefix)
	if err != nil {
		return 0, err
	}
	known, err := s.store.KnownObjectKeys(ctx, s.now())
	if err != nil {
		return 0, err
	}
	// Refuse the catastrophic case outright: a fresh or restored-empty database
	// must never reinterpret an entire bucket as garbage. The ratio guard also
	// stops a partially restored database or a bucket shared with another
	// instance from deleting most of the objects in one pass.
	if len(objectsList) > 0 && len(known) == 0 {
		return 0, errors.New("refusing orphan sweep: database knows no objects under the prefix")
	}
	if len(known) > 0 && len(objectsList)-len(known) > len(known) {
		return 0, errors.New("refusing orphan sweep: more than half of the listed objects are unknown")
	}
	now := s.now().UTC()
	deleted := 0
	var firstErr error
	for _, object := range objectsList {
		if _, protected := known[object.Key]; protected {
			continue
		}
		if !startup {
			if object.LastModified.IsZero() || now.Sub(object.LastModified) < orphanObjectGrace {
				continue
			}
		}
		deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.objects.Delete(deleteCtx, object.Key)
		cancel()
		if err != nil {
			s.logger.Error("janitor could not delete orphan object", "key", object.Key, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.logger.Warn("janitor deleted orphan object", "key", object.Key, "size", object.Size)
		deleted++
	}

	uploads, listErr := s.objects.ListMultipartUploads(listCtx, s.cfg.S3.Prefix)
	if listErr != nil {
		s.logger.Warn("janitor could not list multipart uploads", "error", listErr)
		return deleted, firstErr
	}
	knownUploads, err := s.store.KnownMultipartUploadIDs(ctx)
	if err != nil {
		if firstErr == nil {
			firstErr = err
		}
		return deleted, firstErr
	}
	for _, upload := range uploads {
		if _, ok := knownUploads[upload.UploadID]; ok {
			continue
		}
		if !startup && (upload.Initiated.IsZero() || now.Sub(upload.Initiated) < orphanObjectGrace) {
			continue
		}
		abortCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.objects.AbortMultipart(abortCtx, upload.Key, upload.UploadID)
		cancel()
		if err != nil {
			s.logger.Error("janitor could not abort orphan multipart upload", "key", upload.Key, "upload_id", upload.UploadID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.logger.Warn("janitor aborted orphan multipart upload", "key", upload.Key, "upload_id", upload.UploadID)
	}
	return deleted, firstErr
}

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

// MaintenanceOnce is the fast lane: expired intents, expired files, sessions,
// and old terminal intent rows. It runs every minute and at startup.
func (s *Server) MaintenanceOnce(ctx context.Context) {
	if count, err := s.cleanupExpiredUploadIntents(ctx); err != nil {
		s.logger.Warn("expired upload intent cleanup incomplete", "error", err)
	} else if count > 0 {
		s.logger.Info("janitor expired uploads", "count", count)
	}
	if deleted, err := s.cleanupExpiredFiles(ctx); err != nil {
		s.logger.Warn("expired file cleanup incomplete", "error", err)
	} else if deleted > 0 {
		s.logger.Info("janitor deleted expired files", "count", deleted)
	}
	if removed, err := s.cleanupTerminalUploadIntents(ctx); err != nil {
		s.logger.Warn("terminal upload intent cleanup failed", "error", err)
	} else if removed > 0 {
		s.logger.Info("janitor removed terminal upload intents", "count", removed)
	}
	if removed, err := s.store.ClearExpiredReservations(ctx, s.now()); err != nil {
		s.logger.Warn("expired reservation cleanup failed", "error", err)
	} else if removed > 0 {
		s.logger.Info("janitor cleared expired upload reservations", "count", removed)
	}
	if removed, err := s.store.DeleteExpiredSessions(ctx, s.now()); err != nil {
		s.logger.Warn("expired session cleanup failed", "error", err)
	} else if removed > 0 {
		s.logger.Info("janitor cleared expired admin sessions", "count", removed)
	}
	if s.cfg.AdminIdleTimeout > 0 {
		if removed, err := s.store.DeleteIdleSessions(ctx, s.now().Add(-s.cfg.AdminIdleTimeout)); err != nil {
			s.logger.Warn("idle session cleanup failed", "error", err)
		} else if removed > 0 {
			s.logger.Info("janitor cleared idle admin sessions", "count", removed)
		}
	}
}

// StartupMaintenance runs before the HTTP listener and performs both the fast
// cleanup and a zero-grace orphan sweep.
func (s *Server) StartupMaintenance(ctx context.Context) {
	s.MaintenanceOnce(ctx)
	if !s.cfg.SweepOnStartup {
		s.logger.Info("startup orphan sweep disabled; set SWEEP_ON_STARTUP=true to enable it")
		return
	}
	if deleted, err := s.deepSweep(ctx, true); err != nil {
		s.logger.Warn("startup orphan object sweep failed", "error", err)
	} else if deleted > 0 {
		s.logger.Info("janitor deleted orphan objects at startup", "count", deleted)
	}
}

func (s *Server) RunMaintenance(ctx context.Context) {
	fast := time.NewTicker(janitorInterval)
	defer fast.Stop()
	deep := time.NewTicker(deepSweepInterval)
	defer deep.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-fast.C:
			s.MaintenanceOnce(ctx)
		case <-deep.C:
			if deleted, err := s.deepSweep(ctx, false); err != nil {
				s.logger.Warn("orphan object sweep failed", "error", err)
			} else if deleted > 0 {
				s.logger.Info("janitor deleted orphan objects", "count", deleted)
			}
		}
	}
}
