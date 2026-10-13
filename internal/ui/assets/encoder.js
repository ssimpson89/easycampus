// EasyCampus Encoder — Alpine component.
//
// State model:
//   - server.* state is replaced wholesale on every status poll. The UI
//     mirrors it but tracks a small form state (event name, selected
//     preset, selected input) so a re-fetch doesn't clobber typing.
//   - All API calls use the /api/* mirrors per main.go.
//   - HLS preview is wired via vendored hls.js (window.Hls).

const F = window.OCFormat;

// Quality presets are fetched from the backend at /api/quality.
//
// The backend ships a tier matrix (consumer / prosumer / broadcast /
// cinema) defined in internal/quality. We render those tiers as
// <optgroup>s in the picker so the operator sees them grouped by
// operator-fitness rather than the previous flat list.
//
// FALLBACK_PRESETS keeps the UI usable when /api/quality is
// unreachable (older binary, network blip during init). The IDs are
// chosen to align with the broadcast tier table so a server upgrade
// later will resolve them without UI churn.
const FALLBACK_PRESETS = [
  { tier: "consumer", label: "Consumer", presets: [
    { id: "consumer-720p30",  label: "Consumer 720p30",  width: 1280, height: 720,  resolution: "1280x720",  fps: "30", fps_int: 30, video_bitrate_kbps: 3500, min_upload_kbps: 5500,  hw_encoders: ["h264_videotoolbox", "h264_nvenc", "libx264"] },
    { id: "consumer-1080p30", label: "Consumer 1080p30", width: 1920, height: 1080, resolution: "1920x1080", fps: "30", fps_int: 30, video_bitrate_kbps: 6000, min_upload_kbps: 8500,  hw_encoders: ["h264_videotoolbox", "h264_nvenc", "libx264"] },
  ]},
];
const DEFAULT_PRESET_ID = "consumer-1080p30";

// HLS preview retry policy: exponential backoff, then give up.
const PREVIEW_MAX_RETRIES = 6;
// Upload status polls stop after this many consecutive 404s.
const UPLOAD_404_LIMIT = 5;

