package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TheOutdoorProgrammer/crate/internal/activity"
	"github.com/TheOutdoorProgrammer/crate/internal/db"
	"github.com/TheOutdoorProgrammer/crate/internal/models"
	"github.com/TheOutdoorProgrammer/crate/internal/services/slskd"
)

const defaultMaxConcurrentSlskd = 10

var supportedExts = map[string]bool{
	".flac": true,
	".mp3":  true,
	".ogg":  true,
	".opus": true,
	".aac":  true,
	".m4a":  true,
	".wav":  true,
}

var losslessExts = map[string]bool{
	".flac": true,
	".wav":  true,
}

type Organizer interface {
	Organize(track *models.Track) error
	// DownloadedFileExists reports whether a remote path's basename already
	// exists under the downloads dir — the rescue check for transfers that
	// vanish from slskd's list after the bytes landed.
	DownloadedFileExists(remoteName string) bool
}

type PostDownloadNotifier interface {
	TriggerScan(ctx context.Context)
}

type Service struct {
	queries     *db.Queries
	slskd       *slskd.Client
	organizer   Organizer
	activityLog *activity.Log
	notifiers   []PostDownloadNotifier
}

func NewService(queries *db.Queries, slskdClient *slskd.Client, org Organizer, actLog *activity.Log) *Service {
	return &Service{queries: queries, slskd: slskdClient, organizer: org, activityLog: actLog}
}

func (s *Service) AddNotifier(n PostDownloadNotifier) {
	s.notifiers = append(s.notifiers, n)
}

func (s *Service) Notifiers() []PostDownloadNotifier {
	return s.notifiers
}

func (s *Service) CancelTransfer(ctx context.Context, d *models.DownloadQueueItem) {
	if d.SlskdSearchID == nil {
		return
	}
	switch d.Status {
	case models.DownloadStatusDownloading:
		parts := strings.SplitN(*d.SlskdSearchID, "|", 2)
		if len(parts) == 2 {
			_ = s.slskd.CancelDownload(ctx, parts[0], parts[1])
		}
	case models.DownloadStatusSearching:
		_ = s.slskd.DeleteSearch(ctx, *d.SlskdSearchID)
	}
}

type ProgressItem struct {
	// Key is "username|transferID" — matches download_queue.slskd_search_id
	// so the UI can attach live progress to a queued track.
	Key              string  `json:"key"`
	Username         string  `json:"username"`
	Filename         string  `json:"filename"`
	PercentComplete  float64 `json:"percent_complete"`
	AverageSpeed     float64 `json:"average_speed_bps"`
	BytesTransferred int64   `json:"bytes_transferred"`
	Size             int64   `json:"size"`
	State            string  `json:"state"`
}

func (s *Service) GetProgress(ctx context.Context) ([]ProgressItem, error) {
	dirs, err := s.slskd.GetAllDownloads(ctx)
	if err != nil {
		return nil, err
	}

	activeTransfers := make(map[string]bool)
	downloading, _ := s.queries.ListDownloads("downloading")
	for _, d := range downloading {
		if d.SlskdSearchID != nil {
			// Lowercased: Soulseek usernames are case-insensitive and slskd's
			// transfers list can report different casing than the search did.
			activeTransfers[strings.ToLower(*d.SlskdSearchID)] = true
		}
	}

	var items []ProgressItem
	for _, ud := range dirs {
		for _, dir := range ud.Directories {
			for _, f := range dir.Files {
				if strings.Contains(f.State, "Completed") {
					continue
				}
				transferKey := strings.ToLower(ud.Username + "|" + f.ID)
				if !activeTransfers[transferKey] {
					continue
				}
				items = append(items, ProgressItem{
					Key:              ud.Username + "|" + f.ID,
					Username:         ud.Username,
					Filename:         filepath.Base(f.Filename),
					PercentComplete:  f.PercentComplete,
					AverageSpeed:     f.AverageSpeed,
					BytesTransferred: f.BytesTransferred,
					Size:             f.Size,
					State:            f.State,
				})
			}
		}
	}
	return items, nil
}

