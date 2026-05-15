package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"easycampus/internal/atomicfile"
	ocffmpeg "easycampus/internal/ffmpeg"
	ocquality "easycampus/internal/quality"
)

// qualityBuckets are the legacy "low / standard / medium / high / ultra"
// presets the existing UI sends with each upload. The broadcast engineer's
// tier table will eventually replace this map; the QualityPreset interface
// is the integration seam (see internal/ffmpeg/config.go).
var qualityBuckets = map[string]ocffmpeg.SimplePreset{
	"low": {
		IDValue: "upload-720p30-2M", LabelValue: "720p30 @ 2 Mbps",
		W: 1280, H: 720, FPSInt: 30, GOPFrames: 60,
		VBitrate: "2000k", ABitrate: "128k",
		ProfileValue: "high", LevelValue: "3.1", EncoderPreset: "veryfast",
	},
	"standard": {
		IDValue: "upload-1080p30-4M", LabelValue: "1080p30 @ 4 Mbps",
		W: 1920, H: 1080, FPSInt: 30, GOPFrames: 60,
		VBitrate: "4000k", ABitrate: "128k",
		ProfileValue: "high", LevelValue: "4.1", EncoderPreset: "veryfast",
	},
	"medium": {
		IDValue: "upload-1080p30-4M", LabelValue: "1080p30 @ 4 Mbps",
		W: 1920, H: 1080, FPSInt: 30, GOPFrames: 60,
		VBitrate: "4000k", ABitrate: "128k",
		ProfileValue: "high", LevelValue: "4.1", EncoderPreset: "veryfast",
	},
	"high": {
		IDValue: "upload-1080p30-6M", LabelValue: "1080p30 @ 6 Mbps",
		W: 1920, H: 1080, FPSInt: 30, GOPFrames: 60,
		VBitrate: "6000k", ABitrate: "192k",
		ProfileValue: "high", LevelValue: "4.1", EncoderPreset: "medium",
	},
	"ultra": {
		IDValue: "upload-1080p30-8M", LabelValue: "1080p30 @ 8 Mbps",
		W: 1920, H: 1080, FPSInt: 30, GOPFrames: 60,
		VBitrate: "8000k", ABitrate: "192k",
		ProfileValue: "high", LevelValue: "4.1", EncoderPreset: "medium",
	},
}

// presetForQuality resolves a quality token into a ocffmpeg.QualityPreset.
//
// Lookup order:
//
//  1. A broadcast tier preset ID (e.g. "consumer-1080p30",
//     "broadcast-1080p60") from internal/quality. Wraps as an
//     ocquality.FFmpegPreset so color VUI / rate-control / broadcast-safe
//     limiter signal through to the supervisor.
//  2. A legacy bucket name (low / standard / medium / high / ultra).
//  3. The "high" bucket as a final fallback.
//
// Both forms are accepted so the UI can migrate to the tier IDs at
// its own pace; the upload endpoint stays backwards-compatible for at
// least one release.
func presetForQuality(quality string) ocffmpeg.QualityPreset {
	if quality == "" {
		return qualityBuckets["high"]
	}
	if p, ok := ocquality.ResolveFFmpegPreset(quality); ok {
		return p
	}
	if p, ok := qualityBuckets[strings.ToLower(quality)]; ok {
		return p
	}
	return qualityBuckets["high"]
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid multipart"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "file is required"})
		return
	}
	defer file.Close()

	// quality_preset is the new tier-aware identifier (e.g.
	// "broadcast-1080p60"). The legacy "quality" field continues to
	// work for one release for backwards compatibility — handlers
	// resolve either through presetForQuality.
	quality := r.FormValue("quality_preset")
	if quality == "" {
		quality = r.FormValue("quality")
	}
	if quality == "" {
		quality = "high"
	}

	eventID, eventDir, err := createEventDir(s.settings.EventsDir)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	filename := uploadFilename(header.Filename)

	inputPath := filepath.Join(eventDir, uploadSourceName(filename))
	if err := saveMultipartFile(file, inputPath); err != nil {
		_ = os.RemoveAll(eventDir)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}

	info := EventInfoFile{
		Name:       filename,
		CreatedAt:  time.Now().Format(time.RFC3339),
		Status:     "processing",
		SourceFile: filename,
		Type:       "upload",
		Quality:    quality,
	}
	infoPath := filepath.Join(eventDir, "info.json")
	_ = writeEventInfo(infoPath, info)

	pid, err := s.startUploadConversion(inputPath, eventDir, infoPath, presetForQuality(quality))
	if err != nil {
		s.logger.Warn("upload conversion failed", "event_id", eventID, "err", err)
		updateInfoStatus(infoPath, "failed", 0)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	recordInfoPID(infoPath, pid)

	s.logger.Info("upload received",
		"event_id", eventID, "filename", filename, "quality", quality, "pid", pid)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "event_id": eventID, "message": fmt.Sprintf("Started processing uploaded file: %s", filename), "status": "processing"})
}

