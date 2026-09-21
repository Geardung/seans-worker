package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Geardung/seans-worker/internal/backend"
	"github.com/Geardung/seans-worker/internal/config"
	"github.com/Geardung/seans-worker/internal/qbit"
	"github.com/Geardung/seans-worker/internal/s3uploader"
)

type Stage string

const (
	StagePreparing   Stage = "preparing"
	StageDownloading Stage = "downloading"
	StageUploading   Stage = "uploading"
	StageDone        Stage = "done"
)

type TaskState struct {
	TaskID       string
	Stage        Stage
	ProgressPct  float64
	SpeedMbps    float64
	ETAMin       int
	Message      string
	TorrentHash  string
	Files        []qbit.TorrentFile
	StartedAt    time.Time
	DownloadStart time.Time
	UploadStart   time.Time
	DownloadSeconds int
	UploadSeconds   int
}

type taskRunner struct {
	cfg      *config.Config
	bc       *backend.Client
	qc       *qbit.Client
	uploader *s3uploader.Uploader
	task     *backend.Task
	state    *TaskState
	logger   *slog.Logger
}

func newTaskRunner(cfg *config.Config, bc *backend.Client, qc *qbit.Client, up *s3uploader.Uploader, task *backend.Task, state *TaskState, logger *slog.Logger) *taskRunner {
	return &taskRunner{
		cfg:      cfg,
		bc:       bc,
		qc:       qc,
		uploader: up,
		task:     task,
		state:    state,
		logger:   logger.With("task_id", task.TaskID),
	}
}

func (tr *taskRunner) run(ctx context.Context) {
	tr.logger.Info("task started", "stage", "preparing")

	if err := tr.prepare(ctx); err != nil {
		tr.failTask(ctx, err)
		return
	}
	if err := tr.download(ctx); err != nil {
		tr.failTask(ctx, err)
		return
	}
	if err := tr.upload(ctx); err != nil {
		tr.failTask(ctx, err)
		return
	}
	tr.complete(ctx)
}

func (tr *taskRunner) prepare(ctx context.Context) error {
	tr.state.Stage = StagePreparing
	tr.state.Message = "preparing task environment"

	taskID := tr.task.TaskID
	stagingPath := filepath.Join(tr.cfg.StagingDir, taskID)
	savePath := filepath.Join(tr.cfg.QBitDownloads, taskID)

	os.RemoveAll(stagingPath)

	existing, err := tr.qc.GetTorrentBySavePath(ctx, "seans", savePath)
	if err != nil {
		return fmt.Errorf("check existing torrents: %w", err)
	}
	if existing != nil {
		tr.logger.Info("removing leftover torrent", "hash", existing.Hash)
		if delErr := tr.qc.DeleteTorrent(ctx, existing.Hash, true); delErr != nil {
			tr.logger.Warn("failed to delete leftover torrent", "error", delErr)
		}
	}

	if err := tr.qc.AddTorrentMagnet(ctx, tr.task.Magnet, savePath, "seans"); err != nil {
		return fmt.Errorf("add magnet: %w", err)
	}

	torrent, err := tr.waitForTorrent(ctx, savePath)
	if err != nil {
		return err
	}
	tr.state.TorrentHash = torrent.Hash

	if torrent.State == "metaDL" {
		tr.logger.Info("waiting for torrent metadata", "timeout", tr.cfg.TorrentMetadataTimeout)
		if err := tr.waitForMetadata(ctx, torrent.Hash); err != nil {
			return err
		}
	}

	files, err := tr.qc.GetFiles(ctx, torrent.Hash)
	if err != nil {
		return fmt.Errorf("get files: %w", err)
	}
	tr.state.Files = files

	// Send manifest to backend and get upload slots
	manifestFiles := make([]backend.ManifestFile, 0, len(files))
	for _, f := range files {
		manifestFiles = append(manifestFiles, backend.ManifestFile{
			Path:      f.Name,
			SizeBytes: f.Size,
		})
	}

	_, err = tr.bc.Manifest(ctx, tr.task.TaskID, manifestFiles)
	if err != nil {
		return fmt.Errorf("send manifest: %w", err)
	}

	tr.logger.Info("preparing complete", "files", len(files))
	return nil
}

func (tr *taskRunner) waitForTorrent(ctx context.Context, savePath string) (*qbit.TorrentInfo, error) {
	deadline := time.After(2 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("torrent did not appear within 2 minutes")
		case <-ticker.C:
			torrent, err := tr.qc.GetTorrentBySavePath(ctx, "seans", savePath)
			if err != nil {
				return nil, err
			}
			if torrent != nil {
				return torrent, nil
			}
		}
	}
}

func (tr *taskRunner) waitForMetadata(ctx context.Context, hash string) error {
	deadline := time.After(tr.cfg.TorrentMetadataTimeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return &TaskError{Reason: "metadata timeout: torrent files not available", Permanent: false}
		case <-ticker.C:
			files, err := tr.qc.GetFiles(ctx, hash)
			if err != nil {
				continue
			}
			if len(files) > 0 {
				return nil
			}
		}
	}
}