// Run processes the download queue on a ticker. Call in a goroutine.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Service) tick(ctx context.Context) {
	retryable, err := s.queries.ListRetryableDownloads()
	if err == nil {
		for _, d := range retryable {
			if d.Attempts >= 4 && d.Source == "scheduler" {
				slog.Info("downloader: removing exhausted scheduler download", "download_id", d.ID)
				s.queries.DeleteDownload(d.ID)
				continue
			}
			slog.Info("downloader: retrying", "download_id", d.ID, "attempts", d.Attempts)
			s.queries.UpdateDownloadStatus(d.ID, models.DownloadStatusPending, nil, d.Error)
		}
	}

	maxConcurrent := s.getMaxConcurrent()
	activeCount, err := s.queries.CountActiveDownloads()
	if err != nil {
		slog.Error("downloader: count active", "error", err)
		activeCount = 0
	}
	slots := maxConcurrent - activeCount

	pending, err := s.queries.ListDownloads("pending")
	if err != nil {
		slog.Error("downloader: list pending", "error", err)
		return
	}
	for _, d := range pending {
		if slots <= 0 {
			break
		}
		if d.Attempts >= 4 {
			if d.Source == "scheduler" {
				slog.Info("downloader: removing exhausted scheduler download", "download_id", d.ID)
				s.queries.DeleteDownload(d.ID)
				continue
			}
			reason := "exceeded maximum retry attempts"
			if d.Error != nil && *d.Error != "" {
				reason += ": " + *d.Error
			}
			s.queries.UpdateDownloadStatus(d.ID, models.DownloadStatusFailed, nil, &reason)
			continue
		}
		if err := s.process(ctx, d); err != nil {
			slog.Error("downloader: process", "download_id", d.ID, "error", err)
		}
		slots--
	}

	searching, err := s.queries.ListDownloads("searching")
	if err != nil {
		return
	}
	for _, d := range searching {
		if err := s.checkSearch(ctx, d); err != nil {
			slog.Error("downloader: check search", "download_id", d.ID, "error", err)
		}
	}

	downloading, err := s.queries.ListDownloads("downloading")
	if err != nil {
		return
	}
	for _, d := range downloading {
		if err := s.checkDownload(ctx, d); err != nil {
			slog.Error("downloader: check download", "download_id", d.ID, "error", err)
		}
	}

	organizing, err := s.queries.ListDownloads("organizing")
	if err != nil {
		return
	}
	for _, d := range organizing {
		if err := s.retryOrganize(d); err != nil {
			slog.Error("downloader: retry organize", "download_id", d.ID, "error", err)
		}
	}
}

func (s *Service) getCooldownDuration() time.Duration {
	if v, err := s.queries.GetSetting("shadow_ban_duration_minutes"); err == nil && v != "" {
		if mins, err := strconv.Atoi(v); err == nil && mins > 0 {
			return time.Duration(mins) * time.Minute
		}
	}
	return 60 * time.Minute
}

func (s *Service) getScoringConfig() scoringConfig {
	cfg := scoringConfig{fallbackEnabled: true}
	if tiersJSON, err := s.queries.GetSetting("quality_tiers"); err == nil && tiersJSON != "" {
		var tiers []models.QualityTier
		if json.Unmarshal([]byte(tiersJSON), &tiers) == nil {
			cfg.tiers = tiers
		}
	}
	if v, err := s.queries.GetSetting("quality_fallback_enabled"); err == nil && v == "false" {
		cfg.fallbackEnabled = false
	}
	if kwJSON, err := s.queries.GetSetting("negative_keywords"); err == nil && kwJSON != "" {
		var keywords []string
		if json.Unmarshal([]byte(kwJSON), &keywords) == nil {
			cfg.negativeKeywords = keywords
		}
	}
	return cfg
}

func (s *Service) getMaxConcurrent() int {
	v, err := s.queries.GetSetting("max_concurrent_slskd")
	if err != nil {
		return defaultMaxConcurrentSlskd
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return defaultMaxConcurrentSlskd
	}
	return n
}

// process kicks off a search for a pending download item.
func (s *Service) process(ctx context.Context, d models.DownloadQueueItem) error {
	track, err := s.queries.GetTrackWithMeta(d.TrackID)
	if err != nil {
		return err
	}

	query := track.ArtistName + " " + track.Title
	slog.Info("downloader: searching", "query", query, "track_id", track.ID)
	s.logActivity("search_started", "track", track.ID,
		fmt.Sprintf("Searching for: %s - %s", track.ArtistName, track.Title))

	search, err := s.slskd.StartSearch(ctx, query)
	if err != nil {
		return s.failWithRetry(d, err.Error())
	}

	return s.queries.UpdateDownloadStatus(d.ID, models.DownloadStatusSearching, &search.ID, nil)
}