func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	eventID := ""
	for i := 0; i < len(parts); i++ {
		if parts[i] == "upload" && i+1 < len(parts) {
			eventID = parts[i+1]
			break
		}
	}
	if !validID(eventID) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid event id"})
		return
	}
	eventDir := filepath.Join(s.settings.EventsDir, eventID)
	infoPath := filepath.Join(eventDir, "info.json")
	infoData, err := os.ReadFile(infoPath)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Event not found"})
		return
	}

	var info EventInfoFile
	if err := json.Unmarshal(infoData, &info); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}

	if info.Type == "upload" {
		s.reconcileJobStatus(&info, eventDir, infoPath)
	}

	s.writeJSON(w, http.StatusOK, uploadStatusPayload(eventID, info))
}

func (s *Server) handleCacheUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid multipart"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "file is required"})
		return
	}
	defer file.Close()

	quality := r.FormValue("quality_preset")
	if quality == "" {
		quality = r.FormValue("quality")
	}
	if quality == "" {
		quality = "standard"
	}

	eventID, eventDir, err := createEventDir(s.settings.CacheDir)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	filename := uploadFilename(header.Filename)

	inputPath := filepath.Join(eventDir, uploadSourceName(filename))
	if err := saveMultipartFile(file, inputPath); err != nil {
		_ = os.RemoveAll(eventDir)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}

	info := EventInfoFile{
		Name:       filename,
		CreatedAt:  time.Now().Format(time.RFC3339),
		Status:     "processing",
		SourceFile: filename,
		Type:       "local_upload",
		Quality:    quality,
	}
	infoPath := filepath.Join(eventDir, "info.json")
	_ = writeEventInfo(infoPath, info)

	pid, err := s.startUploadConversion(inputPath, eventDir, infoPath, presetForQuality(quality))
	if err != nil {
		updateInfoStatus(infoPath, "failed", 0)
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	recordInfoPID(infoPath, pid)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "event_id": eventID, "message": fmt.Sprintf("Started processing local file: %s", filename), "status": "processing"})
}

func (s *Server) handleCacheUploadStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	eventID := pathSegmentAfter(r.URL.Path, "upload")
	if eventID == "" || eventID == "status" {
		eventID = pathSegmentAfter(r.URL.Path, "cache")
	}
	if !validID(eventID) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid event id"})
		return
	}
	eventDir := filepath.Join(s.settings.CacheDir, eventID)
	infoPath := filepath.Join(eventDir, "info.json")
	infoData, err := os.ReadFile(infoPath)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Event not found"})
		return
	}

	var info EventInfoFile
	if err := json.Unmarshal(infoData, &info); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}

	if info.Type == "local_upload" {
		s.reconcileJobStatus(&info, eventDir, infoPath)
	}

	s.writeJSON(w, http.StatusOK, uploadStatusPayload(eventID, info))
}