func (tr *taskRunner) download(ctx context.Context) error {
	tr.state.Stage = StageDownloading
	tr.state.DownloadStart = time.Now()
	tr.state.Message = "downloading torrent"
	tr.logger.Info("download started")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var lastDownloaded int64
	lastProgressTime := time.Now()
	taskDeadline := time.After(tr.cfg.TaskMaxDuration)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-taskDeadline:
			return &TaskError{Reason: "task timeout exceeded", Permanent: false}
		case <-ticker.C:
			torrent, err := tr.qc.GetTorrentBySavePath(ctx, "seans", filepath.Join(tr.cfg.QBitDownloads, tr.task.TaskID))
			if err != nil {
				tr.logger.Warn("poll error", "error", err)
				continue
			}
			if torrent == nil {
				return fmt.Errorf("torrent disappeared")
			}

			tr.state.ProgressPct = torrent.Progress * 100
			tr.state.SpeedMbps = float64(torrent.DlSpeed) / (1024 * 1024)

			if torrent.Size > 0 && torrent.DlSpeed > 0 {
				remaining := float64(torrent.Size-int64(float64(torrent.Size)*torrent.Progress)) / float64(torrent.DlSpeed)
				tr.state.ETAMin = int(math.Ceil(remaining / 60))
			}

			if torrent.Downloaded > lastDownloaded {
				lastDownloaded = torrent.Downloaded
				lastProgressTime = time.Now()
			}
			if time.Since(lastProgressTime) > tr.cfg.DownloadStallTimeout {
				return &TaskError{Reason: "stalled: no download progress", Permanent: true}
			}

			if torrent.Progress >= 1.0 && !isActiveState(string(torrent.State)) {
				tr.state.ProgressPct = 100
				tr.state.DownloadSeconds = int(time.Since(tr.state.DownloadStart).Seconds())
				tr.logger.Info("download complete", "seconds", tr.state.DownloadSeconds)
				return nil
			}
		}
	}
}

func isActiveState(state string) bool {
	switch state {
	case "downloading", "forcedDL", "stalledDL", "checkingDL", "queuedDL", "metaDL":
		return true
	default:
		return false
	}
}

func (tr *taskRunner) upload(ctx context.Context) error {
	tr.state.Stage = StageUploading
	tr.state.UploadStart = time.Now()
	tr.state.Message = "uploading to S3"
	tr.logger.Info("upload started")

	stagingPath := filepath.Join(tr.cfg.StagingDir, tr.task.TaskID)
	logFile := filepath.Join(tr.cfg.StateDir, "rclone.log")

	if err := tr.uploader.Move(ctx, stagingPath, "users/"+tr.task.TaskID+"/", 3, logFile); err != nil {
		return &TaskError{Reason: fmt.Sprintf("upload failed: %v", err), Permanent: false}
	}

	tr.state.UploadSeconds = int(time.Since(tr.state.UploadStart).Seconds())
	tr.logger.Info("upload complete", "seconds", tr.state.UploadSeconds)

	os.RemoveAll(stagingPath)

	if tr.state.TorrentHash != "" {
		if err := tr.qc.DeleteTorrent(ctx, tr.state.TorrentHash, true); err != nil {
			tr.logger.Warn("failed to delete torrent after upload", "error", err)
		}
	}

	return nil
}

func (tr *taskRunner) complete(ctx context.Context) {
	files := make([]backend.FileInfo, 0)
	for _, f := range tr.state.Files {
		if f.Priority > 0 {
			files = append(files, backend.FileInfo{
				Path:      f.Name,
				SizeBytes: f.Size,
			})
		}
	}

	if err := tr.bc.Complete(ctx, tr.task.TaskID, files); err != nil {
		tr.logger.Error("failed to report completion", "error", err)
		return
	}

	tr.state.Stage = StageDone
	tr.state.Message = "task completed"
	tr.logger.Info("task completed",
		"download_sec", tr.state.DownloadSeconds,
		"upload_sec", tr.state.UploadSeconds,
		"files", len(files),
	)
}

func (tr *taskRunner) failTask(ctx context.Context, err error) {
	var taskErr *TaskError
	reason := err.Error()

	if errors.As(err, &taskErr) {
		reason = taskErr.Reason
	}

	tr.logger.Error("task failed", "reason", reason)
	if reportErr := tr.bc.Fail(ctx, tr.task.TaskID, reason); reportErr != nil {
		tr.logger.Error("failed to report failure", "error", reportErr)
	}
}

type TaskError struct {
	Reason    string
	Permanent bool
}

func (e *TaskError) Error() string {
	return e.Reason
}

func relativePath(name string) string {
	return strings.TrimPrefix(name, "/")
}