// checkSearch polls a search and initiates download when results are ready.
func (s *Service) checkSearch(ctx context.Context, d models.DownloadQueueItem) error {
	if d.SlskdSearchID == nil {
		errStr := "missing search id"
		return s.queries.UpdateDownloadStatus(d.ID, models.DownloadStatusFailed, nil, &errStr)
	}

	search, err := s.slskd.GetSearch(ctx, *d.SlskdSearchID)
	if err != nil {
		return err
	}

	if !search.IsComplete {
		if d.LastAttempt != nil && time.Since(mustParseTime(*d.LastAttempt)) < 5*time.Minute {
			return nil
		}
	}

	track, err := s.queries.GetTrackWithMeta(d.TrackID)
	if err != nil {
		return err
	}

	cfg := s.getScoringConfig()
	cfg.excludeNegative = true
	if users, err := s.queries.ListAlbumDownloadSources(track.AlbumID); err == nil && len(users) > 0 {
		cfg.preferredUsers = make(map[string]bool, len(users))
		for _, u := range users {
			cfg.preferredUsers[u] = true
		}
	}
	best := pickBestFile(search.Responses, track, s.queries, cfg)
	if best == nil {
		_ = s.slskd.DeleteSearch(ctx, *d.SlskdSearchID)
		errStr := "no suitable file found"
		s.logActivity("download_failed", "track", track.ID,
			fmt.Sprintf("No results for: %s - %s", track.ArtistName, track.Title))
		_ = s.queries.UpdateTrackStatus(track.ID, models.TrackStatusWanted)
		return s.queries.UpdateDownloadStatus(d.ID, models.DownloadStatusFailed, nil, &errStr)
	}

	slog.Info("downloader: starting download",
		"username", best.username,
		"filename", best.file.Filename,
		"track_id", track.ID,
		"album_source", cfg.preferredUsers[best.username],
	)
	if err := s.beginTransfer(ctx, d.ID, track, best.username, best.file); err != nil {
		s.queries.CooldownUser(best.username, "download failed: "+err.Error(), s.getCooldownDuration())
		slog.Info("downloader: download start failed, shadow banning user", "username", best.username, "error", err)
		return s.failWithRetry(d, err.Error())
	}

	_ = s.slskd.DeleteSearch(ctx, *d.SlskdSearchID)
	return nil
}

// beginTransfer stamps the chosen source on the track, asks slskd for the
// file, and flips the queue row + track to downloading under the transfer key.
func (s *Service) beginTransfer(ctx context.Context, downloadID int64, track *models.Track, username string, f slskd.SearchFile) error {
	s.logActivity("download_started", "track", track.ID,
		fmt.Sprintf("Downloading from %s: %s", username, filepath.Base(f.Filename)))

	_ = s.queries.UpdateTrackDownloadedFrom(track.ID, username)
	_ = s.queries.UpdateTrackDownloadedFilename(track.ID, f.Filename)

	ext := strings.ToLower(filepath.Ext(f.Filename))
	if format := strings.TrimPrefix(ext, "."); format != "" {
		_ = s.queries.UpdateTrackQuality(track.ID, format, f.BitRate)
	}

	transfer, err := s.slskd.StartDownload(ctx, username, f.Filename, f.Size)
	if err != nil {
		return err
	}

	// Store transfer ID in slskd_search_id field (reuse the column)
	transferKey := username + "|" + transfer.ID
	if err := s.queries.UpdateDownloadStatus(downloadID, models.DownloadStatusDownloading, &transferKey, nil); err != nil {
		return err
	}
	return s.queries.UpdateTrackStatus(track.ID, models.TrackStatusDownloading)
}

// DownloadAlbum tries to source an entire album from a single Soulseek peer
// before any per-track searches: one "Artist Album" search, pick the user
// whose share covers the most wanted tracks (same scoring/guards as the
// per-track pipeline), and start those transfers directly. Tracks nobody's
// share covers — or whose queue row is already in flight — are enqueued for
// the normal per-track pipeline.
func (s *Service) DownloadAlbum(ctx context.Context, albumID int64) {
	album, err := s.queries.GetAlbum(albumID)
	if err != nil || album == nil {
		return
	}
	artist, err := s.queries.GetArtist(album.ArtistID)
	if err != nil || artist == nil {
		return
	}
	tracks, err := s.queries.ListTracksByAlbum(albumID)
	if err != nil {
		return
	}
	var wanted []*models.Track
	for i := range tracks {
		if tracks[i].Status != models.TrackStatusWanted {
			continue
		}
		tracks[i].ArtistName = artist.Name
		tracks[i].AlbumTitle = album.Title
		wanted = append(wanted, &tracks[i])
	}
	if len(wanted) == 0 {
		return
	}

	query := artist.Name + " " + album.Title
	slog.Info("downloader: album search", "query", query, "album", album.Title, "wanted", len(wanted))
	s.logActivity("search_started", "album", albumID,
		fmt.Sprintf("Searching for album: %s - %s", artist.Name, album.Title))

	search, err := s.slskd.StartSearch(ctx, query)
	if err != nil {
		slog.Warn("downloader: album search failed, using per-track pipeline", "album", album.Title, "error", err)
		s.enqueueTrackIDs(wanted)
		return
	}
	defer func() { _ = s.slskd.DeleteSearch(context.Background(), search.ID) }()

	deadline := time.Now().Add(30 * time.Second)
	for !search.IsComplete && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		search, err = s.slskd.GetSearch(ctx, search.ID)
		if err != nil {
			slog.Warn("downloader: album search poll failed, using per-track pipeline", "album", album.Title, "error", err)
			s.enqueueTrackIDs(wanted)
			return
		}
	}

	cfg := s.getScoringConfig()
	cfg.excludeNegative = true
	bestUser, files := pickAlbumSource(search.Responses, wanted, s.queries, cfg)
	if bestUser == "" {
		slog.Info("downloader: no single-source match, using per-track pipeline", "album", album.Title)
		s.enqueueTrackIDs(wanted)
		return
	}

	slog.Info("downloader: album sourced from one peer", "album", album.Title,
		"username", bestUser, "tracks", len(files), "wanted", len(wanted))
	for _, tr := range wanted {
		cand, ok := files[tr.ID]
		if !ok {
			continue
		}
		dlID, err := s.queries.EnqueueDownloadReturningID(tr.ID)
		if err != nil || dlID == 0 {
			continue // already in flight — the normal pipeline owns it
		}
		if err := s.beginTransfer(ctx, dlID, tr, bestUser, cand.file); err != nil {
			s.queries.CooldownUser(bestUser, "download failed: "+err.Error(), s.getCooldownDuration())
			slog.Info("downloader: album transfer start failed, shadow banning user", "username", bestUser, "error", err)
			continue // the pending queue row retries through the per-track path
		}
	}
	s.enqueueTrackIDs(wanted) // covers tracks the album search couldn't
}

