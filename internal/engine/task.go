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

// Stage represents the current processing stage of a task.
type Stage string

const (
	StagePreparing   Stage = "preparing"
	StageDownloading Stage = "downloading"
	StageUploading   Stage = "uploading"
	StageDone        Stage = "done"
)

// TaskState holds all runtime state for a single task.
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

func newTaskRunner(cfg *config.Config, bc *backend.Client, qc *qbit.Client, up *s3uploader.Uploader, task *backend.Task, logger *slog.Logger) *taskRunner {
	return &taskRunner{
		cfg:      cfg,
		bc:       bc,
		qc:       qc,
		uploader: up,
		task:     task,
		state: &TaskState{
			TaskID:    task.TaskID,
			Stage:     StagePreparing,
			StartedAt: time.Now(),
		},
		logger: logger.With("task_id", task.TaskID),
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

	if tr.task.TorrentKind == "file_b64" {
		if err := tr.qc.AddTorrentFile(ctx, tr.task.TorrentData, savePath, "seans"); err != nil {
			return fmt.Errorf("add torrent file: %w", err)
		}
	} else {
		if err := tr.qc.AddTorrentMagnet(ctx, tr.task.TorrentData, savePath, "seans"); err != nil {
			return fmt.Errorf("add magnet: %w", err)
		}
	}

	torrent, err := tr.waitForTorrent(ctx, savePath)
	if err != nil {
		return err
	}
	tr.state.TorrentHash = torrent.Hash

	if tr.task.TorrentKind == "magnet" && torrent.State == "metaDL" {
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

	if err := tr.checkSizeAndPrioritize(ctx, torrent.Hash, files); err != nil {
		return err
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

func (tr *taskRunner) checkSizeAndPrioritize(ctx context.Context, hash string, files []qbit.TorrentFile) error {
	selectFiles := tr.task.SelectFiles

	if selectFiles == nil {
		var totalSize int64
		for _, f := range files {
			totalSize += f.Size
		}
		if totalSize > tr.task.MaxBytes {
			return &TaskError{
				Reason:    fmt.Sprintf("quota exceeded: %d bytes > %d limit", totalSize, tr.task.MaxBytes),
				Permanent: true,
			}
		}
		return nil
	}

	matchedIndices := tr.matchFiles(files, selectFiles)
	if len(matchedIndices) == 0 {
		return &TaskError{
			Reason:    "no matching files found for select_files",
			Permanent: true,
		}
	}

	var selectedSize int64
	matchedSet := make(map[int]bool)
	for _, idx := range matchedIndices {
		matchedSet[idx] = true
		for _, f := range files {
			if f.Index == idx {
				selectedSize += f.Size
				break
			}
		}
	}

	if selectedSize > tr.task.MaxBytes {
		return &TaskError{
			Reason:    fmt.Sprintf("quota exceeded: selected files %d bytes > %d limit", selectedSize, tr.task.MaxBytes),
			Permanent: true,
		}
	}

	unmatchedIndices := make([]int, 0)
	for _, f := range files {
		if !matchedSet[f.Index] {
			unmatchedIndices = append(unmatchedIndices, f.Index)
		}
	}

	if len(unmatchedIndices) > 0 {
		if err := tr.qc.SetFilePriority(ctx, hash, unmatchedIndices, 0); err != nil {
			tr.logger.Warn("failed to set skip priority", "error", err)
		}
	}
	if len(matchedIndices) > 0 {
		if err := tr.qc.SetFilePriority(ctx, hash, matchedIndices, 1); err != nil {
			tr.logger.Warn("failed to set normal priority", "error", err)
		}
	}

	tr.logger.Info("file selection applied",
		"selected", len(matchedIndices),
		"skipped", len(unmatchedIndices),
		"selected_bytes", selectedSize,
	)
	return nil
}

func (tr *taskRunner) matchFiles(files []qbit.TorrentFile, selectFiles []string) []int {
	matched := make([]int, 0)
	matchedNames := make(map[string]bool)

	// Exact match
	for _, sel := range selectFiles {
		for _, f := range files {
			if f.Name == sel && !matchedNames[f.Name] {
				matched = append(matched, f.Index)
				matchedNames[f.Name] = true
			}
		}
	}

	// Suffix match fallback
	for _, sel := range selectFiles {
		alreadyMatched := false
		for _, f := range files {
			if f.Name == sel {
				alreadyMatched = true
				break
			}
		}
		if alreadyMatched {
			continue
		}
		selBase := filepath.Base(sel)
		for _, f := range files {
			if matchedNames[f.Name] {
				continue
			}
			if filepath.Base(f.Name) == selBase {
				matched = append(matched, f.Index)
				matchedNames[f.Name] = true
				tr.logger.Info("suffix match fallback", "select_file", sel, "matched_to", f.Name)
			}
		}
	}

	return matched
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

			if torrent.Progress >= 1.0 && !isActiveState(torrent.State) {
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

	if err := tr.uploader.Move(ctx, stagingPath, tr.task.DestPrefix, 3, logFile); err != nil {
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
		if tr.isSelected(f) {
			files = append(files, backend.FileInfo{
				S3Key: tr.task.DestPrefix + relativePath(f.Name),
				Size:  f.Size,
			})
		}
	}

	stats := backend.TaskStats{
		DownloadSeconds: tr.state.DownloadSeconds,
		UploadSeconds:   tr.state.UploadSeconds,
	}

	if err := tr.bc.Complete(ctx, tr.task.TaskID, files, stats); err != nil {
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
	permanent := false

	if errors.As(err, &taskErr) {
		reason = taskErr.Reason
		permanent = taskErr.Permanent
	}

	tr.logger.Error("task failed", "reason", reason, "permanent", permanent)
	if reportErr := tr.bc.Fail(ctx, tr.task.TaskID, reason, permanent); reportErr != nil {
		tr.logger.Error("failed to report failure", "error", reportErr)
	}
}

func (tr *taskRunner) isSelected(f qbit.TorrentFile) bool {
	if tr.task.SelectFiles == nil {
		return f.Priority > 0
	}
	for _, sel := range tr.task.SelectFiles {
		if f.Name == sel || filepath.Base(f.Name) == filepath.Base(sel) {
			return true
		}
	}
	return false
}

func relativePath(name string) string {
	return strings.TrimPrefix(name, "/")
}

// TaskError represents a task failure with permanence flag.
type TaskError struct {
	Reason    string
	Permanent bool
}

func (e *TaskError) Error() string {
	return e.Reason
}