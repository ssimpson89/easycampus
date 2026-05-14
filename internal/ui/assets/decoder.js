// EasyCampus Decoder — Alpine component.
//
// Responsibilities:
//   1. Connect to a remote encoder, list events, and either stream them
//      live or transparently cache them locally before playback.
//   2. Drive a single in-page HLS player (browser-side) plus an optional
//      hardware output (mpv via the /api/output endpoints) that mirrors
//      the browser's position.
//   3. Upload local files for offline playback (separate from encoder
//      uploads; these live in /cache/ instead of /events/).
//   4. Maintain encoder + decoder bookmarks for frame-accurate cues.
//
// Browser is the source of truth for position; the hardware output
// follows. Sync is loose (2 s drift check) for steady-state playback
// and explicit (single seek POST) on user-initiated seeks.

const F = window.OCFormat;

// Mirror of the encoder quality presets so the upload form lets the
// operator pick a target before uploading. /api/cache/upload accepts the
// same "low/standard/high/ultra" strings.
const UPLOAD_PRESETS = [
  { id: "low",      label: "Low (720p · 2 Mbps)" },
  { id: "standard", label: "Standard (1080p · 4 Mbps)" },
  { id: "high",     label: "High (1080p · 6 Mbps)" },
  { id: "ultra",    label: "Ultra (1080p · 8 Mbps)" },
];