func (s *Service) enqueueTrackIDs(tracks []*models.Track) {
	for _, t := range tracks {
		_ = s.queries.EnqueueDownload(t.ID)
	}
}

// pickAlbumSource scores each user's share against the album's wanted tracks
// — same guards as the per-track path (tier, blacklist, cooldown, artist+title
// in path) — and returns the user covering the most tracks with their best
// file per track. "" when no user's share matches anything.
func pickAlbumSource(results []slskd.SearchResult, wanted []*models.Track, ac availabilityChecker, cfg scoringConfig) (string, map[int64]candidate) {
	cfg.requireArtist = true
	bestUser := ""
	var bestFiles map[int64]candidate
	bestCoverage, bestScore := 0, 0
	for _, res := range results {
		if ac != nil && ac.IsUserCooledDown(res.Username) {
			continue
		}
		single := []slskd.SearchResult{res}
		files := make(map[int64]candidate)
		score := 0
		for _, tr := range wanted {
			if cands := scoreCandidates(single, tr, ac, cfg); len(cands) > 0 {
				files[tr.ID] = cands[0]
				score += cands[0].score
			}
		}
		if len(files) > bestCoverage || (len(files) == bestCoverage && len(files) > 0 && score > bestScore) {
			bestUser, bestFiles, bestCoverage, bestScore = res.Username, files, len(files), score
		}
	}
	return bestUser, bestFiles
}

// checkDownload polls an active transfer and marks complete when done.
func (s *Service) checkDownload(ctx context.Context, d models.DownloadQueueItem) error {
	if d.SlskdSearchID == nil {
		return nil
	}

	parts := strings.SplitN(*d.SlskdSearchID, "|", 2)
	if len(parts) != 2 {
		return nil
	}
	username, transferID := parts[0], parts[1]

	transfer, err := s.slskd.GetDownload(ctx, username, transferID)
	if err != nil {
		pollErr := err
		// The transfer vanished from slskd's list — either its record was
		// pruned/recreated (id or username casing drift), or it completed and
		// was cleaned up between ticks. Re-locate by filename first; if the
		// file already landed on disk, organize it instead of failing.
		if t := s.findTransferByFilename(ctx, d, username); t != nil {
			transfer = t
			key := username + "|" + t.ID
			_ = s.queries.UpdateDownloadStatus(d.ID, d.Status, &key, nil)
			d.SlskdSearchID = &key
		} else if s.organizeIfOnDisk(ctx, d, username) {
			return nil
		} else {
			_ = s.queries.UpdateTrackStatus(d.TrackID, models.TrackStatusWanted)
			return s.failWithRetry(d, pollErr.Error())
		}
	}

	state := transfer.State
	switch {
	case strings.Contains(state, "Succeeded"):
		return s.completeDownload(ctx, d, username, transfer.Filename)

	case strings.Contains(state, "Errored"), strings.Contains(state, "Rejected"), strings.Contains(state, "Cancelled"):
		_ = s.queries.UpdateTrackStatus(d.TrackID, models.TrackStatusWanted)
		_ = s.queries.BlacklistFile(username, transfer.Filename, "transfer "+state)
		s.removeTransferRecord(ctx, d)
		slog.Info("downloader: blacklisted file", "username", username, "filename", filepath.Base(transfer.Filename))
		s.logActivity("download_failed", "track", d.TrackID,
			fmt.Sprintf("Transfer failed: %s from %s (blacklisted)", state, username))
		return s.failWithRetry(d, "transfer failed: "+state)
	}

	if transfer.BytesTransferred > d.LastProgressBytes {
		_ = s.queries.UpdateDownloadProgress(d.ID, transfer.BytesTransferred)
		return nil
	}

	timeout := staleTimeoutForState(state)
	if d.LastAttempt != nil {
		lastProgress := mustParseTime(*d.LastAttempt)
		if !lastProgress.IsZero() && time.Since(lastProgress) > timeout {
			_ = s.slskd.CancelDownload(ctx, username, transferID)
			_ = s.queries.UpdateTrackStatus(d.TrackID, models.TrackStatusWanted)

			reason := fmt.Sprintf("stale transfer (%s): no progress for %s", state, timeout)
			if strings.Contains(state, "Queued") || strings.Contains(state, "Requested") {
				s.queries.CooldownUser(username, reason, s.getCooldownDuration())
				slog.Info("downloader: stale queued transfer, shadow banning user", "username", username, "state", state, "download_id", d.ID)
			} else {
				_ = s.queries.BlacklistFile(username, transfer.Filename, reason)
				slog.Info("downloader: stale transfer, blacklisting file", "username", username, "filename", filepath.Base(transfer.Filename), "download_id", d.ID)
			}
			s.logActivity("download_failed", "track", d.TrackID,
				fmt.Sprintf("Stale transfer (%s, %s) from %s", state, timeout, username))
			return s.failWithRetry(d, reason)
		}
	}
	return nil
}