func (s *Server) handleCacheUploads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	uploads := make([]map[string]interface{}, 0)
	entries, err := os.ReadDir(s.settings.CacheDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			infoPath := filepath.Join(s.settings.CacheDir, entry.Name(), "info.json")
			infoData, err := os.ReadFile(infoPath)
			if err != nil {
				continue
			}
			var info EventInfoFile
			if err := json.Unmarshal(infoData, &info); err != nil {
				continue
			}
			if info.Type == "local_upload" {
				uploads = append(uploads, map[string]interface{}{
					"event_id":   entry.Name(),
					"name":       info.Name,
					"status":     info.Status,
					"quality":    info.Quality,
					"created_at": info.CreatedAt,
				})
			}
		}
	}
	s.writeJSON(w, http.StatusOK, uploads)
}

func (s *Server) handleExternalSourceAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	var req ExternalSourceRequest
	if err := decodeJSON(r, &req); err != nil {
		s.logger.Warn("external source: bad request body", "err", err)
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": err.Error()})
		return
	}
	eventID, err := s.createExternalSource(req)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "event_id": eventID, "message": fmt.Sprintf("Started processing external source: %s", req.Name), "status": "processing"})
}

func (s *Server) createExternalSource(req ExternalSourceRequest) (string, error) {
	if req.Bitrate == 0 {
		req.Bitrate = 4000000
	}
	if req.Framerate == 0 {
		req.Framerate = 30
	}

	eventID, eventDir, err := createEventDir(s.settings.EventsDir)
	if err != nil {
		return "", err
	}

	info := EventInfoFile{
		Name:      req.Name,
		CreatedAt: time.Now().Format(time.RFC3339),
		Status:    "processing",
		SourceURL: req.URL,
		Type:      "external",
	}
	infoPath := filepath.Join(eventDir, "info.json")
	if err := writeEventInfo(infoPath, info); err != nil {
		return "", err
	}

	preset := ocffmpeg.SimplePreset{
		IDValue:       fmt.Sprintf("external-%dfps-%dbps", req.Framerate, req.Bitrate),
		LabelValue:    fmt.Sprintf("External %dfps @ %d bps", req.Framerate, req.Bitrate),
		W:             1920,
		H:             1080,
		FPSInt:        req.Framerate,
		GOPFrames:     req.Framerate * 2,
		VBitrate:      fmt.Sprintf("%d", req.Bitrate),
		ABitrate:      "128k",
		ProfileValue:  "high",
		LevelValue:    "4.1",
		EncoderPreset: "veryfast",
	}

	cfg := ocffmpeg.Config{
		Binary:    "ffmpeg",
		Mode:      ocffmpeg.ModeUpload,
		Input:     ocffmpeg.Input{Kind: ocffmpeg.InputNetwork, URL: req.URL},
		Preset:    preset,
		Encoder:   ocffmpeg.EncoderAuto,
		HWAccel:   ocffmpeg.HWAccelAuto,
		OutputDir: eventDir,
	}
	job, err := s.ffmpeg.StartJob(cfg, ocffmpeg.JobOptions{
		LogPath: filepath.Join(eventDir, "ffmpeg.log"),
		OnComplete: func(err error) {
			status := "completed"
			if err != nil {
				status = "failed"
			}
			updateInfoStatus(infoPath, status, 0)
			s.logger.Info("external source job complete",
				"event_id", eventID, "status", status, "err", err)
		},
	})
	if err != nil {
		updateInfoStatus(infoPath, "failed", 0)
		return "", err
	}
	recordInfoPID(infoPath, job.PID())
	s.logger.Info("external source job started",
		"event_id", eventID, "url", req.URL, "pid", job.PID())
	return eventID, nil
}

