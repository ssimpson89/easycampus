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

// Upload presets use the same internal/quality tier IDs as the encoder
// (/api/cache/upload resolves them through the same catalog). The list
// is replaced by /api/quality at init; this fallback keeps the picker
// usable if that fetch fails.
const UPLOAD_FALLBACK_PRESETS = [
  { id: "consumer-720p30",  label: "Consumer 720p30 · 1280x720 · 3.5 Mbps · 30 fps" },
  { id: "consumer-1080p30", label: "Consumer 1080p30 · 1920x1080 · 6 Mbps · 30 fps" },
];
const DEFAULT_UPLOAD_PRESET = "consumer-1080p30";
// Legacy low/standard/high/ultra values from older releases, migrated
// on restore the same way the encoder migrates its form state.
const LEGACY_UPLOAD_PRESETS = {
  low:      "consumer-720p30",
  standard: "consumer-1080p30",
  medium:   "consumer-1080p30",
  high:     "consumer-1080p30",
  ultra:    "prosumer-1080p60",
};

// HLS retry policy: exponential backoff, then give up with an error.
const HLS_MAX_RETRIES = 6;
// Upload status polls stop after this many consecutive 404s.
const UPLOAD_404_LIMIT = 5;
// How long selectEvent waits for the first cached segment.
const CACHE_READY_TIMEOUT_MS = 30000;
// Remote event list refresh cadence while connected.
const EVENTS_REFRESH_MS = 12000;
// Projector status poll cadence.
const OUTPUT_STATUS_MS = 5000;

const sleep = (ms) => new Promise(r => setTimeout(r, ms));