// completeDownload moves a finished transfer's file into the library and
// flips the queue row + track to their owned states.
func (s *Service) completeDownload(ctx context.Context, d models.DownloadQueueItem, username, remoteName string) error {
	rawName := remoteName
	if idx := strings.LastIndexAny(rawName, `/\`); idx >= 0 {
		rawName = rawName[idx+1:]
	}
	if err := s.queries.UpdateDownloadStatus(d.ID, models.DownloadStatusOrganizing, d.SlskdSearchID, nil); err != nil {
		return err
	}
	slog.Info("downloader: organizing", "filename", rawName, "track_id", d.TrackID)
	track, err := s.queries.GetTrackWithMeta(d.TrackID)
	if err != nil {
		return err
	}
	track.FilePath = &rawName

	if err := s.organizer.Organize(track); err != nil {
		slog.Error("downloader: organize failed, will retry", "filename", rawName, "track_id", d.TrackID, "error", err)
		return err
	}
	s.logActivity("download_complete", "track", d.TrackID,
		fmt.Sprintf("Downloaded: %s - %s from %s", track.ArtistName, track.Title, username))
	for _, n := range s.notifiers {
		n.TriggerScan(ctx)
	}
	if err := s.queries.UpdateDownloadStatus(d.ID, models.DownloadStatusComplete, d.SlskdSearchID, nil); err != nil {
		return err
	}
	s.removeTransferRecord(ctx, d)
	return nil
}

// removeTransferRecord drops a finished/dead transfer from slskd's downloads
// list so completed transfers don't pile up there. Best-effort — the record
// may already be gone (e.g. the on-disk rescue path).
func (s *Service) removeTransferRecord(ctx context.Context, d models.DownloadQueueItem) {
	if d.SlskdSearchID == nil {
		return
	}
	parts := strings.SplitN(*d.SlskdSearchID, "|", 2)
	if len(parts) != 2 {
		return
	}
	if err := s.slskd.CancelDownload(ctx, parts[0], parts[1]); err != nil {
		slog.Debug("downloader: could not remove slskd transfer record", "id", parts[1], "error", err)
	}
}

// findTransferByFilename re-locates a transfer that vanished by id — slskd
// can drop and recreate transfer records (restart, prune), and the stored
// username casing may differ from what the transfers list reports.
func (s *Service) findTransferByFilename(ctx context.Context, d models.DownloadQueueItem, username string) *slskd.Transfer {
	track, err := s.queries.GetTrackWithMeta(d.TrackID)
	if err != nil || track.DownloadedFilename == nil {
		return nil
	}
	tr, err := s.slskd.GetDownloadByFilename(ctx, username, *track.DownloadedFilename)
	if err != nil {
		return nil
	}
	return tr
}

// organizeIfOnDisk finishes a download whose transfer vanished from slskd but
// whose file already landed in the downloads dir — fast transfers can
// complete and be pruned between ticks. Returns false when the file isn't
// there, so the caller can fall back to a real retry.
func (s *Service) organizeIfOnDisk(ctx context.Context, d models.DownloadQueueItem, username string) bool {
	track, err := s.queries.GetTrackWithMeta(d.TrackID)
	if err != nil || track.DownloadedFilename == nil {
		return false
	}
	if !s.organizer.DownloadedFileExists(*track.DownloadedFilename) {
		return false
	}
	slog.Info("downloader: transfer gone but file on disk, organizing",
		"track_id", d.TrackID, "filename", *track.DownloadedFilename)
	// On error the row sits in "organizing" — retryOrganize owns it now.
	if err := s.completeDownload(ctx, d, username, *track.DownloadedFilename); err != nil {
		slog.Error("downloader: on-disk organize failed", "track_id", d.TrackID, "error", err)
	}
	return true
}

// retryOrganize is called for downloads stuck in "organizing" (e.g. after a crash).
// It re-derives the filename from the slskd transfer key and retries the move.
func (s *Service) retryOrganize(d models.DownloadQueueItem) error {
	if d.SlskdSearchID == nil {
		return nil
	}
	parts := strings.SplitN(*d.SlskdSearchID, "|", 2)
	if len(parts) != 2 {
		return nil
	}
	// The transfer key is "username|transferID". We stored it before transitioning
	// to organizing, so parse the original slskd filename from the transfer record.
	// We don't have the filename directly, so look it up from the track if available,
	// otherwise fall back to a no-op (the file is either already moved or needs a manual fix).
	track, err := s.queries.GetTrackWithMeta(d.TrackID)
	if err != nil {
		return err
	}
	if track.FilePath == nil {
		// No filename recorded yet; nothing to retry
		return nil
	}
	slog.Info("downloader: retrying organize", "filename", *track.FilePath, "track_id", d.TrackID)
	if err := s.organizer.Organize(track); err != nil {
		slog.Error("downloader: retry organize failed", "filename", *track.FilePath, "track_id", d.TrackID, "error", err)
		return err
	}
	return s.queries.UpdateDownloadStatus(d.ID, models.DownloadStatusComplete, d.SlskdSearchID, nil)
}

func (s *Service) failWithRetry(d models.DownloadQueueItem, errMsg string) error {
	backoff := retryDelay(d.Attempts)
	// 5m + 15m + 30m + 1h = 1h50m cumulative at attempt 4; stop after ~2h
	if d.Attempts >= 4 {
		_ = s.queries.UpdateTrackStatus(d.TrackID, models.TrackStatusWanted)
		if d.Source == "scheduler" {
			slog.Info("downloader: removing exhausted scheduler download", "download_id", d.ID)
			return s.queries.DeleteDownload(d.ID)
		}
		slog.Warn("downloader: giving up after 2h of retries", "download_id", d.ID, "attempts", d.Attempts)
		errFinal := errMsg + " (giving up after 2h)"
		return s.queries.UpdateDownloadStatus(d.ID, models.DownloadStatusFailed, nil, &errFinal)
	}
	retryAt := time.Now().UTC().Add(backoff).Format(time.RFC3339)
	slog.Info("downloader: scheduling retry", "download_id", d.ID, "attempts", d.Attempts, "retry_in", backoff)
	return s.queries.ScheduleRetry(d.ID, retryAt, errMsg)
}

func retryDelay(attempts int) time.Duration {
	delays := []time.Duration{
		5 * time.Minute,
		15 * time.Minute,
		30 * time.Minute,
		1 * time.Hour,
	}
	if attempts >= len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempts]
}

func (s *Service) logActivity(action, entityType string, entityID int64, details string) {
	if s.activityLog != nil {
		s.activityLog.Record(action, entityType, entityID, details)
	}
}

// ManualSearchResult is a scored slskd result returned to the frontend.
type ManualSearchResult struct {
	Username      string `json:"username"`
	Filename      string `json:"filename"`
	Size          int64  `json:"size"`
	BitRate       int    `json:"bit_rate"`
	SampleRate    int    `json:"sample_rate"`
	BitDepth      int    `json:"bit_depth"`
	Duration      int    `json:"duration"`
	Format        string `json:"format"`
	Score         int    `json:"score"`
	FreeSlot      bool   `json:"free_slot"`
	QueueLength   int    `json:"queue_length"`
	Blacklisted   bool   `json:"blacklisted"`
	NegativeMatch bool   `json:"negative_match"`
	Locked        bool   `json:"locked"`
}

// ManualSearch runs a slskd search for a track and returns all scored results.
// StartManualSearch kicks off an slskd search and returns the search ID and query used.
// If customQuery is non-empty, it is used instead of the default "artist title" query.
func (s *Service) StartManualSearch(ctx context.Context, trackID int64, customQuery string) (string, string, error) {
	track, err := s.queries.GetTrackWithMeta(trackID)
	if err != nil {
		return "", "", err
	}
	query := track.ArtistName + " " + track.Title
	if customQuery != "" {
		query = customQuery
	}
	search, err := s.slskd.StartSearch(ctx, query)
	if err != nil {
		return "", "", fmt.Errorf("start search: %w", err)
	}
	return search.ID, query, nil
}

// ManualSearchResponse holds scored results plus completion status.
type ManualSearchResponse struct {
	Results    []ManualSearchResult `json:"results"`
	IsComplete bool                 `json:"is_complete"`
	FileCount  int                  `json:"file_count"`
}

// PollManualSearch returns the current scored results for an in-progress search.
func (s *Service) PollManualSearch(ctx context.Context, trackID int64, searchID string) (*ManualSearchResponse, error) {
	track, err := s.queries.GetTrackWithMeta(trackID)
	if err != nil {
		return nil, err
	}

	search, err := s.slskd.GetSearch(ctx, searchID)
	if err != nil {
		return nil, fmt.Errorf("get search: %w", err)
	}

	cfg := s.getScoringConfig()
	cfg.fallbackEnabled = true
	cfg.includeAll = true // manual search surfaces every slskd result; the user picks
	candidates := scoreCandidates(search.Responses, track, nil, cfg)

	return &ManualSearchResponse{
		Results:    formatCandidates(candidates, s.queries, cfg.negativeKeywords),
		IsComplete: search.IsComplete,
		FileCount:  search.FileCount,
	}, nil
}

// CleanupSearch deletes an slskd search.
func (s *Service) CleanupSearch(ctx context.Context, searchID string) {
	_ = s.slskd.DeleteSearch(ctx, searchID)
}

func formatCandidates(candidates []candidate, q *db.Queries, negativeKeywords []string) []ManualSearchResult {
	results := make([]ManualSearchResult, 0, len(candidates))
	for _, c := range candidates {
		ext := strings.ToLower(filepath.Ext(c.file.Filename))
		if ext == "" {
			ext = inferExt(strings.ToLower(c.file.Filename))
		}
		format := strings.TrimPrefix(ext, ".")
		if format == "mp3" && c.file.BitRate > 0 {
			format = fmt.Sprintf("MP3 %dkbps", c.file.BitRate)
		} else {
			format = strings.ToUpper(format)
		}

		results = append(results, ManualSearchResult{
			Username:      c.username,
			Filename:      c.file.Filename,
			Size:          c.file.Size,
			BitRate:       c.file.BitRate,
			SampleRate:    c.file.SampleRate,
			BitDepth:      c.file.BitDepth,
			Duration:      c.file.Length,
			Format:        format,
			Score:         c.score,
			FreeSlot:      c.freeSlot,
			QueueLength:   c.queueLength,
			Blacklisted:   q.IsBlacklisted(c.username, c.file.Filename),
			NegativeMatch: matchesNegativeKeyword(strings.ToLower(c.file.Filename), negativeKeywords),
			Locked:        c.file.IsLocked,
		})
	}
	return results
}

// ManualDownload starts a download of a specific file from a specific user.
func (s *Service) ManualDownload(ctx context.Context, trackID int64, username, filename string, size int64, bitrate int) error {
	track, err := s.queries.GetTrackWithMeta(trackID)
	if err != nil {
		return err
	}

	_ = s.queries.UpdateTrackDownloadedFrom(trackID, username)
	_ = s.queries.UpdateTrackDownloadedFilename(trackID, filename)

	ext := strings.ToLower(filepath.Ext(filename))
	if format := strings.TrimPrefix(ext, "."); format != "" {
		_ = s.queries.UpdateTrackQuality(trackID, format, bitrate)
	}

	transfer, err := s.slskd.StartDownload(ctx, username, filename, size)
	if err != nil {
		return fmt.Errorf("start download: %w", err)
	}

	dlID, err := s.queries.EnqueueDownloadReturningID(trackID)
	if err != nil {
		return err
	}

	transferKey := username + "|" + transfer.ID
	_ = s.queries.UpdateDownloadStatus(dlID, models.DownloadStatusDownloading, &transferKey, nil)
	_ = s.queries.UpdateTrackStatus(trackID, models.TrackStatusDownloading)

	s.logActivity("download_started", "track", trackID,
		fmt.Sprintf("Manual download from %s: %s - %s", username, track.ArtistName, track.Title))

	return nil
}

func scoreCandidates(results []slskd.SearchResult, track *models.Track, ac availabilityChecker, cfg scoringConfig) []candidate {
	titleLower := strings.ToLower(track.Title)
	artistLower := strings.ToLower(track.ArtistName)

	var candidates []candidate

	for _, result := range results {
		if ac != nil && ac.IsUserCooledDown(result.Username) {
			continue
		}
		for _, f := range result.Files {
			if f.IsLocked && !cfg.includeAll {
				continue
			}
			if ac != nil && ac.IsBlacklisted(result.Username, f.Filename) {
				continue
			}
			nameLower := strings.ToLower(f.Filename)
			// Manual search (includeAll) returns everything slskd found and lets the
			// user pick; the title/artist/negative filters below are auto-download
			// guards that must not silently drop results in the manual flow.
			if !cfg.includeAll {
				if !strings.Contains(nameLower, titleLower) {
					continue
				}
				if cfg.requireArtist && !strings.Contains(nameLower, artistLower) {
					continue
				}
				if cfg.excludeNegative && matchesNegativeKeyword(nameLower, cfg.negativeKeywords) {
					continue
				}
			}

			ext := strings.ToLower(filepath.Ext(f.Filename))
			if ext == "" {
				ext = inferExt(nameLower)
			}
			if ext == "" || !supportedExts[ext] {
				// Unknown/unsupported formats can't be quality-scored for an
				// unattended download, but manual search still surfaces them.
				if !cfg.includeAll {
					continue
				}
			}

			qualityScore, allowed := tierBasedScore(ext, f.BitRate, cfg)
			if !allowed && !cfg.includeAll {
				continue
			}
			score := qualityScore
			// Keep artist bonus below the tier gap (25) so quality always dominates.
			if artistLower != "" && strings.Contains(nameLower, artistLower) {
				score += 20
			}
			freeSlotBonus := 0
			if result.HasFreeUploadSlot {
				freeSlotBonus = 10
			}
			score += freeSlotBonus + queueScore(result.QueueLength)
			if cfg.preferredUsers[result.Username] {
				score += sameUserBonus
			}

			candidates = append(candidates, candidate{
				username:    result.Username,
				file:        f,
				score:       score,
				freeSlot:    result.HasFreeUploadSlot,
				queueLength: result.QueueLength,
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	return candidates
}

// QualityTierRank returns the rank of a format/bitrate in the given tiers.
// Lower rank = higher priority. Returns -1 if not matched.
func QualityTierRank(tiers []models.QualityTier, format string, bitrate int) int {
	format = strings.ToLower(format)
	for i, tier := range tiers {
		if strings.ToLower(tier.Format) != format {
			continue
		}
		if tier.MinBitrate > 0 && bitrate > 0 && bitrate < tier.MinBitrate {
			continue
		}
		return i
	}
	return -1
}

// IsUpgradeable returns true if the track's current quality can be improved
// according to the tier list (a lower-index tier exists that the track doesn't match).
func IsUpgradeable(tiers []models.QualityTier, track *models.Track) bool {
	if track.DownloadFormat == nil {
		return len(tiers) > 0
	}
	currentRank := QualityTierRank(tiers, *track.DownloadFormat, derefInt(track.DownloadBitrate))
	if currentRank < 0 {
		return len(tiers) > 0
	}
	return currentRank > 0
}

func staleTimeoutForState(state string) time.Duration {
	switch {
	case strings.Contains(state, "InProgress"), strings.Contains(state, "Initializing"):
		return 5 * time.Minute
	case strings.Contains(state, "Queued"):
		return 30 * time.Minute
	case strings.Contains(state, "Requested"):
		return 10 * time.Minute
	default:
		return 5 * time.Minute
	}
}

func mustParseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

type candidate struct {
	username    string
	file        slskd.SearchFile
	score       int
	freeSlot    bool
	queueLength int
}

type availabilityChecker interface {
	IsBlacklisted(username, filename string) bool
	IsUserCooledDown(username string) bool
}

type scoringConfig struct {
	tiers            []models.QualityTier
	fallbackEnabled  bool
	negativeKeywords []string
	excludeNegative  bool
	requireArtist    bool
	// includeAll disables the title/artist/format filters so manual search can
	// surface every file slskd returned (still scored and annotated, never dropped).
	includeAll bool
	// preferredUsers are peers already serving other tracks on the same album —
	// their files get a bonus so an album tends to come from one source.
	preferredUsers map[string]bool
}

// sameUserBonus is strong enough to keep an album on one peer across one
// quality-tier gap (25) plus availability bonuses, but a two-tier drop still
// falls back to the better file elsewhere.
const sameUserBonus = 30

func pickBestFile(results []slskd.SearchResult, track *models.Track, ac availabilityChecker, cfg scoringConfig) *candidate {
	cfg.requireArtist = true
	candidates := scoreCandidates(results, track, ac, cfg)
	if len(candidates) == 0 {
		return nil
	}
	return &candidates[0]
}

func inferExt(nameLower string) string {
	basename := nameLower
	if idx := strings.LastIndexAny(nameLower, `/\`); idx >= 0 {
		basename = nameLower[idx+1:]
	}
	for ext := range supportedExts {
		if strings.HasSuffix(basename, ext) {
			return ext
		}
	}
	return ""
}

func tierBasedScore(ext string, bitRate int, cfg scoringConfig) (int, bool) {
	format := strings.TrimPrefix(ext, ".")
	if len(cfg.tiers) == 0 {
		return fallbackScore(ext, bitRate), true
	}
	rank := QualityTierRank(cfg.tiers, format, bitRate)
	if rank >= 0 {
		score := 100 - (rank * 25)
		if score < 25 {
			score = 25
		}
		return score, true
	}
	if !cfg.fallbackEnabled {
		return 0, false
	}
	lowestTier := 100 - ((len(cfg.tiers) - 1) * 25)
	if lowestTier < 25 {
		lowestTier = 25
	}
	score := fallbackScore(ext, bitRate)
	if score >= lowestTier {
		score = lowestTier - 5
	}
	return score, true
}

func fallbackScore(ext string, bitRate int) int {
	if losslessExts[ext] {
		return 20
	}
	switch ext {
	case ".ogg", ".opus", ".aac", ".m4a":
		if bitRate >= 256 {
			return 15
		}
		return 10
	default:
		if bitRate >= 320 {
			return 15
		}
		if bitRate >= 256 {
			return 12
		}
		return 5
	}
}

func queueScore(queueLength int) int {
	return int(15.0 / float64(1+queueLength))
}

func matchesNegativeKeyword(filenameLower string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(filenameLower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