func (s *Server) handleExternalSourceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "method not allowed"})
		return
	}
	eventID := pathSegmentAfter(r.URL.Path, "external-sources")
	if !validID(eventID) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid event id"})
		return
	}
	eventDir := filepath.Join(s.settings.EventsDir, eventID)
	infoPath := filepath.Join(eventDir, "info.json")
	infoData, err := os.ReadFile(infoPath)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Event not found"})
		return
	}

	var info EventInfoFile
	if err := json.Unmarshal(infoData, &info); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}

	if info.Type == "external" {
		s.reconcileJobStatus(&info, eventDir, infoPath)
	}

	s.writeJSON(w, http.StatusOK, uploadStatusPayload(eventID, info))
}

// uploadStatusPayload assembles the JSON response for the three upload
// status endpoints (handleUploadStatus, handleCacheUploadStatus, and the
// per-id branch of handleCacheUploads). Centralising means the on-wire
// schema stays in lockstep — the encoder UI's poll path doesn't have to
// special-case which upload kind it's looking at.
//
// "progress" is a structured block (percent, frame, fps, speed, ...) when
// ffmpeg has emitted at least one -progress tick, otherwise null. The UI
// renders an indeterminate bar on null/missing, a real bar on percent>=0,
// and a frame/fps readout on percent==-1 (duration probe failed).
func uploadStatusPayload(eventID string, info EventInfoFile) map[string]interface{} {
	return map[string]interface{}{
		"event_id":             eventID,
		"status":               info.Status,
		"name":                 info.Name,
		"type":                 info.Type,
		"progress":             info.Progress, // nil ⇒ JSON null, populated ⇒ struct
		"source_duration_secs": info.SourceDurationSecs,
	}
}

// startUploadConversion spawns a one-shot HLS conversion via the internal
// Job runner. Two callbacks are wired:
//
//   - OnProgress writes the current ffmpeg -progress block (frame, fps,
//     speed, out_time_us) into info.json's "progress" key so the encoder
//     UI's poll of /api/(cache|events)/upload/{id}/status surfaces a
//     real progress bar instead of "processing...".
//   - OnComplete flips status to "completed" or "failed" once ffmpeg
//     exits and clears the pid field.
//
// Source duration is probed via ffprobe before the conversion starts;
// it's stored alongside the progress block so the UI can compute a
// percentage. Probe failure isn't fatal — the UI falls back to an
// indeterminate bar driven by frame/fps/speed.
//
// Replaces the previous bare exec.Command("ffmpeg", ...) implementation;
// every ffmpeg invocation in this binary now flows through internal/ffmpeg.
func (s *Server) startUploadConversion(inputPath, eventDir, infoPath string, preset ocffmpeg.QualityPreset) (int, error) {
	// Probe up front so progress percent is correct from the first
	// callback. Bounded with a short timeout so a hung ffprobe can't
	// block the upload accept path. Failure is logged but not fatal.
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelProbe()
	const ffmpegBinary = "ffmpeg"
	duration, perr := probeDurationSecs(probeCtx, ffmpegBinary, inputPath)
	if perr == nil {
		updateInfoDuration(infoPath, duration)
	} else {
		s.logger.Info("upload duration probe failed; UI will show indeterminate bar",
			"input", inputPath, "ffprobe", ffprobePath(ffmpegBinary), "err", perr)
	}

	cfg := ocffmpeg.Config{
		Binary:    ffmpegBinary,
		Mode:      ocffmpeg.ModeUpload,
		Input:     ocffmpeg.Input{Kind: ocffmpeg.InputFile, VideoDevice: inputPath},
		Preset:    preset,
		Encoder:   ocffmpeg.EncoderAuto, // VideoToolbox on macOS, NVENC/QSV/VAAPI on others
		HWAccel:   ocffmpeg.HWAccelAuto, // EffectiveHWAccel forces None for ModeUpload
		OutputDir: eventDir,
	}
	job, err := s.ffmpeg.StartJob(cfg, ocffmpeg.JobOptions{
		LogPath: filepath.Join(eventDir, "ffmpeg.log"),
		OnProgress: func(p ocffmpeg.Progress) {
			updateInfoProgress(infoPath, p, duration)
		},
		OnComplete: func(err error) {
			status := "completed"
			if err != nil {
				status = "failed"
			}
			updateInfoStatus(infoPath, status, 0)
			s.logger.Info("upload conversion complete",
				"input", inputPath, "status", status, "err", err)
		},
	})
	if err != nil {
		return 0, err
	}
	return job.PID(), nil
}