document.addEventListener("alpine:init", () => {
  Alpine.data("decoder", () => ({

    // -------- Connection / events --------
    encoderUrl:    "",
    streamUrl:     "",
    savedStreamUrl:"",
    isConnected:   false,   // connected to a remote encoder
    isDiscovering: false,
    nodeType:      "",      // this server's NODE_TYPE (topbar)
    remoteIsDecoder: false, // encoder address actually points at a decoder
    connectHint:   "",      // actionable hint after a failed connect
    staleCaches:   [],      // cached event IDs the encoder no longer lists
    isClearingStale: false,
    statusLine:    "",      // neutral progress text (not an error)
    selectingEventId: null, // event selection in flight
    isTogglingHardware: false,
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
    atLiveEdge:   false,    // within ~2 segment durations of the live edge
    currentTime:  0,
    duration:     0,
    bufferLen:    0,
    resolution:   "—",
    seeking:      false,
    gotoTime:     "",
    browserMuted: true,
    mpvMuted:     true,

    // -------- Local uploads --------
    uploadPresets: UPLOAD_FALLBACK_PRESETS,
    uploadQuality: DEFAULT_UPLOAD_PRESET,
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
    _eventsPoll:    null,
    _outputStatusPoll: null,
    _netRetries:    0,
    _netRetryTimer: null,
    _selectToken:   0,
    // play/pause events fired while we adopt mpv's state on load must
    // not be echoed back to mpv.
    _suppressHwUntil: 0,

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
      this.$watch("uploadQuality",  (v) => localStorage.setItem("oc_dec_upload_quality", v || DEFAULT_UPLOAD_PRESET));
      this.$watch("playbackProfile",(v) => localStorage.setItem("oc_dec_playback_profile", v || "auto"));

      // Reflect lock state on <body> so the framing CSS knows.
      this.$watch("locked", (v) => document.body.classList.toggle("is-locked", !!v));
      document.body.classList.toggle("is-locked", !!this.locked);

      // Pre-fill encoder URL from server config if we don't have a local one.
      try {
        const cfg = await fetch("/api/config").then(r => r.ok ? r.json() : null);
        this.nodeType = cfg?.NODE_TYPE || "";
        if (cfg?.DEFAULT_ENCODER_URL && !this.encoderUrl) {
          this.encoderUrl = cfg.DEFAULT_ENCODER_URL;
        }
      } catch (_) {}

      await Promise.all([
        this.checkOutputStatus(),
        this.refreshOutputCapabilities(),
        this.loadCachedEventList(),
        this.loadPlaybackProfiles(),
        this.loadUploadPresets(),
      ]);

      // Keep the projector state honest: mpv can exit (or be started
      // from another tab) without this page knowing.
      this._outputStatusPoll = setInterval(() => this.checkOutputStatus(), OUTPUT_STATUS_MS);

      // Refresh the remote event list while connected so new events
      // (and finished live ones) show up without a manual Refresh.
      this._eventsPoll = setInterval(() => {
        if (this.isConnected && !this.isDiscovering) this.refreshRemoteEvents();
      }, EVENTS_REFRESH_MS);

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
      // This is NOT a user action: if the projector is already running
      // we adopt its position instead of pushing ours to it, so a page
      // reload mid-service never pauses or seeks the projector.
      if (this.savedStreamUrl) {
        const stillValid = await this.validateSavedStream();
        if (stillValid) {
          this.streamUrl = this.savedStreamUrl;
          this.connectStream({ adoptHardware: true });
        }
      }

      // Frame-by-frame keyboard shortcuts. Skip when focus is on a form
      // control or button (Space/arrows belong to it) or when locked.
      document.addEventListener("keydown", (e) => {
        const t = e.target;
        if (t && (["INPUT", "TEXTAREA", "SELECT", "BUTTON"].includes(t.tagName) || t.isContentEditable)) return;
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
      // 0 is a valid display index; only fall back on missing/garbage.
      const idx = parseInt(localStorage.getItem("oc_dec_screen_idx") ?? "1", 10);
      this.screenIndex    = Number.isInteger(idx) && idx >= 0 ? idx : 1;
      this.browserMuted   = localStorage.getItem("oc_dec_browser_muted") !== "0";
      this.mpvMuted       = localStorage.getItem("oc_dec_mpv_muted")     !== "0";
      this.locked         = localStorage.getItem("oc_dec_locked") === "1";
      const q = localStorage.getItem("oc_dec_upload_quality") || DEFAULT_UPLOAD_PRESET;
      this.uploadQuality  = LEGACY_UPLOAD_PRESETS[q] || q;
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
        this.resyncSelect("profileSelect", this.playbackProfile);
      } catch (_) {}
    },

    // loadUploadPresets pulls the tier catalog from this server's
    // /api/quality (same catalog the encoder uses) and flattens it for
    // the upload picker.
    async loadUploadPresets() {
      try {
        const data = await this.api("/api/quality");
        const flat = [];
        for (const t of (data?.tiers || [])) {
          for (const p of (t.presets || [])) {
            flat.push({ id: p.id, label: `${p.label} · ${F.presetSummary(p)}` });
          }
        }
        if (flat.length === 0) return;
        this.uploadPresets = flat;
        if (!flat.some(p => p.id === this.uploadQuality)) {
          this.uploadQuality = (flat.find(p => p.id === DEFAULT_UPLOAD_PRESET) || flat[0]).id;
        }
        this.resyncSelect("uploadQualitySelect", this.uploadQuality);
      } catch (_) {}
    },

    // resyncSelect re-applies a model value to a <select> after its
    // <option>s arrive asynchronously (x-model ran before x-for did).
    resyncSelect(ref, value) {
      this.$nextTick(() => {
        const el = this.$refs[ref];
        if (el && value != null) el.value = String(value);
      });
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
      if (!r.ok) {
        const err = new Error((data && (data.detail || data.error)) || `HTTP ${r.status}`);
        err.status = r.status;
        throw err;
      }
      return data;
    },

    // encoderBase returns the normalized remote encoder URL (no trailing
    // slash), or "" when none is configured.
    encoderBase() {
      let url = (this.encoderUrl || "").trim();
      if (!url) return "";
      if (!/^https?:\/\//i.test(url)) url = "http://" + url;
      return url.replace(/\/+$/, "").replace(/\/events\/[^/]+$/, "");
    },

    // ============================================================
    // ENCODER DISCOVERY + CACHE MANAGEMENT
    // ============================================================
    async discoverEvents() {
      if (!this.encoderUrl) { this.error = "Enter the encoder address first."; return; }

      const url = this.encoderBase();
      this.encoderUrl = url;

      this.isDiscovering = true;
      this.error = "";
      this.connectHint = "";
      this.remoteIsDecoder = false;
      this.staleCaches = [];
      this.events = [];

      try {
        // Ask the remote what it is. Pointing the decoder at itself (the
        // default when DEFAULT_ENCODER_URL is unset) or at another
        // decoder returns an empty event list, which must not be
        // mistaken for "the encoder deleted everything".
        this.remoteIsDecoder = await this.remoteLooksLikeDecoder(url);

        const r = await fetch(`${url}/api/events`);
        if (!r.ok) throw new Error(`Connection failed (${r.status})`);
        const list = await r.json();
        this.events = Array.isArray(list) ? list : [];
        this.isConnected = true;
        this.connectionOK = true;

        await Promise.all([this.loadCachedEventList(), this.loadLocalUploads()]);
        if (!this.remoteIsDecoder) this.computeStaleCaches();
      } catch (e) {
        this.error = `Connection failed: ${e.message}`;
        if (e instanceof TypeError) {
          // fetch() rejects with TypeError for network failures and
          // CORS rejections alike; the latter is the common LAN case.
          this.connectHint = `Couldn't reach ${url}. If the encoder is running, add ${window.location.origin} to EASYCAMPUS_ALLOWED_ORIGINS on the encoder and restart it.`;
        }
        this.isConnected = false;
        this.connectionOK = false;
      } finally {
        await this.loadLocalUploads();
        this.isDiscovering = false;
      }
    },

    async remoteLooksLikeDecoder(url) {
      if (url === window.location.origin && this.nodeType === "decoder") return true;
      try {
        const r = await fetch(`${url}/api/config`);
        if (!r.ok) return false;
        const cfg = await r.json();
        return cfg?.NODE_TYPE === "decoder";
      } catch (_) {
        return false;
      }
    },

    // refreshRemoteEvents is the quiet background refresh: no spinner,
    // no clearing the list, no error banner on a transient failure.
    async refreshRemoteEvents() {
      const url = this.encoderBase();
      if (!url) return;
      try {
        const r = await fetch(`${url}/api/events`);
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        const list = await r.json();
        this.events = Array.isArray(list) ? list : [];
        this.connectionOK = true;
        await Promise.all([this.loadCachedEventList(), this.loadLocalUploads()]);
        if (!this.remoteIsDecoder) this.computeStaleCaches();
      } catch (_) {
        this.connectionOK = false;
      }
    },

    // computeStaleCaches lists local caches for events the encoder no
    // longer has. Nothing is deleted automatically: the operator reviews
    // the list and clicks "Clear stale caches". Local uploads (decoder
    // only) and the cache currently playing are never listed.
    computeStaleCaches() {
      const localUploadIds = new Set(this.localUploads.map(u => u.event_id));
      const encoderIds = new Set(this.events.map(e => e.id));
      this.staleCaches = Object.keys(this.cachedEvents)
        .filter(id => !encoderIds.has(id) && !localUploadIds.has(id) && id !== this.activeCacheEventId)
        .sort();
    },

    async clearStaleCaches() {
      if (this.locked || this.staleCaches.length === 0) return;
      const n = this.staleCaches.length;
      if (!confirm(`Delete ${n} local cache${n === 1 ? "" : "s"} for events the encoder no longer lists?`)) return;
      this.isClearingStale = true;
      const failed = [];
      for (const id of [...this.staleCaches]) {
        try {
          await this.api(`/api/cache/${encodeURIComponent(id)}`, { method: "DELETE" });
          delete this.cachedEvents[id];
        } catch (_) {
          failed.push(id);
        }
      }
      this.staleCaches = failed;
      this.isClearingStale = false;
      if (failed.length) this.showToast(`Could not clear ${failed.length} cache(s)`, "bad");
      else this.showToast("Stale caches cleared", "good");
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
      if (this.locked || this.selectingEventId) return;
      const token = ++this._selectToken;
      this.selectingEventId = ev.id;
      this.error = "";
      try {
        // Local uploads play directly from /cache.
        if (ev.type === "local_upload") {
          const url = `${window.location.origin}/cache/${ev.id}/master.m3u8`;
          this.activeCacheEventId = ev.id;
          this.usingCache = true;
          this.streamUrl = url;
          this.savedStreamUrl = url;
          await this.maybeUpdateHardware(url);
          this.connectStream({ userInitiated: true });
          this.loadBookmarks();
          return;
        }

        // Remote events: start a local cache, wait for the first
        // segment, then point the player at /cache.
        const base = this.encoderBase();
        this.statusLine = "Starting local cache…";
        await this.api("/api/cache/start", { method: "POST", body: { event_id: ev.id, remote_url: base } });
        this.activeCacheEventId = ev.id;
        this.usingCache = true;
        this.pollCacheStatus();

        this.statusLine = "Waiting for the first cached segment…";
        const ready = await this.waitForCacheSegments(ev.id, token);
        if (token !== this._selectToken) return;   // superseded
        if (!ready) {
          this.statusLine = "No segments cached yet. Trying to play anyway…";
        }

        const local = `${window.location.origin}/cache/${ev.id}/master.m3u8`;
        this.streamUrl = local;
        this.savedStreamUrl = local;

        await this.maybeUpdateHardware(local);
        this.connectStream({ userInitiated: true, keepStatus: !ready });
        this.loadBookmarks();
      } catch (e) {
        this.statusLine = "";
        this.error = `Cache failed: ${e.message}`;
      } finally {
        if (token === this._selectToken) this.selectingEventId = null;
      }
    },

    // waitForCacheSegments polls /cache/{id}/status (CacheStatus:
    // cached, active, segments, size_bytes) until at least one segment
    // has landed, the timeout passes, or a newer selection supersedes
    // this one.
    async waitForCacheSegments(id, token) {
      const deadline = Date.now() + CACHE_READY_TIMEOUT_MS;
      while (Date.now() < deadline) {
        if (token !== this._selectToken) return false;
        const st = await this.api(`/api/cache/${encodeURIComponent(id)}/status`).catch(() => null);
        if (st && st.segments > 0) {
          this.cacheStatus = st;
          return true;
        }
        await sleep(500);
      }
      return false;
    },

    async deleteCache(ev) {
      if (this.locked) return;
      const label = ev.type === "local_upload" ? "local upload" : "local cache";
      if (!confirm(`Delete ${label} for "${ev.name}"?`)) return;
      try {
        await this.api(`/api/cache/${ev.id}`, { method: "DELETE" });
        delete this.cachedEvents[ev.id];
        if (ev.type === "local_upload") {
          this.allEvents = this.allEvents.filter(e => e.id !== ev.id);
          this.localUploads = this.localUploads.filter(u => u.event_id !== ev.id);
        }
        if (this.activeCacheEventId === ev.id) this.teardownPlayer();
        this.showToast("Cache cleared", "good");
      } catch (e) {
        this.showToast(e.message, "bad");
      }
    },

    // teardownPlayer stops playback of the active cache and clears every
    // timer tied to it. Used when the cache being played is deleted.
    teardownPlayer() {
      if (this._cachePoll) { clearInterval(this._cachePoll); this._cachePoll = null; }
      if (this._statsInterval) { clearInterval(this._statsInterval); this._statsInterval = null; }
      clearTimeout(this._netRetryTimer);
      this._netRetryTimer = null;
      if (this.player) { try { this.player.destroy(); } catch (_) {} this.player = null; }
      const v = this.$refs.videoPlayer;
      if (v) { v.pause(); v.removeAttribute("src"); v.load(); }
      this.activeCacheEventId = null;
      this.usingCache = false;
      this.streamUrl = "";
      this.savedStreamUrl = "";
      this.hasPlayed = false;
      this.isReady = false;
      this.isPlaying = false;
      this.isLive = false;
      this.atLiveEdge = false;
      this.currentTime = 0;
      this.duration = 0;
      this.bookmarks = [];
      this.statusLine = "";
      this.cacheStatus = { cached: false, active: false, segments: 0, size_bytes: 0 };
      this.totalSegments = 0;
      this.cacheProgress = 0;
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
        const base = this.encoderBase();
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
    // connectStream attaches the in-page player to this.streamUrl.
    //   userInitiated: the operator picked this stream, so it's fine to
    //                  align the projector to the browser afterwards.
    //   adoptHardware: page (re)load. Never push state to mpv; instead
    //                  pull mpv's position and follow it.
    //   retry:         internal re-attach after a manifest load error.
    connectStream(opts = {}) {
      const { userInitiated = false, adoptHardware = false, retry = false, keepStatus = false } = opts;
      if (!this.streamUrl) return;
      if (!retry) {
        this.error = "";
        this._netRetries = 0;
        if (!keepStatus) this.statusLine = "";
      }
      clearTimeout(this._netRetryTimer);
      this._netRetryTimer = null;
      this.isReady = false;
      this.isLive = false;
      this.atLiveEdge = false;

      const video = this.$refs.videoPlayer;
      if (!video) return;
      video.muted = this.browserMuted;

      // Tear down any prior player.
      if (this.player) { try { this.player.destroy(); } catch (_) {} this.player = null; }

      // Wire <video> events. play/pause mirror to mpv only when they
      // weren't caused by us adopting mpv's own state.
      const echo = (cmd) => { if (Date.now() >= this._suppressHwUntil) this.syncHardware(cmd); };
      video.onplay  = () => { this.isPlaying = true;  echo("play"); };
      video.onpause = () => { this.isPlaying = false; echo("pause"); };
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
          this._netRetries = 0;
          this.statusLine = "";
          this.hasPlayed = true;
          const lvl = data.levels?.[0]?.details;
          this.isLive = lvl ? !!lvl.live : false;
          this.duration = isFinite(video.duration) && video.duration > 0 ? video.duration : 0;
          if (this.isLive) {
            const liveEdge = video.seekable.length > 0 ? video.seekable.end(video.seekable.length - 1) : this.duration;
            video.currentTime = Math.max(0, liveEdge - 5);
          } else {
            video.currentTime = 0;
          }
          this.currentTime = video.currentTime;
          this.isPlaying = !video.paused;

          if (!this.hardwareActive) return;
          if (adoptHardware) {
            // Reload path: follow the projector, don't steer it.
            this.adoptHardwarePosition().finally(() => this.startHardwareSync());
          } else if (userInitiated) {
            // Operator just picked this stream: align mpv to the browser.
            setTimeout(() => { this.syncHardware("seek"); this.syncHardware("pause"); }, 500);
          }
        });
        hls.on(Hls.Events.LEVEL_LOADED, (_e, data) => {
          if (data?.details) this.isLive = !!data.details.live;
        });
        hls.on(Hls.Events.FRAG_LOADED, () => {
          if (this._netRetries) { this._netRetries = 0; this.statusLine = ""; }
        });
        hls.on(Hls.Events.ERROR, (_e, data) => {
          if (!data.fatal) return;
          if (data.type === Hls.ErrorTypes.NETWORK_ERROR) {
            this.scheduleNetworkRetry(hls, data, opts);
          } else if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
            hls.recoverMediaError();
          } else {
            try { hls.destroy(); } catch (_) {}
            if (this.player === hls) this.player = null;
            this.statusLine = "";
            this.error = `Playback error: ${data.details || data.type}`;
          }
        });
        this.player = hls;
      } else if (video.canPlayType("application/vnd.apple.mpegurl")) {
        video.src = this.streamUrl;
        video.addEventListener("loadedmetadata", () => {
          this.hasPlayed = true;
          if (this.hardwareActive && adoptHardware) {
            this.adoptHardwarePosition().finally(() => this.startHardwareSync());
          }
        }, { once: true });
      } else {
        this.error = "This browser does not support HLS playback.";
      }

      this.startStats();
    },

    // scheduleNetworkRetry: fatal network errors back off exponentially
    // (1 s, 2 s, 4 s … capped at 15 s) and give up after HLS_MAX_RETRIES.
    // A failed manifest load destroys and re-attaches the player; other
    // network errors restart loading on the existing instance.
    scheduleNetworkRetry(hls, data, opts) {
      if (this.player !== hls) return;
      if (this._netRetries >= HLS_MAX_RETRIES) {
        try { hls.destroy(); } catch (_) {}
        this.player = null;
        this.statusLine = "";
        this.error = "Stream unavailable. Check the encoder and select the event again.";
        return;
      }
      const delay = Math.min(1000 * 2 ** this._netRetries, 15000);
      this._netRetries++;
      this.statusLine = "Waiting for stream…";
      const manifestFailed = [
        Hls.ErrorDetails.MANIFEST_LOAD_ERROR,
        Hls.ErrorDetails.MANIFEST_LOAD_TIMEOUT,
      ].includes(data.details);
      clearTimeout(this._netRetryTimer);
      this._netRetryTimer = setTimeout(() => {
        if (this.player !== hls) return;
        if (manifestFailed) this.connectStream({ ...opts, retry: true });
        else hls.startLoad();
      }, delay);
    },

    // adoptHardwarePosition seeks the browser to where mpv is (and plays
    // if mpv is playing) without echoing anything back to mpv.
    async adoptHardwarePosition() {
      const data = await this.api("/api/output/frame-position").catch(() => null);
      const v = this.$refs.videoPlayer;
      if (!data || !v || data.time_pos == null) return;
      let t = Number(data.time_pos);
      if (!Number.isFinite(t)) return;
      if (v.seekable.length > 0) {
        t = Math.max(v.seekable.start(0), Math.min(t, v.seekable.end(v.seekable.length - 1)));
      }
      this._suppressHwUntil = Date.now() + 2000;
      this._lastSeekTime = Date.now();
      v.currentTime = t;
      this.currentTime = t;
      if (data.paused === false && v.paused) {
        // May be refused by autoplay policy if browser audio is on;
        // the projector keeps playing either way.
        v.play().catch(() => {});
      }
    },

    async connectManualStream() {
      if (this.locked || !this.streamUrl) return;
      this.savedStreamUrl = this.streamUrl;
      const m = this.streamUrl.match(/\/cache\/([^/]+)\/master\.m3u8/);
      this.usingCache = !!m && this.streamUrl.startsWith(window.location.origin);
      this.activeCacheEventId = this.usingCache ? m[1] : null;
      if (this.usingCache) this.pollCacheStatus();
      else if (this._cachePoll) { clearInterval(this._cachePoll); this._cachePoll = null; }
      await this.maybeUpdateHardware(this.streamUrl);
      this.connectStream({ userInitiated: true });
    },

    startStats() {
      if (this._statsInterval) clearInterval(this._statsInterval);
      this._statsInterval = setInterval(() => {
        const v = this.$refs.videoPlayer;
        if (!v) return;
        if (v.buffered.length > 0) this.bufferLen = v.buffered.end(v.buffered.length - 1) - v.currentTime;
        this.resolution = v.videoWidth ? `${v.videoWidth}×${v.videoHeight}` : "—";
        this.updateLiveEdge(v);
        if (this.isLive && this.isConnected) {
          const now = Date.now();
          if (!this._lastBookmarkPoll || now - this._lastBookmarkPoll > 5000) {
            this._lastBookmarkPoll = now;
            this.loadBookmarks();
          }
        }
      }, 1000);
    },

    // updateLiveEdge: the LIVE badge only shows when the playhead is
    // within ~2 target durations of the live edge.
    updateLiveEdge(v) {
      if (!this.isLive || !v || v.seekable.length === 0) { this.atLiveEdge = false; return; }
      const hls = this.player;
      const lvl = hls?.levels?.[Math.max(0, hls.currentLevel)]?.details;
      const target = lvl?.targetduration || 6;
      const edge = v.seekable.end(v.seekable.length - 1);
      this.atLiveEdge = (edge - v.currentTime) <= target * 2;
    },

    goLive() {
      if (this.locked) return;
      const v = this.$refs.videoPlayer;
      if (!v) return;
      let t = this.player?.liveSyncPosition;
      if (!Number.isFinite(t) && v.seekable.length > 0) t = v.seekable.end(v.seekable.length - 1) - 3;
      if (!Number.isFinite(t)) return;
      this.seekTo(t);
      this.atLiveEdge = true;
    },

    currentFps() {
      return F.eventFps(this.allEvents.find(e => e.id === this.activeCacheEventId));
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
      // EventInfo.fps when the encoder reports it; 30 otherwise.
      this.seekTo(this.currentTime + (direction / this.currentFps()));
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
        time = Math.max(v.seekable.start(0), Math.min(time, v.seekable.end(v.seekable.length - 1)));
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
      if (this.locked) return;
      this.mpvMuted = !this.mpvMuted;
      try {
        await this.api("/api/output/mute", { method: "POST", body: { muted: this.mpvMuted } });
      } catch (_) {}
    },

    // ============================================================
    // HARDWARE OUTPUT (mpv)
    // ============================================================
    // checkOutputStatus runs at init and every OUTPUT_STATUS_MS. It
    // reconciles hardwareActive with mpv's real state (mpv may have
    // exited, or been started from another tab) and keeps the drift
    // poll running only while mpv is up.
    async checkOutputStatus() {
      if (this.isTogglingHardware) return;
      const data = await this.api("/api/output/status").catch(() => null);
      if (!data || this.isTogglingHardware) return;
      const running = !!data.is_running;
      if (running === this.hardwareActive) return;
      this.hardwareActive = running;
      if (running) {
        this.startHardwareSync();
      } else if (this._hwSyncPoll) {
        clearInterval(this._hwSyncPoll);
        this._hwSyncPoll = null;
      }
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
          this.resyncSelect("screenSelect", this.screenIndex);
          this.resyncSelect("decklinkSelect", this.decklinkFormat);
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
      if (this.locked || this.isTogglingHardware) return;
      this.isTogglingHardware = true;
      try {
        if (this.hardwareActive) {
          try {
            await this.api("/api/output/stop", { method: "POST" });
          } catch (e) {
            // Keep hardwareActive: we don't know that mpv stopped.
            this.showToast(`Stop failed: ${e.message}`, "bad");
            return;
          }
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
        } catch (e) {
          this.error = `Hardware output failed: ${e.message}`;
          this.showToast(this.error, "bad");
          return;
        }
        this.hardwareActive = true;
        // mpv always starts muted; reflect that rather than whatever
        // localStorage remembered from a previous session.
        this.mpvMuted = true;
        this.showToast("Projector starting…", "good");
        // Give mpv ~1s to come up, then push the browser's current
        // position so they start in sync. This is a user action.
        setTimeout(async () => {
          const v = this.$refs.videoPlayer;
          if (v && v.currentTime > 0) {
            await this._sendSync("seek", v.currentTime).catch(() => {});
            if (v.paused) await this._sendSync("pause", v.currentTime).catch(() => {});
          }
          this.startHardwareSync();
        }, 1000);
      } finally {
        this.isTogglingHardware = false;
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
    // Encoder bookmarks live on the remote encoder, so they're read from
    // ${encoderUrl}/api/events/{id}/bookmarks (the decoder's own store is
    // always empty). Decoder bookmarks are device-local (localStorage);
    // the decoder never writes to the encoder's bookmark store.
    async loadBookmarks() {
      const id = this.activeCacheEventId;
      if (!id) { this.bookmarks = []; return; }
      const decoderKey = `oc_dec_bookmarks_${id}`;
      let decoderBookmarks = [];
      try { decoderBookmarks = JSON.parse(localStorage.getItem(decoderKey) || "[]"); } catch (_) {}
      let encoderBookmarks = [];
      const base = this.encoderBase();
      const ev = this.allEvents.find(e => e.id === id);
      if (base && !(ev && ev.type === "local_upload")) {
        try {
          const r = await fetch(`${base}/api/events/${encodeURIComponent(id)}/bookmarks`);
          if (r.ok) {
            const data = await r.json();
            encoderBookmarks = (data?.bookmarks || []).map(b => ({ ...b, source: "encoder" }));
          }
        } catch (_) {}
      }
      if (id !== this.activeCacheEventId) return;   // switched events mid-fetch
      this.bookmarks = [...decoderBookmarks, ...encoderBookmarks].sort((a, b) => (a.frame || 0) - (b.frame || 0));
    },

    async createBookmark() {
      const v = this.$refs.videoPlayer;
      if (!v || !this.activeCacheEventId || this.locked) return;
      const fps = this.currentFps();
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
      let existing = [];
      try { existing = JSON.parse(localStorage.getItem(key) || "[]"); } catch (_) {}
      existing.push(bm);
      localStorage.setItem(key, JSON.stringify(existing));
      this.newBookmarkName = "";
      this.loadBookmarks();
    },

    deleteBookmark(bm) {
      if (this.locked) return;
      if (bm.source === "encoder") {
        this.showToast("Encoder bookmarks can only be removed from the encoder.", "bad");
        return;
      }
      const key = `oc_dec_bookmarks_${this.activeCacheEventId}`;
      let existing = [];
      try { existing = JSON.parse(localStorage.getItem(key) || "[]"); } catch (_) {}
      localStorage.setItem(key, JSON.stringify(existing.filter(b => b.id !== bm.id)));
      this.loadBookmarks();
    },

    async jumpToBookmark(bm) {
      if (this.locked) return;
      // Frame-precise: seek both browser and mpv to the exact frame.
      let t = bm.time_pos != null ? bm.time_pos : (bm.frame || 0) / (bm.fps || this.currentFps());
      const v = this.$refs.videoPlayer;
      if (this.isLive && v && v.seekable.length > 0) {
        t = Math.max(v.seekable.start(0), Math.min(t, v.seekable.end(v.seekable.length - 1)));
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
        if (!r.ok) throw new Error(F.errorDetail(await r.text(), r.status));
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
      // Tolerate a few 404s (status file not written yet), then stop.
      let notFound = 0;
      const tick = async () => {
        let data;
        try {
          data = await this.api(`/api/cache/upload/${eventId}/status`);
        } catch (e) {
          if (e.status === 404 && ++notFound >= UPLOAD_404_LIMIT) {
            this.uploadStatus = "Failed";
            this.isUploading = false;
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
          this.isUploading = false;
          this.loadLocalUploads();
          setTimeout(() => { this.uploadStatus = ""; this.uploadProgress = 0; }, 3000);
          return;
        }
        if (data?.status === "failed") {
          this.uploadStatus = "Processing failed";
          this.isUploading = false;
          this.showToast("Video processing failed", "bad");
          return;
        }
        this.uploadStatus = "Processing video…";
        this.uploadProgress = Math.min(data?.progress || 0, 95);
        setTimeout(tick, 2000);
      };
      tick();
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