document.addEventListener("alpine:init", () => {
  Alpine.data("decoder", () => ({

    // -------- Connection / events --------
    encoderUrl:    "",
    streamUrl:     "",
    savedStreamUrl:"",
    isConnected:   false,   // connected to a remote encoder
    isDiscovering: false,
    hasPlayed:     false,   // we've attached the HLS player at least once
    events:        [],      // events fetched from the remote encoder
    allEvents:     [],      // events + local uploads, deduplicated
    cachedEvents:  {},      // map<event_id, {cached, active, segments, size_bytes}>
    activeCacheEventId: null,
    usingCache:    false,
    connectionOK:  true,
    error:         "",

    // -------- Lock UI --------
    locked: false,

    // -------- Hardware output (mpv) --------
    hardwareActive: false,
    outputMode:     "display",
    fullscreen:     true,
    decklinkFormat: "720p59.94",
    screenIndex:    1,
    // Playback profile picker. Mirrors the encoder's quality preset
    // picker — fetched once from /api/playback-profiles on init,
    // operator's last choice persisted in localStorage.
    playbackProfile:  "auto",
    playbackProfiles: [
      { id: "auto", label: "Auto", description: "Use mpv defaults." },
    ],
    capabilities: {
      displays: [{ index: 0, name: "Primary Display", resolution: "", is_primary: true }],
      decklink: { available: false, formats: [], devices: [] },
    },
    isRefreshingCapabilities: false,

    // -------- Player state --------
    player:       null,     // Hls instance (or null on Safari native)
    isPlaying:    false,
    isReady:      false,
    isLive:       false,
    currentTime:  0,
    duration:     0,
    bufferLen:    0,
    resolution:   "—",
    seeking:      false,
    gotoTime:     "",
    browserMuted: true,
    mpvMuted:     true,

    // -------- Local uploads --------
    uploadPresets: UPLOAD_PRESETS,
    uploadQuality: "standard",
    uploadStatus:  "",
    uploadProgress:0,
    uploadEventId: "",
    isUploading:   false,
    localUploads:  [],

    // -------- Cache progress --------
    cacheStatus: { cached: false, active: false, segments: 0, size_bytes: 0 },
    totalSegments: 0,
    cacheProgress: 0,

    // -------- Bookmarks --------
    bookmarks: [],
    newBookmarkName: "",

    // -------- UI internals --------
    toast: "",
    _toastTimer:    null,
    _statsInterval: null,
    _cachePoll:     null,
    _hwSyncPoll:    null,
    _localUploadsPoll: null,
    _lastSeekTime:  0,
    _seekTimeout:   null,
    _lastBookmarkPoll: 0,

    // ============================================================
    // INIT
    // ============================================================
    async init() {
      this.restoreLocalState();

      // If we have a stream URL, restore activeCacheEventId so cache
      // progress polling resumes after a refresh.
      if (this.streamUrl && this.streamUrl.includes("/cache/")) {
        const m = this.streamUrl.match(/\/cache\/([^/]+)\/master\.m3u8/);
        if (m) {
          this.activeCacheEventId = m[1];
          this.usingCache = true;
        }
      }

      // Persistence watchers.
      this.$watch("encoderUrl",     (v) => localStorage.setItem("oc_dec_encoder_url", v || ""));
      this.$watch("savedStreamUrl", (v) => localStorage.setItem("oc_dec_stream_url",  v || ""));
      this.$watch("outputMode",     (v) => localStorage.setItem("oc_dec_output_mode", v || "display"));
      this.$watch("fullscreen",     (v) => localStorage.setItem("oc_dec_fullscreen", v ? "1" : "0"));
      this.$watch("decklinkFormat", (v) => localStorage.setItem("oc_dec_decklink",   v || ""));
      this.$watch("screenIndex",    (v) => localStorage.setItem("oc_dec_screen_idx", String(v ?? 1)));
      this.$watch("browserMuted",   (v) => localStorage.setItem("oc_dec_browser_muted", v ? "1" : "0"));
      this.$watch("mpvMuted",       (v) => localStorage.setItem("oc_dec_mpv_muted",     v ? "1" : "0"));
      this.$watch("locked",         (v) => localStorage.setItem("oc_dec_locked",        v ? "1" : "0"));
      this.$watch("uploadQuality",  (v) => localStorage.setItem("oc_dec_upload_quality", v || "standard"));
      this.$watch("playbackProfile",(v) => localStorage.setItem("oc_dec_playback_profile", v || "auto"));

      // Reflect lock state on <body> so the framing CSS knows.
      this.$watch("locked", (v) => document.body.classList.toggle("is-locked", !!v));
      document.body.classList.toggle("is-locked", !!this.locked);

      // Pre-fill encoder URL from server config if we don't have a local one.
      try {
        const cfg = await fetch("/api/config").then(r => r.ok ? r.json() : null);
        if (cfg?.DEFAULT_ENCODER_URL && !this.encoderUrl) {
          this.encoderUrl = cfg.DEFAULT_ENCODER_URL;
        }
      } catch (_) {}

      await Promise.all([
        this.checkOutputStatus(),
        this.refreshOutputCapabilities(),
        this.loadCachedEventList(),
        this.loadPlaybackProfiles(),
      ]);

      // Auto-connect to the encoder if we have a URL; otherwise just
      // populate the local-uploads list so the operator can pick one.
      if (this.encoderUrl) {
        await this.discoverEvents();
      } else {
        await this.loadLocalUploads();
      }

      // Poll local uploads cheaply — completed uploads might appear from
      // a background transcode that started before this page loaded.
      this._localUploadsPoll = setInterval(() => this.loadLocalUploads(), 5000);

      // Resume an in-flight cache if we found one above.
      if (this.activeCacheEventId) this.pollCacheStatus();

      // Auto-reconnect to the last-played stream (if it still exists).
      if (this.savedStreamUrl) {
        const stillValid = await this.validateSavedStream();
        if (stillValid) {
          this.streamUrl = this.savedStreamUrl;
          this.connectStream();
        }
      }

      // Frame-by-frame keyboard shortcuts. Skip when typing in an input
      // or when the UI is locked.
      document.addEventListener("keydown", (e) => {
        if (e.target?.tagName === "INPUT" || e.target?.tagName === "TEXTAREA") return;
        if (this.locked) return;
        if (e.key === "ArrowLeft")  { e.preventDefault(); this.frameStep(-1); }
        if (e.key === "ArrowRight") { e.preventDefault(); this.frameStep(1); }
        if (e.key === " ")          { e.preventDefault(); this.togglePlay(); }
      });

      // Reload warning when locked or playing live.
      window.addEventListener("beforeunload", (e) => {
        if (this.locked) {
          e.preventDefault();
          e.returnValue = "Controls are locked. Are you sure you want to leave?";
        }
      });
    },

    restoreLocalState() {
      this.encoderUrl     = localStorage.getItem("oc_dec_encoder_url")  || "";
      this.savedStreamUrl = localStorage.getItem("oc_dec_stream_url")   || "";
      this.streamUrl      = this.savedStreamUrl;
      this.outputMode     = localStorage.getItem("oc_dec_output_mode")  || "display";
      this.fullscreen     = localStorage.getItem("oc_dec_fullscreen")   !== "0";
      this.decklinkFormat = localStorage.getItem("oc_dec_decklink")     || "720p59.94";
      this.screenIndex    = parseInt(localStorage.getItem("oc_dec_screen_idx") || "1", 10);
      this.browserMuted   = localStorage.getItem("oc_dec_browser_muted") !== "0";
      this.mpvMuted       = localStorage.getItem("oc_dec_mpv_muted")     !== "0";
      this.locked         = localStorage.getItem("oc_dec_locked") === "1";
      this.uploadQuality  = localStorage.getItem("oc_dec_upload_quality") || "standard";
      this.playbackProfile = localStorage.getItem("oc_dec_playback_profile") || "auto";
    },

    // ============================================================
    // PLAYBACK PROFILES (mpv --video-sync / --interpolation / etc.)
    // ============================================================
    // The catalog comes from /api/playback-profiles. We fall back to a
    // single hardcoded "auto" entry if the fetch fails, so the picker
    // is always rendered.
    async loadPlaybackProfiles() {
      try {
        const data = await this.api("/api/playback-profiles").catch(() => null);
        if (!data || !Array.isArray(data.profiles)) return;
        this.playbackProfiles = data.profiles;
        // Honor the server-reported default only if the operator
        // hasn't already chosen a profile locally. localStorage wins
        // because a daemon restart shouldn't clobber an operator's
        // session-level choice.
        const stored = localStorage.getItem("oc_dec_playback_profile");
        if (!stored && data.default) {
          this.playbackProfile = data.default;
        }
        // If the operator's stored profile no longer exists in the
        // catalog (server upgrade dropped it), reset to default.
        if (!this.playbackProfiles.find(p => p.id === this.playbackProfile)) {
          this.playbackProfile = data.default || "auto";
        }
      } catch (_) {}
    },

    // playbackProfileDescription returns the description string for
    // the currently-selected profile. Used as the subline under the
    // picker so the operator sees the rationale without hovering.
    playbackProfileDescription() {
      const p = this.playbackProfiles.find(p => p.id === this.playbackProfile);
      return p?.description || "";
    },

    // ============================================================
    // API
    // ============================================================
    async api(path, { method = "GET", body, raw = false } = {}) {
      const headers = {};
      const opts = { method, headers };
      if (body !== undefined) { headers["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
      const r = await fetch(path, opts);
      if (raw) return r;
      const text = await r.text();
      let data = null;
      if (text) { try { data = JSON.parse(text); } catch (_) { data = { detail: text }; } }
      if (!r.ok) throw new Error((data && (data.detail || data.error)) || `HTTP ${r.status}`);
      return data;
    },

    // ============================================================
    // ENCODER DISCOVERY + CACHE MANAGEMENT
    // ============================================================
    async discoverEvents() {
      if (!this.encoderUrl) { this.error = "Enter the encoder address first."; return; }

      let url = this.encoderUrl.trim();
      if (!/^https?:\/\//i.test(url)) url = "http://" + url;
      url = url.replace(/\/$/, "");
      this.encoderUrl = url;

      this.isDiscovering = true;
      this.error = "";
      this.events = [];

      try {
        const r = await fetch(`${url}/api/events`);
        if (!r.ok) throw new Error(`Connection failed (${r.status})`);
        this.events = await r.json();
        this.isConnected = true;
        this.connectionOK = true;

        await this.loadCachedEventList();
        await this.reconcileCache();
      } catch (e) {
        this.error = `Connection failed: ${e.message}`;
        this.isConnected = false;
        this.connectionOK = false;
      } finally {
        await this.loadLocalUploads();
        this.isDiscovering = false;
      }
    },

    // After a successful encoder fetch, remove any local cache for events
    // the encoder no longer has — but preserve local uploads (which only
    // live on the decoder and never appear in the encoder's event list).
    async reconcileCache() {
      const localUploadIds = new Set();
      try {
        const ups = await this.api("/api/cache/uploads").catch(() => []);
        (ups || []).forEach(u => localUploadIds.add(u.event_id));
      } catch (_) {}

      const encoderIds = new Set(this.events.map(e => e.id));
      const stale = Object.keys(this.cachedEvents)
        .filter(id => !encoderIds.has(id) && !localUploadIds.has(id));

      for (const id of stale) {
        delete this.cachedEvents[id];
        try { await this.api(`/api/cache/${id}`, { method: "DELETE" }); } catch (_) {}
      }

      // If the saved stream points to a deleted event, drop it.
      if (this.savedStreamUrl) {
        const m = this.savedStreamUrl.match(/\/events\/(\d{8}_\d{6}|\d{8}-\d{6})\//);
        if (m && !encoderIds.has(m[1])) {
          this.savedStreamUrl = "";
          this.streamUrl = "";
        }
      }
    },

    async loadCachedEventList() {
      try {
        const list = await this.api("/api/cache").catch(() => []);
        const next = {};
        for (const item of (list || [])) {
          next[item.event_id] = {
            cached: true,
            active: !item.is_complete,
            segments: item.segments,
            size_bytes: item.size_bytes,
          };
        }
        this.cachedEvents = next;
      } catch (_) {
        this.cachedEvents = {};
      }
    },

    async loadLocalUploads() {
      try {
        const uploads = await this.api("/api/cache/uploads").catch(() => []);
        this.localUploads = uploads || [];

        // Build a deduplicated list of remote events + completed local
        // uploads. Local uploads disguise themselves as type=local_upload
        // so the UI can show a distinct icon and skip remote-only actions.
        const map = new Map();
        for (const ev of this.events) {
          if (ev.type !== "local_upload") map.set(ev.id, ev);
        }
        for (const u of (uploads || [])) {
          if (u.status === "completed") {
            map.set(u.event_id, {
              id:         u.event_id,
              name:       u.name,
              type:       "local_upload",
              status:     "completed",
              created_at: u.created_at,
              quality:    u.quality,
            });
          }
        }
        this.allEvents = Array.from(map.values()).sort((a, b) => {
          const at = new Date(a.created_at || 0).getTime();
          const bt = new Date(b.created_at || 0).getTime();
          return bt - at;
        });
      } catch (_) {}
    },

    async validateSavedStream() {
      if (!this.savedStreamUrl) return false;
      if (!this.savedStreamUrl.includes("/cache/")) return true;
      const m = this.savedStreamUrl.match(/\/cache\/([^/]+)\/master\.m3u8/);
      if (!m) return false;
      const id = m[1];
      // Cached event still around?
      return !!this.cachedEvents[id] || this.localUploads.some(u => u.event_id === id);
    },

    async selectEvent(ev) {
      if (this.locked) return;

      // Local uploads play directly from /cache.
      if (ev.type === "local_upload") {
        const url = `${window.location.origin}/cache/${ev.id}/master.m3u8`;
        this.activeCacheEventId = ev.id;
        this.usingCache = true;
        this.streamUrl = url;
        this.savedStreamUrl = url;
        this.error = "";
        await this.maybeUpdateHardware(url);
        this.connectStream();
        this.loadBookmarks();
        return;
      }

      // Remote events: start a local cache, then point the player at /cache.
      let base = this.encoderUrl.trim().replace(/\/$/, "");
      base = base.replace(/\/events\/[^/]+$/, "");
      this.error = "Starting local cache…";
      try {
        await this.api("/api/cache/start", { method: "POST", body: { event_id: ev.id, remote_url: base } });
        this.activeCacheEventId = ev.id;
        this.usingCache = true;
        this.pollCacheStatus();
        // Give the cache a beat to fill the first few segments.
        await new Promise(r => setTimeout(r, 1000));

        const local = `${window.location.origin}/cache/${ev.id}/master.m3u8`;
        this.streamUrl = local;
        this.savedStreamUrl = local;
        this.error = "";

        await this.maybeUpdateHardware(local);
        this.connectStream();
        this.loadBookmarks();
      } catch (e) {
        this.error = `Cache failed: ${e.message}`;
      }
    },

    async deleteCache(ev) {
      const label = ev.type === "local_upload" ? "local upload" : "local cache";
      if (!confirm(`Delete ${label} for "${ev.name}"?`)) return;
      try {
        await this.api(`/api/cache/${ev.id}`, { method: "DELETE" });
        delete this.cachedEvents[ev.id];
        if (ev.type === "local_upload") {
          this.allEvents = this.allEvents.filter(e => e.id !== ev.id);
          this.localUploads = this.localUploads.filter(u => u.event_id !== ev.id);
        }
        if (this.activeCacheEventId === ev.id) {
          this.activeCacheEventId = null;
          this.streamUrl = "";
          this.savedStreamUrl = "";
        }
        this.showToast("Cache cleared", "good");
      } catch (e) {
        this.showToast(e.message, "bad");
      }
    },

    pollCacheStatus() {
      if (this._cachePoll) clearInterval(this._cachePoll);
      if (!this.activeCacheEventId) return;
      this.totalSegments = 0;
      this.cacheProgress = 0;
      this.fetchTotalSegments();
      this._cachePoll = setInterval(async () => {
        if (!this.activeCacheEventId) return;
        try {
          this.fetchTotalSegments();
          const data = await this.api(`/api/cache/${this.activeCacheEventId}/status`).catch(() => null);
          if (data) {
            this.cacheStatus = data;
            this.updateCacheProgress();
          }
        } catch (_) {}
      }, 2000);
    },

    async fetchTotalSegments() {
      if (!this.activeCacheEventId || !this.encoderUrl) return;
      const ev = this.allEvents.find(e => e.id === this.activeCacheEventId);
      if (ev && ev.type === "local_upload") return;
      try {
        const base = this.encoderUrl.trim().replace(/\/$/, "");
        const url = `${base}/events/${this.activeCacheEventId}/master.m3u8`;
        const r = await fetch(url);
        if (!r.ok) return;
        const text = await r.text();
        let count = 0;
        for (const line of text.split("\n")) {
          const l = line.trim();
          if (l && !l.startsWith("#")) count++;
        }
        this.totalSegments = count;
        this.updateCacheProgress();
      } catch (_) {}
    },

    updateCacheProgress() {
      if (this.totalSegments > 0) {
        const p = ((this.cacheStatus.segments || 0) / this.totalSegments) * 100;
        this.cacheProgress = Math.min(100, Math.round(p));
      } else {
        this.cacheProgress = 0;
      }
    },

    // ============================================================
    // PLAYER
    // ============================================================
    connectStream() {
      if (!this.streamUrl) return;
      this.error = "";
      this.isReady = false;
      this.isLive = false;

      const video = this.$refs.videoPlayer;
      if (!video) return;
      video.muted = this.browserMuted;

      // Tear down any prior player.
      if (this.player) { try { this.player.destroy(); } catch (_) {} this.player = null; }

      // Wire <video> events.
      video.onplay  = () => { this.isPlaying = true;  this.syncHardware("play"); };
      video.onpause = () => { this.isPlaying = false; this.syncHardware("pause"); };
      video.onseeked = () => { this.currentTime = video.currentTime; };
      video.ontimeupdate = () => { if (!this.seeking) this.currentTime = video.currentTime; };
      video.onloadedmetadata = () => {
        const d = video.duration;
        if (isFinite(d) && d > 0 && d < 315360000) this.duration = d;
      };
      video.ondurationchange = () => {
        const d = video.duration;
        if (isFinite(d) && d > 0 && d < 315360000) this.duration = d;
      };
      video.oncanplay = () => { this.isReady = true; };

      if (window.Hls && Hls.isSupported()) {
        const hls = new Hls({ debug: false, enableWorker: true, lowLatencyMode: true });
        hls.loadSource(this.streamUrl);
        hls.attachMedia(video);
        hls.on(Hls.Events.MANIFEST_PARSED, (_e, data) => {
          this.isConnected = true;
          this.hasPlayed = true;
          const lvl = data.levels?.[0]?.details;
          this.isLive = lvl ? !!lvl.live : false;
          this.duration = isFinite(video.duration) && video.duration > 0 ? video.duration : 0;
          if (this.isLive) {
            const liveEdge = video.seekable.length > 0 ? video.seekable.end(0) : this.duration;
            video.currentTime = Math.max(0, liveEdge - 5);
          } else {
            video.currentTime = 0;
          }
          this.currentTime = video.currentTime;
          video.pause();
          this.isPlaying = false;
          if (this.hardwareActive) {
            setTimeout(() => { this.syncHardware("seek"); this.syncHardware("pause"); }, 500);
          }
        });
        hls.on(Hls.Events.ERROR, (_e, data) => {
          if (!data.fatal) return;
          if (data.type === Hls.ErrorTypes.NETWORK_ERROR)        hls.startLoad();
          else if (data.type === Hls.ErrorTypes.MEDIA_ERROR)     hls.recoverMediaError();
          else { try { hls.destroy(); } catch (_) {} this.player = null; this.error = `Playback error: ${data.type}`; }
        });
        this.player = hls;
      } else if (video.canPlayType("application/vnd.apple.mpegurl")) {
        video.src = this.streamUrl;
        video.addEventListener("loadedmetadata", () => {
          this.isConnected = true;
          this.hasPlayed = true;
        }, { once: true });
      } else {
        this.error = "This browser does not support HLS playback.";
      }

      this.startStats();
    },

    async connectManualStream() {
      this.savedStreamUrl = this.streamUrl;
      this.usingCache = this.streamUrl.includes(window.location.origin);
      await this.maybeUpdateHardware(this.streamUrl);
      this.connectStream();
    },

    startStats() {
      if (this._statsInterval) clearInterval(this._statsInterval);
      this._statsInterval = setInterval(() => {
        const v = this.$refs.videoPlayer;
        if (!v) return;
        if (v.buffered.length > 0) this.bufferLen = v.buffered.end(v.buffered.length - 1) - v.currentTime;
        this.resolution = v.videoWidth ? `${v.videoWidth}×${v.videoHeight}` : "—";
        if (this.isLive && this.isConnected) {
          const now = Date.now();
          if (!this._lastBookmarkPoll || now - this._lastBookmarkPoll > 5000) {
            this._lastBookmarkPoll = now;
            this.loadBookmarks();
          }
        }
      }, 1000);
    },

    togglePlay() {
      const v = this.$refs.videoPlayer;
      if (!v || !this.isReady || this.locked) return;
      if (v.paused) v.play().catch(() => {}); else v.pause();
    },

    // "Play and lock" — start playback and lock controls so a bumped
    // trackpad can't rewind during a service.
    playAndLock() {
      const v = this.$refs.videoPlayer;
      if (!v || !this.isReady) return;
      if (v.paused) v.play().catch(() => {});
      this.locked = true;
    },

    jump(seconds) {
      this.seekTo(this.currentTime + seconds);
    },

    frameStep(direction) {
      // 30 fps is the safe assumption for HLS segments; if we wired
      // backend FPS through to the event payload we'd use it here.
      this.seekTo(this.currentTime + (direction / 30));
    },

    gotoTimeNow() {
      const t = F.parseTime(this.gotoTime);
      if (t === null) return;
      this.seekTo(t);
    },

    seekTo(time) {
      const v = this.$refs.videoPlayer;
      if (!v || this.locked) return;
      if (this.isLive && v.seekable.length > 0) {
        time = Math.max(v.seekable.start(0), Math.min(time, v.seekable.end(0)));
      } else {
        time = Math.max(0, Math.min(time, this.duration || time));
      }
      v.currentTime = time;
      this.currentTime = v.currentTime;
      this.seeking = false;
      this._lastSeekTime = Date.now();
      if (this.hardwareActive) this._sendSync("seek", time).catch(() => {});
    },

    onScrubInput(e) {
      this.seeking = true;
      this.currentTime = parseFloat(e.target.value);
    },

    onScrubChange(e) {
      this.seekTo(parseFloat(e.target.value));
    },

    scrubFill() {
      if (!this.duration) return "0%";
      return `${Math.max(0, Math.min(100, (this.currentTime / this.duration) * 100)).toFixed(2)}%`;
    },

    bookmarkLeft(b) {
      if (!this.duration) return "0%";
      const t = (b.time_pos != null) ? b.time_pos : ((b.frame || 0) / (b.fps || 30));
      return `${Math.max(0, Math.min(100, (t / this.duration) * 100)).toFixed(2)}%`;
    },

    toggleBrowserMute() {
      this.browserMuted = !this.browserMuted;
      const v = this.$refs.videoPlayer;
      if (v) v.muted = this.browserMuted;
    },

    async toggleMPVMute() {
      this.mpvMuted = !this.mpvMuted;
      try {
        await this.api("/api/output/mute", { method: "POST", body: { muted: this.mpvMuted } });
      } catch (_) {}
    },

    // ============================================================
    // HARDWARE OUTPUT (mpv)
    // ============================================================
    async checkOutputStatus() {
      try {
        const data = await this.api("/api/output/status").catch(() => null);
        if (data) this.hardwareActive = !!data.is_running;
      } catch (_) {}
    },

    async refreshOutputCapabilities() {
      this.isRefreshingCapabilities = true;
      try {
        const data = await this.api("/api/output/capabilities").catch(() => null);
        if (data) {
          this.capabilities = data;
          if (this.outputMode === "display" && this.capabilities.displays.length > 0) {
            const valid = this.capabilities.displays.map(d => d.index);
            if (!valid.includes(this.screenIndex)) this.screenIndex = this.capabilities.displays[0].index;
          }
          if (this.outputMode === "decklink" && this.capabilities.decklink.available &&
              this.capabilities.decklink.formats.length > 0 &&
              !this.capabilities.decklink.formats.includes(this.decklinkFormat)) {
            this.decklinkFormat = this.capabilities.decklink.formats[0];
          }
        }
      } finally {
        this.isRefreshingCapabilities = false;
      }
    },

    onOutputModeChange() {
      if (this.outputMode === "decklink" && !this.capabilities.decklink.available) {
        this.outputMode = "display";
        return;
      }
      if (this.outputMode === "display" && this.capabilities.displays.length > 0) {
        this.screenIndex = this.capabilities.displays[0].index;
      } else if (this.outputMode === "decklink" && this.capabilities.decklink.formats.length > 0) {
        this.decklinkFormat = this.capabilities.decklink.formats[0];
      }
    },

    formatDisplayOption(d) {
      let out = `${d.index}: ${d.name}`;
      if (d.resolution) out += ` (${d.resolution})`;
      if (d.is_primary) out += " · Primary";
      return out;
    },

    async toggleHardware() {
      if (this.locked) return;
      if (this.hardwareActive) {
        try { await this.api("/api/output/stop", { method: "POST" }); } catch (_) {}
        this.hardwareActive = false;
        if (this._hwSyncPoll) { clearInterval(this._hwSyncPoll); this._hwSyncPoll = null; }
        this.showToast("Projector stopped", "good");
        return;
      }
      if (!this.streamUrl) { this.showToast("Pick an event first.", "bad"); return; }
      try {
        await this.api("/api/output/start", {
          method: "POST",
          body: {
            url: this.streamUrl,
            screen_index: this.screenIndex,
            output_mode: this.outputMode,
            fullscreen: this.fullscreen,
            decklink_format: this.decklinkFormat,
            playback_profile: this.playbackProfile,
          },
        });
        this.hardwareActive = true;
        this.showToast("Projector starting…", "good");
        // Give mpv ~1s to come up, then push the browser's current
        // position so they start in sync.
        setTimeout(async () => {
          const v = this.$refs.videoPlayer;
          if (v && v.currentTime > 0) {
            await this._sendSync("seek", v.currentTime).catch(() => {});
            if (v.paused) await this._sendSync("pause", v.currentTime).catch(() => {});
          }
          this.startHardwareSync();
        }, 1000);
      } catch (e) {
        this.error = `Hardware output failed: ${e.message}`;
        this.showToast(this.error, "bad");
      }
    },

    async maybeUpdateHardware(url) {
      if (!this.hardwareActive) return;
      try {
        await this.api("/api/output/update", { method: "POST", body: { url } });
      } catch (_) {}
    },

    // Sync helpers. seek is debounced (100 ms) so a continuous scrub
    // doesn't flood mpv with intermediate frames. play/pause go through
    // immediately.
    syncHardware(command) {
      if (!this.hardwareActive) return;
      const ts = this.currentTime;
      if (command === "seek") {
        if (this._seekTimeout) clearTimeout(this._seekTimeout);
        this._seekTimeout = setTimeout(() => this._sendSync("seek", ts).catch(() => {}), 100);
      } else {
        this._sendSync(command, ts).catch(() => {});
      }
    },

    async _sendSync(command, timestamp) {
      await this.api("/api/output/sync", { method: "POST", body: { command, timestamp } });
    },

    // Loose alignment poll. Browser is authoritative; if mpv has drifted
    // > 3 s we push the browser's position to mpv. play/pause state is
    // forced to mirror the browser too.
    startHardwareSync() {
      if (this._hwSyncPoll) clearInterval(this._hwSyncPoll);
      this._hwSyncPoll = setInterval(() => {
        if (this.hardwareActive && this.isPlaying && !this.seeking) this.syncFromHardware();
      }, 2000);
    },

    async syncFromHardware() {
      if (!this.hardwareActive || this.seeking) return;
      if (this._lastSeekTime && Date.now() - this._lastSeekTime < 5000) return;
      try {
        const data = await this.api("/api/output/frame-position").catch(() => null);
        if (!data) return;
        const v = this.$refs.videoPlayer;
        if (v && data.time_pos != null) {
          const drift = Math.abs(v.currentTime - data.time_pos);
          if (drift > 3.0) {
            this._lastSeekTime = Date.now();
            await this._sendSync("seek", v.currentTime).catch(() => {});
          }
          if (data.paused && !v.paused) await this._sendSync("play",  v.currentTime).catch(() => {});
          if (!data.paused && v.paused)  await this._sendSync("pause", v.currentTime).catch(() => {});
        }
      } catch (_) {}
    },

    // ============================================================
    // BOOKMARKS
    // ============================================================
    async loadBookmarks() {
      if (!this.activeCacheEventId) { this.bookmarks = []; return; }
      const decoderKey = `oc_dec_bookmarks_${this.activeCacheEventId}`;
      const decoderBookmarks = JSON.parse(localStorage.getItem(decoderKey) || "[]");
      let encoderBookmarks = [];
      try {
        const data = await this.api(`/api/events/${this.activeCacheEventId}/bookmarks`).catch(() => null);
        encoderBookmarks = data?.bookmarks || [];
      } catch (_) {}
      this.bookmarks = [...decoderBookmarks, ...encoderBookmarks].sort((a, b) => (a.frame || 0) - (b.frame || 0));
    },

    async createBookmark() {
      const v = this.$refs.videoPlayer;
      if (!v || !this.activeCacheEventId) return;
      const fps = 30;
      const frame = Math.round(v.currentTime * fps);
      const bm = {
        id: `bm_dec_${Date.now()}`,
        name: this.newBookmarkName.trim() || `Frame ${frame}`,
        frame, fps,
        time_pos: v.currentTime,
        created_at: new Date().toISOString(),
        source: "decoder",
        color: "orange",
      };
      const key = `oc_dec_bookmarks_${this.activeCacheEventId}`;
      const existing = JSON.parse(localStorage.getItem(key) || "[]");
      existing.push(bm);
      localStorage.setItem(key, JSON.stringify(existing));
      this.newBookmarkName = "";
      this.loadBookmarks();
    },

    deleteBookmark(bm) {
      if (bm.source === "encoder") {
        this.showToast("Encoder bookmarks can only be removed from the encoder.", "bad");
        return;
      }
      const key = `oc_dec_bookmarks_${this.activeCacheEventId}`;
      const existing = JSON.parse(localStorage.getItem(key) || "[]");
      localStorage.setItem(key, JSON.stringify(existing.filter(b => b.id !== bm.id)));
      this.loadBookmarks();
    },

    async jumpToBookmark(bm) {
      // Frame-precise: seek both browser and mpv to the exact frame.
      let t = bm.time_pos != null ? bm.time_pos : (bm.frame || 0) / (bm.fps || 30);
      const v = this.$refs.videoPlayer;
      if (this.isLive && v && v.seekable.length > 0) {
        t = Math.max(v.seekable.start(0), Math.min(t, v.seekable.end(0)));
      }
      if (v) { v.currentTime = t; this.currentTime = t; }
      this._lastSeekTime = Date.now();
      if (this.hardwareActive) {
        try { await this.api("/api/output/sync", { method: "POST", body: { command: "seek", timestamp: t } }); } catch (_) {}
      }
    },

    // ============================================================
    // UPLOAD
    // ============================================================
    pickFile()    { if (!this.locked) this.$refs.fileInput?.click(); },

    async onFileChosen(e) {
      const file = e.target.files?.[0];
      if (!file) return;
      this.isUploading = true;
      this.uploadStatus = "Uploading…";
      this.uploadProgress = 0;

      const fd = new FormData();
      fd.append("file", file);
      fd.append("quality", this.uploadQuality);

      try {
        const r = await fetch("/api/cache/upload", { method: "POST", body: fd });
        if (!r.ok) throw new Error(await r.text() || `HTTP ${r.status}`);
        const data = await r.json();
        this.uploadEventId = data.event_id;
        this.uploadStatus = "Processing…";
        this.pollCacheUploadStatus(data.event_id);
      } catch (err) {
        this.uploadStatus = "Failed";
        this.isUploading = false;
        this.showToast(`Upload failed: ${err.message}`, "bad");
      } finally {
        e.target.value = "";
      }
    },

    async pollCacheUploadStatus(eventId) {
      const tick = async () => {
        try {
          const data = await this.api(`/api/cache/upload/${eventId}/status`).catch(() => null);
          if (!data) { setTimeout(tick, 2000); return; }
          if (data.status === "completed") {
            this.uploadStatus = "Ready";
            this.uploadProgress = 100;
            this.isUploading = false;
            this.loadLocalUploads();
            setTimeout(() => { this.uploadStatus = ""; this.uploadProgress = 0; }, 3000);
            return;
          }
          if (data.status === "failed") {
            this.uploadStatus = "Processing failed";
            this.isUploading = false;
            this.showToast("Video processing failed", "bad");
            return;
          }
          this.uploadStatus = "Processing video…";
          this.uploadProgress = Math.min(data.progress || 0, 95);
          setTimeout(tick, 2000);
        } catch (_) { setTimeout(tick, 2000); }
      };
      tick();
    },

    async deleteLocalUpload(u) {
      if (!confirm(`Delete "${u.name}"?`)) return;
      try {
        await this.api(`/api/cache/${u.event_id}`, { method: "DELETE" });
        this.localUploads = this.localUploads.filter(x => x.event_id !== u.event_id);
        this.allEvents = this.allEvents.filter(e => e.id !== u.event_id);
        if (this.streamUrl.includes(u.event_id)) {
          this.streamUrl = "";
          this.savedStreamUrl = "";
        }
        this.showToast("Upload deleted", "good");
      } catch (e) {
        this.showToast(e.message, "bad");
      }
    },

    // ============================================================
    // LOCK / UTIL
    // ============================================================
    toggleLock() { this.locked = !this.locked; },

    eventIcon(ev) {
      if (ev.status === "running")    return "recording";
      if (ev.type === "upload")       return "upload";
      if (ev.type === "local_upload") return "local";
      return "standard";
    },

    eventCacheBadge(ev) {
      const c = this.cachedEvents[ev.id];
      if (!c) return null;
      if (c.active) return { label: "Caching", kind: "info" };
      if (c.cached) return { label: "Cached",  kind: "good" };
      return null;
    },

    showToast(message, kind = "") {
      this.toast = message;
      this._lastToastKind = kind;
      clearTimeout(this._toastTimer);
      this._toastTimer = setTimeout(() => { this.toast = ""; }, 3500);
    },
    get toastKind() { return this._lastToastKind || ""; },

    // Formatters
    fmtTime:  (s) => F.formatTime(s),
    fmtBytes: (b) => F.formatBytes(b),
    fmtDate:  (d) => F.formatDate(d),
  }));
});