// reconcileJobStatus resolves a "processing" job whose completion
// callback never ran (e.g. the daemon restarted mid-conversion). Jobs
// tracked by this process are trusted; otherwise fall back to a PID
// liveness probe.
func (s *Server) reconcileJobStatus(info *EventInfoFile, eventDir, infoPath string) {
	if info.Status != "processing" || info.PID == "" {
		return
	}
	pid, err := parsePID(info.PID)
	if err != nil || s.ffmpeg.JobRunning(pid) || isProcessRunning(pid) {
		return
	}
	status := "failed"
	if _, err := os.Stat(filepath.Join(eventDir, "master.m3u8")); err == nil {
		status = "completed"
	}
	updateInfoStatus(infoPath, status, 0)
	info.Status = status
}

// infoMu serializes read-modify-write cycles on info.json files. Job
// completion callbacks run on their own goroutines and race the
// handler that records the PID; without this a fast-failing job's
// "failed" could be overwritten back to "processing".
var infoMu sync.Mutex

// recordInfoPID stores pid in info.json, but only while the job is still
// "processing" — if the completion callback already ran, leave it alone.
func recordInfoPID(infoPath string, pid int) {
	infoMu.Lock()
	defer infoMu.Unlock()
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return
	}
	var info map[string]interface{}
	if err := json.Unmarshal(data, &info); err != nil {
		return
	}
	if st, _ := info["status"].(string); st != "processing" {
		return
	}
	info["pid"] = fmt.Sprintf("%d", pid)
	updated, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return
	}
	_ = atomicfile.Write(infoPath, updated, 0644)
}

// uploadSourceName is the on-disk name for an uploaded source file. It
// is generated rather than client-chosen so an upload can never collide
// with files the pipeline writes into the same directory (info.json,
// ffmpeg.log, master.m3u8, init/segment files). Only a short
// alphanumeric extension is kept so FFmpeg can still probe by suffix;
// the original filename is kept as metadata in info.json.
func uploadSourceName(original string) string {
	ext := strings.TrimPrefix(filepath.Ext(original), ".")
	if ext == "" || len(ext) > 8 || strings.IndexFunc(ext, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) >= 0 {
		return "source.upload"
	}
	return "source." + strings.ToLower(ext)
}

// uploadFilename reduces a client-supplied multipart filename to a safe
// single path component.
func uploadFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	if name == "" || name == "." || name == ".." || name == "/" || name == "info.json" {
		return "video"
	}
	return name
}

// probeDurationSecs runs ffprobe to read the source's wall-clock length.
// Returns 0 on any error (binary missing, source unparseable, timeout) —
// callers treat 0 as "unknown" and the UI shows an indeterminate bar.
func probeDurationSecs(ctx context.Context, ffmpegBinary, path string) (float64, error) {
	cmd := exec.CommandContext(ctx,
		ffprobePath(ffmpegBinary),
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return 0, fmt.Errorf("%w: %s", err, msg)
		}
		return 0, err
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable duration %q: %w", strings.TrimSpace(string(out)), err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("non-positive duration %v", d)
	}
	return d, nil
}

// ffprobePath finds the ffprobe that ships alongside the ffmpeg binary
// in use, so a custom or bundled ffmpeg install doesn't silently fall
// back to whatever ffprobe (if any) is first on PATH. Falls back to
// plain "ffprobe" when no sibling exists.
func ffprobePath(ffmpegBinary string) string {
	if ffmpegBinary == "" {
		ffmpegBinary = "ffmpeg"
	}
	resolved, err := exec.LookPath(ffmpegBinary)
	if err != nil {
		return "ffprobe"
	}
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = real
	}
	sibling := filepath.Join(filepath.Dir(resolved), "ffprobe"+filepath.Ext(resolved))
	if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() {
		return sibling
	}
	return "ffprobe"
}

