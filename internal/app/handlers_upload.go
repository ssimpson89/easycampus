package app

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
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

	inputPath := filepath.Join(eventDir, filename)
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

	s.writeJSON(w, http.StatusOK, map[string]interface{}{"event_id": eventID, "status": info.Status, "name": info.Name, "type": info.Type, "progress": 0})
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

	inputPath := filepath.Join(eventDir, filename)
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

	s.writeJSON(w, http.StatusOK, map[string]interface{}{"event_id": eventID, "status": info.Status, "name": info.Name, "type": info.Type, "progress": 0})
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

	s.writeJSON(w, http.StatusOK, map[string]interface{}{"event_id": eventID, "status": info.Status, "name": info.Name, "type": info.Type, "progress": 0})
}

// startUploadConversion spawns a one-shot HLS conversion via the internal
// Job runner. The status callback updates info.json when ffmpeg exits.
//
// Replaces the previous bare exec.Command("ffmpeg", ...) implementation;
// every ffmpeg invocation in this binary now flows through internal/ffmpeg.
func (s *Server) startUploadConversion(inputPath, eventDir, infoPath string, preset ocffmpeg.QualityPreset) (int, error) {
	cfg := ocffmpeg.Config{
		Binary:    "ffmpeg",
		Mode:      ocffmpeg.ModeUpload,
		Input:     ocffmpeg.Input{Kind: ocffmpeg.InputFile, VideoDevice: inputPath},
		Preset:    preset,
		Encoder:   ocffmpeg.EncoderAuto, // VideoToolbox on macOS, NVENC/QSV/VAAPI on others
		HWAccel:   ocffmpeg.HWAccelAuto, // matched decode + encode path where possible
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

// uploadFilename reduces a client-supplied multipart filename to a safe
// single path component.
func uploadFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	if name == "" || name == "." || name == ".." || name == "/" || name == "info.json" {
		return "video"
	}
	return name
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