document.addEventListener("alpine:init", () => {
  Alpine.data("encoder", () => ({

    // -------- Server-sourced --------
    stream:   { is_running: false, event_id: null, uptime_seconds: 0, last_error: null, ffmpeg_pid: null, quality_preset: "" },
    inputs:   [],
    events:   [],
    health:   null,

    // -------- Form state (saved to localStorage) --------
    eventName:     "",
    selectedInput: "0",
    // selectedPreset is an internal/quality preset ID
    // (e.g. "consumer-1080p30"). Legacy values like "low"/"standard"
    // from a previous release are migrated on restoreFormState().
    selectedPreset: DEFAULT_PRESET_ID,
    presetMenuOpen: false,

    // -------- UI state --------
    connectionOK:  true,
    nodeType:      "",      // NODE_TYPE from /api/config, shown in the topbar
    isStarting:    false,
    isStopping:    false,
    stopConfirm:   false,
    error:         "",
    toast:         "",
    // Screen-reader announcement. Changes only on start/stop so the
    // ticking uptime in the banner isn't read out every 2 s.
    liveAnnouncement: "",

    // -------- Local camera preview --------
    // getUserMedia only exists in secure contexts (HTTPS / localhost).
    // Over plain http from another machine the preview can't work, so
    // the panel is hidden rather than offering a button that fails.
    secureContext:  !!window.isSecureContext && !!navigator.mediaDevices,
    cameraReady:    false,
    cameraLoading:  false,
    cameraError:    "",
    browserDevices: [],

    // -------- Upload --------
    uploadFile:    null,
    uploadStatus:  "",
    uploadProgress:0,
    uploadEventId: "",

    // -------- External source form --------
    extName:    "",
    extUrl:     "",
    extLoading: false,
    extStatus:  "",

    // -------- Event preview / bookmarks --------
    selectedEvent:    null,
    previewHls:       null,
    previewReady:     false,
    previewPlaying:   false,
    previewLive:      false,
    previewAtEdge:    false,   // playhead within ~2 segments of the live edge
    previewStatus:    "",      // neutral status line ("Waiting for stream…")
    previewTime:      0,
    previewDuration:  0,
    previewSeek:      "",
    bookmarks:        [],
    newBookmarkName:  "",

    _statusTimer:  null,
    _eventsTimer:  null,
    _toastTimer:   null,
    _stopConfirmTimer: null,
    _userStopRequested: false,   // distinguishes operator stop from a crash
    _previewRetries: 0,
    _previewRetryTimer: null,

    // qualityTiers is the tier-grouped catalog fetched from /api/quality
    // at init. Each tier carries a label and a list of preset summaries.
    // Falls back to FALLBACK_PRESETS if the fetch fails so the picker
    // is always populated.
    qualityTiers: FALLBACK_PRESETS,

    // ============================================================
    // INIT
    // ============================================================
    async init() {
      this.restoreFormState();
      if (!this.eventName) {
        this.eventName = this.defaultEventName();
      }

      // Initial parallel fetches.
      await Promise.all([
        this.fetchInputs(),
        this.refreshStatus(),
        this.loadEvents(),
        this.fetchQualityCatalog(),
        this.loadConfig(),
      ]);

      // Persist form state on change.
      this.$watch("selectedPreset", () => this.saveFormState());
      this.$watch("selectedInput",  (val) => {
        this.saveFormState();
        this.syncPreviewToInput(val);
      });
      this.$watch("eventName", () => this.saveFormState());

      // Polling timers. Status is hot (2 s) since uptime ticks visibly;
      // event list is cold (30 s) since it only changes on user action.
      this._statusTimer = setInterval(() => this.refreshStatus(), 2000);
      this._eventsTimer = setInterval(() => this.loadEvents(), 30000);

      // Warn the operator if they try to navigate away mid-stream.
      window.addEventListener("beforeunload", (e) => {
        if (this.stream.is_running) {
          e.preventDefault();
          e.returnValue = "A stream is currently running. Are you sure you want to leave?";
        }
      });

      // Try to bring the local camera preview up automatically if the
      // browser has already granted permission. We use enumerateDevices()
      // as a feature probe — if labels come back populated, permission
      // has been granted previously and getUserMedia won't prompt.
      if (!this.secureContext) return;
      try {
        const devs = await navigator.mediaDevices.enumerateDevices();
        const vid = devs.filter(d => d.kind === "videoinput");
        this.browserDevices = vid;
        if (vid.length && vid.some(d => d.label)) {
          // Don't wait — if it fails, the operator sees the "Start camera
          // preview" button as a fallback.
          this.initCamera().catch(() => {});
        }
      } catch (_) {}
    },

    // ============================================================
    // STATE
    // ============================================================
    get isLive() { return !!this.stream?.is_running; },

    // isLiveEvent: an event is live only when it is the event the
    // running stream is writing to. ev.status alone can be stale (a
    // crashed ffmpeg used to leave "running" behind).
    isLiveEvent(ev) {
      return !!ev && this.isLive && ev.id === this.stream?.event_id;
    },

    // livePreset resolves the preset the *server* is encoding with
    // (StreamStatus.quality_preset), not whatever this browser has
    // selected in the idle form. Unknown IDs still show their raw ID.
    get livePreset() {
      const id = this.stream?.quality_preset;
      if (!id) return null;
      return this.allPresets.find(p => p.id === id) || { id, label: id };
    },

    liveBitrateLabel() {
      const p = this.livePreset;
      if (!p || !p.video_bitrate_kbps) return "—";
      return F.formatBitrate(p.video_bitrate_kbps * 1000);
    },

    get liveUptime() {
      if (!this.stream?.uptime_seconds) return "0:00:00";
      const s = Math.floor(this.stream.uptime_seconds);
      const h = Math.floor(s / 3600);
      const m = Math.floor((s % 3600) / 60);
      const sec = s % 60;
      return `${h}:${String(m).padStart(2,"0")}:${String(sec).padStart(2,"0")}`;
    },

    get liveHeadline() {
      if (this.stream.event_id) {
        const ev = this.events.find(e => e.id === this.stream.event_id);
        if (ev) return ev.name;
      }
      return this.eventName || "Live stream";
    },

    // allPresets returns the flat list across every tier in display
    // order. Used by currentPreset() and the dropdown change handlers.
    get allPresets() {
      const out = [];
      for (const t of this.qualityTiers) {
        if (!Array.isArray(t.presets)) continue;
        for (const p of t.presets) out.push(p);
      }
      return out;
    },

    get currentPreset() {
      const flat = this.allPresets;
      return flat.find(p => p.id === this.selectedPreset) || flat[0] || null;
    },

    // presetSubline renders the per-preset description shown in the
    // <option> label and under the dropdown. Subline keeps the four
    // load-bearing numbers the operator cares about: resolution, fps,
    // target bitrate, and minimum upload bandwidth.
    presetSubline(p) {
      if (!p) return "";
      const res = p.resolution || `${p.width}x${p.height}`;
      const fps = p.fps || p.fps_int;
      const bps = `${p.video_bitrate_kbps >= 1000
        ? (p.video_bitrate_kbps / 1000).toFixed(p.video_bitrate_kbps % 1000 === 0 ? 0 : 1) + " Mbps"
        : p.video_bitrate_kbps + " kbps"}`;
      const up = `min ${(p.min_upload_kbps / 1000).toFixed(p.min_upload_kbps % 1000 === 0 ? 0 : 1)} Mbps up`;
      return `${res} · ${fps} fps · ${bps} · ${up}`;
    },

    get currentInput() {
      return this.inputs.find(i => i.id === this.selectedInput) || null;
    },

    get canStart() {
      return !this.isLive && !this.isStarting && !!this.eventName.trim() && this.inputs.length > 0;
    },

    // Pre-flight rows shown when idle. Each row has a status (good/warn/bad)
    // and a label/detail. Same pattern as easystream's pre-flight check.
    get preflightRows() {
      const rows = [];

      rows.push({
        label: "FFmpeg",
        status: this.health?.ffmpeg_available ? "good" : "bad",
        detail: this.health?.ffmpeg_available
          ? "Ready to encode"
          : "Not detected. Install FFmpeg to record or stream.",
      });

      if (this.inputs.length > 0) {
        const sel = this.currentInput;
        rows.push({
          label: "Video input",
          status: sel ? "good" : "warn",
          detail: sel ? sel.name : "Pick a camera or capture device",
        });
      } else {
        rows.push({
          label: "Video input",
          status: "warn",
          detail: "No capture devices found. Click Refresh after connecting one.",
        });
      }

      rows.push({
        label: "Storage",
        status: this.health?.events_dir_writable ? "good" : "warn",
        detail: this.health?.events_dir_writable
          ? `Events dir: ${this.health?.events_dir || "ready"}`
          : "Events directory is not writable.",
      });

      if (this.secureContext) {
        rows.push({
          label: "Camera preview",
          status: this.cameraReady ? "good" : this.cameraError ? "warn" : "idle",
          detail: this.cameraReady
            ? "Preview of this browser's camera is running"
            : this.cameraError || "Optional. Click Start camera preview to check this browser's camera.",
        });
      }

      return rows;
    },

    preflightStatusText(status) {
      if (status === "good") return "OK:";
      if (status === "warn") return "Warning:";
      if (status === "bad")  return "Problem:";
      return "Not checked:";
    },

    // ============================================================
    // FORM PERSISTENCE
    // ============================================================
    restoreFormState() {
      try {
        const raw = localStorage.getItem("oc_encoder_form");
        if (!raw) return;
        const p = JSON.parse(raw);
        if (p.preset) {
          // Legacy low/standard/high/ultra map onto consumer tier IDs.
          // The migration keeps existing operator muscle memory working
          // — the dropdown will surface the new label after the fetch.
          const legacy = {
            low:      "consumer-720p30",
            standard: "consumer-1080p30",
            medium:   "consumer-1080p30",
            high:     "consumer-1080p30",
            ultra:    "prosumer-1080p60",
          };
          this.selectedPreset = legacy[p.preset] || p.preset;
        }
        if (p.input)        this.selectedInput  = p.input;
        if (p.eventName)    this.eventName      = p.eventName;
      } catch (_) {}
    },

    saveFormState() {
      try {
        localStorage.setItem("oc_encoder_form", JSON.stringify({
          preset:    this.selectedPreset,
          input:     this.selectedInput,
          eventName: this.eventName,
        }));
      } catch (_) {}
    },

    defaultEventName() {
      const d = new Date();
      const fmt = d.toLocaleString(undefined, {
        month: "short", day: "numeric", year: "numeric",
        hour: "numeric", minute: "2-digit",
      });
      return `Service · ${fmt}`;
    },

    // ============================================================
    // API
    // ============================================================
    async api(path, { method = "GET", body, headers = {} } = {}) {
      const opts = { method, headers: { ...headers } };
      if (body !== undefined) {
        opts.headers["Content-Type"] = "application/json";
        opts.body = JSON.stringify(body);
      }
      const r = await fetch(path, opts);
      const text = await r.text();
      let data = null;
      if (text) {
        try { data = JSON.parse(text); } catch (_) { data = { detail: text }; }
      }
      if (!r.ok) {
        const msg = (data && (data.detail || data.error)) || `HTTP ${r.status}`;
        const err = new Error(msg);
        err.status = r.status;
        throw err;
      }
      return data;
    },

    async loadConfig() {
      try {
        const cfg = await this.api("/api/config");
        this.nodeType = cfg?.NODE_TYPE || "";
      } catch (_) {}
    },

    async refreshStatus() {
      try {
        const s = await this.api("/api/status");
        const wasRunning = !!this.stream?.is_running;
        this.stream = s || this.stream;
        const nowRunning = !!this.stream?.is_running;
        if (wasRunning && !nowRunning) {
          // running -> stopped. If the operator didn't ask for it, the
          // encoder crashed or ffmpeg exited; say so loudly.
          if (!this._userStopRequested) {
            this.error = this.stream.last_error || "Stream stopped unexpectedly";
            this.liveAnnouncement = "Stream stopped unexpectedly";
          } else {
            this.liveAnnouncement = "Stream stopped";
          }
          this._userStopRequested = false;
          this.loadEvents();
        } else if (!wasRunning && nowRunning) {
          this.liveAnnouncement = "Stream is live";
        }
        // Health gets a second probe occasionally; cheap enough to bundle.
        const h = await this.api("/api/health").catch(() => null);
        if (h) this.health = h;
        this.connectionOK = true;
      } catch (_) {
        this.connectionOK = false;
      }
    },

    async fetchInputs() {
      try {
        const data = await this.api("/api/inputs");
        this.inputs = Array.isArray(data) ? data : [];
        // If saved input is no longer present, fall back to first available.
        if (this.inputs.length > 0 && !this.inputs.some(i => i.id === this.selectedInput)) {
          this.selectedInput = this.inputs[0].id;
        }
        this.resyncSelect("inputSelect", this.selectedInput);
      } catch (e) {
        console.error("Failed to fetch inputs:", e);
      }
    },

    async loadEvents() {
      try {
        const data = await this.api("/api/events");
        this.events = Array.isArray(data) ? data : [];
      } catch (e) {
        console.error("Failed to load events:", e);
      }
    },

    // fetchQualityCatalog pulls the tier-grouped preset list from the
    // backend (/api/quality). Falls back silently to FALLBACK_PRESETS
    // when the endpoint is unavailable so the picker stays populated.
    async fetchQualityCatalog() {
      try {
        const data = await this.api("/api/quality");
        if (data && Array.isArray(data.tiers) && data.tiers.length > 0) {
          this.qualityTiers = data.tiers;
          // Validate the persisted preset against the new catalog —
          // if it disappeared (renamed / removed), fall back to a
          // sensible default rather than silently failing on submit.
          if (!this.allPresets.some(p => p.id === this.selectedPreset)) {
            this.selectedPreset = (this.allPresets.find(p => p.id === DEFAULT_PRESET_ID)
              || this.allPresets[0] || {}).id || DEFAULT_PRESET_ID;
          }
          this.resyncSelect("presetSelect", this.selectedPreset);
        }
      } catch (e) {
        console.warn("Failed to fetch quality catalog, using fallback:", e);
      }
    },

    // ============================================================
    // STREAM START / STOP
    // ============================================================
    async startStream() {
      if (!this.canStart) return;
      this.isStarting = true;
      this.error = "";
      try {
        const sel = this.currentInput;
        if (!sel) throw new Error("Please pick a video input.");
        const preset = this.currentPreset;
        if (!preset) throw new Error("Please pick a quality preset.");
        // We send both quality_preset (the canonical identifier the
        // supervisor resolves into a full tier preset) and the legacy
        // video_bitrate / video_framerate fields so an older backend
        // build that doesn't yet know about quality_preset still gets
        // a workable envelope. The backend prefers quality_preset
        // when set.
        const bitrateBps = (preset.video_bitrate_kbps || 0) * 1000;
        await this.api("/api/stream/start", {
          method: "POST",
          body: {
            event_name:           this.eventName.trim(),
            quality_preset:       preset.id,
            video_bitrate:        String(bitrateBps),
            video_framerate:      preset.fps_int || parseInt(preset.fps, 10) || 30,
            video_device_index:   sel.id,
            video_device_format:  sel.format,
            video_device_type:    sel.device_type,
          },
        });
        this.showToast("Stream started", "good");
        await this.refreshStatus();
        await this.loadEvents();
      } catch (e) {
        this.error = e.message;
        this.showToast(e.message, "bad");
      } finally {
        this.isStarting = false;
      }
    },

    // resyncSelect re-applies a model value to a <select> after its
    // <option>s arrive asynchronously (x-for renders after x-model ran,
    // so the browser would otherwise show the first option).
    resyncSelect(ref, value) {
      this.$nextTick(() => {
        const el = this.$refs[ref];
        if (el && value != null) el.value = String(value);
      });
    },

    showStopConfirm() {
      this.stopConfirm = true;
      this.$nextTick(() => this.$refs.confirmStop?.focus?.());
      clearTimeout(this._stopConfirmTimer);
      this._stopConfirmTimer = setTimeout(() => { this.stopConfirm = false; }, 6000);
    },

    cancelStop() {
      clearTimeout(this._stopConfirmTimer);
      this._stopConfirmTimer = null;
      this.stopConfirm = false;
    },

    async confirmStop() {
      this.cancelStop();
      this.isStopping = true;
      this._userStopRequested = true;
      try {
        await this.api("/api/stream/stop", { method: "POST" });
        this.showToast("Stream stopped", "good");
        await this.refreshStatus();
        await this.loadEvents();
      } catch (e) {
        this._userStopRequested = false;
        this.error = e.message;
        this.showToast(e.message, "bad");
      } finally {
        this.isStopping = false;
      }
    },

    // ============================================================
    // CAMERA PREVIEW (in-browser)
    // ============================================================
    async initCamera(deviceId = null) {
      if (!this.secureContext) return;
      this.cameraLoading = true;
      this.cameraError = "";
      try {
        const video = this.$refs.cameraVideo;
        if (!video) return;

        // Stop any prior tracks.
        if (video.srcObject) {
          video.srcObject.getTracks().forEach(t => t.stop());
        }

        const constraints = {
          video: { width: 1280, height: 720, ...(deviceId ? { deviceId: { exact: deviceId } } : {}) },
          audio: false,
        };
        const stream = await navigator.mediaDevices.getUserMedia(constraints);
        video.srcObject = stream;
        this.cameraReady = true;

        // Refresh the device list now that we have permission to read labels.
        const devs = await navigator.mediaDevices.enumerateDevices();
        this.browserDevices = devs.filter(d => d.kind === "videoinput");
      } catch (err) {
        this.cameraError = err.message || String(err);
        this.cameraReady = false;
      } finally {
        this.cameraLoading = false;
      }
    },

    // Match the backend-chosen video device to a browser MediaDevice so
    // the local preview shows the same camera the backend will encode.
    // Fuzzy match because OS-side and getUserMedia labels diverge in
    // capitalization and (Built-in) suffixes.
    async syncPreviewToInput(backendId) {
      const backendDevice = this.inputs.find(i => i.id === backendId);
      if (!this.secureContext || !backendDevice || this.browserDevices.length === 0) return;
      const target = backendDevice.name.toLowerCase();
      const match = this.browserDevices.find(d => {
        const lbl = (d.label || "").toLowerCase();
        return lbl.includes(target) || (lbl && target.includes(lbl));
      });
      if (match) {
        try { await this.initCamera(match.deviceId); } catch (_) {}
      }
    },

    stopCamera() {
      const video = this.$refs.cameraVideo;
      if (video && video.srcObject) {
        video.srcObject.getTracks().forEach(t => t.stop());
        video.srcObject = null;
      }
      this.cameraReady = false;
    },

    // ============================================================
    // EVENT PREVIEW (HLS) + BOOKMARKS
    // ============================================================
    async selectEventForPreview(ev) {
      this.selectedEvent = ev;
      this.previewReady = false;
      this.previewPlaying = false;
      this.previewTime = 0;
      this.previewDuration = 0;
      this.previewLive = false;
      this.previewAtEdge = false;
      this.previewStatus = "";
      this._previewRetries = 0;
      this.bookmarks = [];
      this.newBookmarkName = "";

      await this.$nextTick();
      this.attachPreview();
      this.loadEventBookmarks();
    },

    attachPreview({ retry = false } = {}) {
      const video = this.$refs.previewVideo;
      if (!video || !this.selectedEvent) return;

      clearTimeout(this._previewRetryTimer);
      this._previewRetryTimer = null;
      if (!retry) {
        this._previewRetries = 0;
        this.previewStatus = "";
      }
      if (this.previewHls) {
        try { this.previewHls.destroy(); } catch (_) {}
        this.previewHls = null;
      }

      const eventId  = this.selectedEvent.id;
      const masterUrl = `/events/${eventId}/master.m3u8`;
      const isLiveEvent = this.isLiveEvent(this.selectedEvent);

      if (window.Hls && Hls.isSupported()) {
        const hls = new Hls({
          enableWorker: false,
          lowLatencyMode: false,
          maxBufferLength: 10,
          maxMaxBufferLength: 20,
          maxBufferSize: 50 * 1000 * 1000,
          liveSyncDurationCount: 2,
          liveMaxLatencyDurationCount: 5,
          backBufferLength: 30,
          autoStartLoad: true,
          startLevel: 0,
          testBandwidth: false,
          progressive: true,
        });
        hls.loadSource(masterUrl);
        hls.attachMedia(video);
        hls.on(Hls.Events.MANIFEST_PARSED, (_e, data) => {
          this._previewRetries = 0;
          this.previewStatus = "";
          this.previewReady = true;
          this.previewDuration = video.duration || 0;
          this.previewTime = video.currentTime || 0;
          const lvl = data.levels?.[0]?.details;
          this.previewLive = lvl ? !!lvl.live : isLiveEvent;
          if (this.previewLive && video.seekable.length > 0) {
            const live = video.seekable.end(video.seekable.length - 1);
            video.currentTime = Math.max(0, live - 5);
            this.previewTime = video.currentTime;
          }
        });
        hls.on(Hls.Events.LEVEL_LOADED, (_e, data) => {
          if (data?.details) this.previewLive = !!data.details.live;
        });
        hls.on(Hls.Events.FRAG_LOADED, () => {
          if (this._previewRetries) { this._previewRetries = 0; this.previewStatus = ""; }
        });
        hls.on(Hls.Events.ERROR, (_e, data) => {
          if (!data.fatal) return;
          if (data.type === Hls.ErrorTypes.NETWORK_ERROR) {
            this.schedulePreviewRetry(hls, data);
          } else if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
            hls.recoverMediaError();
          } else {
            try { hls.destroy(); } catch (_) {}
            if (this.previewHls === hls) this.previewHls = null;
            this.previewStatus = "";
            this.error = `Preview error: ${data.details || data.type}`;
          }
        });
        this.previewHls = hls;
      } else if (video.canPlayType("application/vnd.apple.mpegurl")) {
        video.src = masterUrl;
        video.addEventListener("loadedmetadata", () => {
          this.previewReady = true;
          this.previewDuration = video.duration || 0;
          this.previewLive = isLiveEvent;
        }, { once: true });
      }
    },

    // schedulePreviewRetry: exponential backoff (1 s, 2 s, 4 s … capped
    // at 15 s) for fatal network errors, giving up after
    // PREVIEW_MAX_RETRIES. A manifest that failed to load is torn down
    // and re-attached; anything else just restarts loading.
    schedulePreviewRetry(hls, data) {
      if (this.previewHls !== hls) return;
      if (this._previewRetries >= PREVIEW_MAX_RETRIES) {
        try { hls.destroy(); } catch (_) {}
        this.previewHls = null;
        this.previewStatus = "";
        this.error = "Preview stream unavailable. Check that the event still has segments, then select it again.";
        return;
      }
      const delay = Math.min(1000 * 2 ** this._previewRetries, 15000);
      this._previewRetries++;
      this.previewStatus = "Waiting for stream…";
      const manifestFailed = [
        Hls.ErrorDetails.MANIFEST_LOAD_ERROR,
        Hls.ErrorDetails.MANIFEST_LOAD_TIMEOUT,
      ].includes(data.details);
      clearTimeout(this._previewRetryTimer);
      this._previewRetryTimer = setTimeout(() => {
        if (this.previewHls !== hls) return;
        if (manifestFailed) this.attachPreview({ retry: true });
        else hls.startLoad();
      }, delay);
    },

    // updatePreviewEdge flags whether the playhead is within ~2 target
    // durations of the live edge; the "Live edge" badge only shows then.
    updatePreviewEdge(video) {
      if (!this.previewLive || !video || video.seekable.length === 0) {
        this.previewAtEdge = false;
        return;
      }
      const hls = this.previewHls;
      const lvl = hls?.levels?.[Math.max(0, hls.currentLevel)]?.details;
      const target = lvl?.targetduration || 6;
      const edge = video.seekable.end(video.seekable.length - 1);
      this.previewAtEdge = (edge - video.currentTime) <= target * 2;
    },

    previewGoLive() {
      const v = this.$refs.previewVideo;
      if (!v) return;
      let t = this.previewHls?.liveSyncPosition;
      if (!Number.isFinite(t) && v.seekable.length > 0) t = v.seekable.end(v.seekable.length - 1) - 3;
      if (!Number.isFinite(t)) return;
      v.currentTime = Math.max(0, t);
      this.previewTime = v.currentTime;
      v.play().catch(() => {});
    },

    // <video> event handlers (wired via x-on:* on the element)
    onPreviewTimeUpdate(e)  { this.previewTime = e.target.currentTime; this.updatePreviewEdge(e.target); },
    onPreviewLoadedMeta(e)  { this.previewDuration = e.target.duration || 0; this.previewReady = true; },
    onPreviewDurationChange(e) {
      const d = e.target.duration;
      if (d && d > 0 && d !== Infinity) this.previewDuration = d;
    },
    onPreviewPlay()         { this.previewPlaying = true;  },
    onPreviewPause()        { this.previewPlaying = false; },

    togglePreviewPlay() {
      const v = this.$refs.previewVideo;
      if (!v) return;
      if (v.paused) v.play().catch(() => {});
      else          v.pause();
    },

    previewFrameStep(dir) {
      const v = this.$refs.previewVideo;
      if (!v) return;
      v.currentTime += dir / F.eventFps(this.selectedEvent);
    },

    previewJump(seconds) {
      const v = this.$refs.previewVideo;
      if (!v) return;
      let t = v.currentTime + seconds;
      if (this.previewLive && v.seekable.length > 0) {
        t = Math.max(v.seekable.start(0), Math.min(t, v.seekable.end(0)));
      } else {
        t = Math.max(0, Math.min(v.duration || 0, t));
      }
      v.currentTime = t;
    },

    previewGoto() {
      const t = F.parseTime(this.previewSeek);
      if (t === null) return;
      const v = this.$refs.previewVideo;
      if (!v) return;
      let target = t;
      if (this.previewLive && v.seekable.length > 0) {
        target = Math.max(v.seekable.start(0), Math.min(target, v.seekable.end(0)));
      } else {
        target = Math.max(0, Math.min(v.duration || 0, target));
      }
      v.currentTime = target;
    },

    onPreviewScrub(e) {
      const v = this.$refs.previewVideo;
      if (!v) return;
      let t = parseFloat(e.target.value);
      if (this.previewLive && v.seekable.length > 0) {
        t = Math.max(v.seekable.start(0), Math.min(t, v.seekable.end(0)));
      } else {
        t = Math.max(0, Math.min(v.duration || 0, t));
      }
      v.currentTime = t;
      this.previewTime = t;
      e.target.style.setProperty("--scrub", `${(t / (v.duration || 1) * 100).toFixed(2)}%`);
    },

    async loadEventBookmarks() {
      if (!this.selectedEvent) { this.bookmarks = []; return; }
      try {
        const data = await this.api(`/api/events/${this.selectedEvent.id}/bookmarks`).catch(() => null);
        const list = Array.isArray(data?.bookmarks) ? data.bookmarks : [];
        this.bookmarks = list.sort((a, b) => (a.frame || 0) - (b.frame || 0));
      } catch (_) {
        this.bookmarks = [];
      }
    },

    async createBookmark() {
      if (!this.selectedEvent) return;
      const v = this.$refs.previewVideo;
      const timePos = v?.currentTime || 0;
      const fps = F.eventFps(this.selectedEvent);
      const frame = Math.round(timePos * fps);
      const name = this.newBookmarkName.trim() || `Frame ${frame}`;
      const bookmark = {
        id: `bm_enc_${Date.now()}`,
        name,
        frame,
        fps,
        time_pos: timePos,
        created_at: new Date().toISOString(),
        source: "encoder",
        color: "blue",
      };
      try {
        await this.api(`/api/events/${this.selectedEvent.id}/bookmarks`, { method: "POST", body: bookmark });
        this.newBookmarkName = "";
        await this.loadEventBookmarks();
      } catch (e) {
        this.showToast(e.message, "bad");
      }
    },

    async deleteBookmark(bookmark) {
      if (!this.selectedEvent) return;
      try {
        await this.api(`/api/events/${this.selectedEvent.id}/bookmarks/${bookmark.id}`, { method: "DELETE" });
        await this.loadEventBookmarks();
      } catch (e) {
        this.showToast(e.message, "bad");
      }
    },

    jumpToBookmark(bookmark) {
      const v = this.$refs.previewVideo;
      if (!v) return;
      let t = bookmark.time_pos || 0;
      if (this.previewLive && v.seekable.length > 0) {
        t = Math.max(v.seekable.start(0), Math.min(t, v.seekable.end(0)));
      }
      v.currentTime = t;
      this.previewTime = t;
      v.play().catch(() => {});
    },

    bookmarkLeft(b) {
      if (!this.previewDuration) return "0%";
      const t = (b.time_pos != null) ? b.time_pos : (b.frame || 0) / (b.fps || 30);
      return `${Math.max(0, Math.min(100, (t / this.previewDuration) * 100)).toFixed(2)}%`;
    },

    scrubFill() {
      if (!this.previewDuration) return "0%";
      return `${Math.max(0, Math.min(100, (this.previewTime / this.previewDuration) * 100)).toFixed(2)}%`;
    },

    // ============================================================
    // EVENT MANAGEMENT
    // ============================================================
    async deleteEvent(ev) {
      if (!confirm(`Delete event "${ev.name}"? This cannot be undone.`)) return;
      try {
        await this.api(`/api/events/${ev.id}`, { method: "DELETE" });
        if (this.selectedEvent?.id === ev.id) {
          clearTimeout(this._previewRetryTimer);
          if (this.previewHls) { try { this.previewHls.destroy(); } catch (_) {} this.previewHls = null; }
          this.selectedEvent = null;
        }
        await this.loadEvents();
        this.showToast("Event deleted", "good");
      } catch (e) {
        this.showToast(e.message, "bad");
      }
    },

    // ============================================================
    // UPLOAD / EXTERNAL SOURCE
    // ============================================================
    async pickFile() {
      this.$refs.fileInput?.click();
    },

    async onFileChosen(e) {
      const file = e.target.files?.[0];
      if (!file) return;
      this.uploadFile = file;
      this.uploadStatus = "Uploading…";
      this.uploadProgress = 0;
      this.uploadEventId = "";

      const fd = new FormData();
      fd.append("file", file);
      // quality_preset is the new canonical field. We still send
      // "quality" with the same value so older backend builds (which
      // don't yet inspect quality_preset) keep accepting the upload.
      fd.append("quality_preset", this.selectedPreset);
      fd.append("quality", this.selectedPreset);

      try {
        const r = await fetch("/api/events/upload", { method: "POST", body: fd });
        if (!r.ok) throw new Error(F.errorDetail(await r.text(), r.status));
        const data = await r.json();
        this.uploadEventId = data.event_id;
        this.uploadStatus = "Processing…";
        this.pollUploadStatus(data.event_id);
      } catch (err) {
        this.uploadStatus = "Failed";
        this.showToast(`Upload failed: ${err.message}`, "bad");
      } finally {
        // Reset the file input so the same file can be re-chosen.
        e.target.value = "";
      }
    },

    async pollUploadStatus(eventId) {
      // A 404 right after upload can be a race with the status file
      // being written, so tolerate a few before giving up.
      let notFound = 0;
      const tick = async () => {
        let data;
        try {
          data = await this.api(`/api/events/upload/${eventId}/status`);
        } catch (e) {
          if (e.status === 404 && ++notFound >= UPLOAD_404_LIMIT) {
            this.uploadStatus = "Failed";
            this.showToast("Upload status not found. The upload may have been removed.", "bad");
            return;
          }
          setTimeout(tick, 2000);
          return;
        }
        notFound = 0;
        if (data?.status === "completed") {
          this.uploadStatus = "Ready";
          this.uploadProgress = 100;
          await this.loadEvents();
          setTimeout(() => { this.uploadStatus = ""; this.uploadProgress = 0; }, 3000);
          return;
        }
        if (data?.status === "failed") {
          this.uploadStatus = "Failed";
          this.showToast("Video processing failed", "bad");
          return;
        }
        this.uploadStatus = "Processing video…";
        this.uploadProgress = Math.min(data?.progress || 0, 95);
        setTimeout(tick, 2000);
      };
      tick();
    },

    async addExternalSource() {
      if (!this.extName.trim() || !this.extUrl.trim()) {
        this.showToast("Name and URL are both required.", "bad");
        return;
      }
      this.extLoading = true;
      this.extStatus = "Downloading…";
      try {
        const preset = this.currentPreset;
        await this.api("/api/external-sources", {
          method: "POST",
          body: {
            name: this.extName.trim(),
            url:  this.extUrl.trim(),
            bitrate:   (preset?.video_bitrate_kbps || 0) * 1000,
            framerate: preset?.fps_int || parseInt(preset?.fps, 10) || 30,
            quality_preset: preset?.id || "",
          },
        });
        this.extName = "";
        this.extUrl = "";
        this.extStatus = "Added";
        this.showToast("External source added", "good");
        await this.loadEvents();
        setTimeout(() => { this.extStatus = ""; }, 3000);
      } catch (e) {
        this.extStatus = "Failed";
        this.showToast(e.message, "bad");
      } finally {
        this.extLoading = false;
      }
    },

    // ============================================================
    // UTIL
    // ============================================================
    showToast(message, kind = "") {
      this.toast = message;
      clearTimeout(this._toastTimer);
      this._toastTimer = setTimeout(() => { this.toast = ""; }, 3500);
      this._lastToastKind = kind;
    },
    get toastKind() { return this._lastToastKind || ""; },

    eventIcon(ev) {
      if (this.isLiveEvent(ev))       return "recording";
      if (ev.type === "upload")       return "upload";
      if (ev.type === "local_upload") return "local";
      return "standard";
    },

    // Formatters surfaced into the template so x-text bindings stay short.
    fmtTime:    (s) => F.formatTime(s),
    fmtBytes:   (b) => F.formatBytes(b),
    fmtDate:    (d) => F.formatDate(d),
    presetSummary: (p) => F.presetSummary(p),
  }));
});