// updateInfoStatus rewrites info.json's status/pid fields atomically.
// Atomic so a crash mid-write cannot leave a truncated file that
// listEvents silently drops.
func updateInfoStatus(infoPath, status string, pid int) {
	infoMu.Lock()
	defer infoMu.Unlock()
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return
	}
	var info map[string]interface{}
	if err := json.Unmarshal(data, &info); err != nil {
		return
	}
	info["status"] = status
	if pid > 0 {
		info["pid"] = fmt.Sprintf("%d", pid)
	}
	if status == "completed" || status == "failed" || status == "stopped" {
		info["stopped_at"] = time.Now().Format(time.RFC3339)
		delete(info, "pid")
		// Once the job is done, snap progress to 100 on success so the
		// UI doesn't show "99.7% — completed". On failure leave progress
		// alone so the operator can see how far it got before the
		// decoder gave up.
		if status == "completed" {
			if p, ok := info["progress"].(map[string]interface{}); ok {
				p["percent"] = 100.0
				p["updated_at"] = time.Now().Format(time.RFC3339)
			}
		}
	}
	updated, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return
	}
	_ = atomicfile.Write(infoPath, updated, 0644)
}

// updateInfoDuration writes the probed source duration into info.json.
// Called once at upload start (before ffmpeg starts emitting progress
// blocks) so the first progress write can compute percent correctly.
func updateInfoDuration(infoPath string, secs float64) {
	infoMu.Lock()
	defer infoMu.Unlock()
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return
	}
	var info map[string]interface{}
	if err := json.Unmarshal(data, &info); err != nil {
		return
	}
	info["source_duration_secs"] = secs
	updated, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return
	}
	_ = atomicfile.Write(infoPath, updated, 0644)
}

// updateInfoProgress writes a single ffmpeg -progress snapshot into
// info.json. Called from the OnProgress callback — ffmpeg emits a block
// roughly every second by default, which is also the UI's natural poll
// cadence, so we don't throttle further. The write is atomic via
// atomicfile.Write so a partial write can never be observed by readers.
//
// durationSecs is the value returned by probeDurationSecs; pass 0 when
// the probe failed and percent will be reported as -1 (UI treats that
// as "indeterminate").
func updateInfoProgress(infoPath string, p ocffmpeg.Progress, durationSecs float64) {
	// Same lock as updateInfoStatus / recordInfoPID, so a progress tick
	// can't clobber the PID or a completion status written concurrently.
	infoMu.Lock()
	defer infoMu.Unlock()
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return
	}
	var info map[string]interface{}
	if err := json.Unmarshal(data, &info); err != nil {
		return
	}
	// A late tick that lands after OnComplete must not overwrite the
	// final snapshot (completed snaps percent to 100).
	if st, _ := info["status"].(string); st != "processing" {
		return
	}
	outSecs := float64(p.OutTimeUS) / 1_000_000.0
	percent := -1.0
	if durationSecs > 0 {
		percent = (outSecs / durationSecs) * 100.0
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
	}
	info["progress"] = map[string]interface{}{
		"percent":       percent,
		"frame":         p.Frame,
		"fps":           p.FPS,
		"speed":         p.Speed,
		"out_time_secs": outSecs,
		"bitrate_kbps":  p.BitrateKbps,
		"updated_at":    p.UpdatedAt.Format(time.RFC3339),
	}
	updated, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return
	}
	_ = atomicfile.Write(infoPath, updated, 0644)
}

func saveMultipartFile(file multipart.File, path string) error {
	dst, err := os.Create(path)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, file)
	return err
}
