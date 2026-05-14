// EasyCampus — pure formatting helpers. No Alpine, no DOM, no state.
// Attached to window.OCFormat for use from the per-page modules.
window.OCFormat = (() => {
  function pad2(n) { return String(n).padStart(2, "0"); }

  // formatTime: H:MM:SS for durations and timestamps. Live durations use
  // the same shape so the operator never has to mentally re-parse the
  // separator when a clip is short vs. long.
  function formatTime(seconds) {
    if (seconds === undefined || seconds === null || isNaN(seconds)) return "00:00:00";
    const s = Math.max(0, Math.floor(seconds));
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    return `${pad2(h)}:${pad2(m)}:${pad2(s % 60)}`;
  }

  // parseTime accepts H:MM:SS, M:SS, or raw seconds. Used by the "Go to"
  // input. Returns null on bad input so the caller can ignore it.
  function parseTime(value) {
    if (value === undefined || value === null || value === "") return null;
    const parts = String(value).trim().split(":").map(Number);
    if (parts.some(isNaN)) return null;
    if (parts.length === 3) return parts[0] * 3600 + parts[1] * 60 + parts[2];
    if (parts.length === 2) return parts[0] * 60 + parts[1];
    if (parts.length === 1) return parts[0];
    return null;
  }

  function formatBytes(bytes) {
    if (!bytes || bytes <= 0) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB"];
    const i = Math.min(units.length - 1, Math.floor(Math.log(bytes) / Math.log(1024)));
    const v = bytes / Math.pow(1024, i);
    return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
  }

  function formatDate(iso) {
    if (!iso) return "—";
    const d = new Date(iso);
    if (isNaN(d.getTime())) return "—";
    const today = new Date();
    const sameDay = d.toDateString() === today.toDateString();
    const time = d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
    if (sameDay) return `Today, ${time}`;
    const yest = new Date(today.getTime() - 86400000);
    if (d.toDateString() === yest.toDateString()) return `Yesterday, ${time}`;
    return d.toLocaleDateString(undefined, { month: "short", day: "numeric" }) + `, ${time}`;
  }

  // bitrateKbps -> "4.0 Mbps" / "800 kbps". Bitrates in the backend are
  // passed as integers in bps; the UI displays in Mbps for >= 1 Mbps to
  // keep the picker readable.
  function formatBitrate(bps) {
    if (!bps || bps <= 0) return "—";
    if (bps >= 1_000_000) return `${(bps / 1_000_000).toFixed(bps % 1_000_000 === 0 ? 0 : 1)} Mbps`;
    return `${Math.round(bps / 1000)} kbps`;
  }

  // presetSummary renders the "what does this preset give me" hint under
  // the dropdown. Resolution is implied by bitrate target since the
  // backend chooses 720p for low and 1080p for everything else.
  function presetSummary(preset) {
    if (!preset) return "";
    return `${preset.resolution} · ${formatBitrate(preset.bitrate)} · ${preset.fps} fps`;
  }

  return { formatTime, parseTime, formatBytes, formatDate, formatBitrate, presetSummary };
})();
