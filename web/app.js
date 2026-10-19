"use strict";
// Media Watcher web UI. No framework, no build step: one file, hash routing.

const $ = (sel, el = document) => el.querySelector(sel);
const view = $("#view");
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

// ---------------------------------------------------------------- API --
class Unauthorized extends Error {}

async function api(path, opts = {}) {
  const init = { credentials: "same-origin", ...opts };
  if (opts.body !== undefined) {
    init.method = init.method || "POST";
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(opts.body);
  }
  const r = await fetch(path, init);
  if (r.status === 401) { showLogin(); throw new Unauthorized(); }
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

function beacon(path, body) {
  const blob = new Blob([JSON.stringify(body || {})], { type: "application/json" });
  if (!navigator.sendBeacon || !navigator.sendBeacon(path, blob)) {
    fetch(path, { method: "POST", body: blob, keepalive: true, credentials: "same-origin" }).catch(() => {});
  }
}

// --------------------------------------------------------- formatting --
function fmtTime(s) {
  s = Math.max(0, Math.floor(s || 0));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  return (h ? h + ":" + String(m).padStart(2, "0") : m) + ":" + String(sec).padStart(2, "0");
}
function fmtDur(s) {
  if (!s) return "";
  const m = Math.round(s / 60);
  return m >= 60 ? `${Math.floor(m / 60)}h ${m % 60}m` : `${m}m`;
}
function epLabel(it) {
  if (it.episode == null) return it.season === 0 ? "Special" : "";
  const s = it.season === 0 ? "SP" : `S${String(it.season).padStart(2, "0")}`;
  const e = `E${String(it.episode).padStart(2, "0")}` + (it.episode_end ? `-E${String(it.episode_end).padStart(2, "0")}` : "");
  return s + e;
}
function pct(it) {
  return it.duration && it.position ? Math.min(100, (it.position / it.duration) * 100) : 0;
}
const img = (src) => `<img loading="lazy" decoding="async" src="${src}" alt="" onerror="this.remove()">`;
// art_v changes when a poster is downloaded, so the browser's cached image is skipped.
const art = (kind, x) => `/img/${kind}/${x.id}${x.art_v ? "?v=" + x.art_v : ""}`;
const bar = (p) => (p > 0 ? `<div class="bar"><i style="width:${p.toFixed(1)}%"></i></div>` : "");

function seriesCard(s) {
  const unwatched = (s.episodes || 0) - (s.watched || 0);
  // An empty show folder: the series should be there but has no episodes yet.
  const badge = s.episodes == null ? "" : s.episodes === 0 ? `<span class="badge missing">MISSING</span>`
    : unwatched > 0 ? `<span class="badge">${unwatched}</span>` : `<span class="badge done">✓</span>`;
  const sub = [s.year, s.episodes != null ? `${s.episodes} ep` : ""].filter(Boolean).join(" · ");
  return `<a class="card" href="#/series/${s.id}"><div class="img">${badge}${esc(s.title)}${img(art("series", s))}</div>
    <div class="t" title="${esc(s.title)}">${esc(s.title)}</div><div class="s">${esc(sub)}</div></a>`;
}
function movieCard(m) {
  const badge = m.watched ? `<span class="badge done">✓</span>` : "";
  const sub = [m.year, fmtDur(m.duration)].filter(Boolean).join(" · ");
  return `<a class="card" href="#/play/${m.id}"><div class="img">${badge}${esc(m.title)}${img(art("item", m))}${bar(pct(m))}</div>
    <div class="t" title="${esc(m.title)}">${esc(m.title)}</div><div class="s">${esc(sub)}</div></a>`;
}
function itemCard(it, removable = false) {
  const title = it.series_title || it.title;
  const sub = it.series_title ? [epLabel(it), it.title].filter(Boolean).join(" · ") : fmtDur(it.duration);
  const x = removable ? `<button class="unresume" data-unresume="${it.id}" title="Remove from Continue watching">×</button>` : "";
  return `<a class="card wide" href="#/play/${it.id}"><div class="img">${x}${esc(title)}${img(art("item", it))}${bar(pct(it))}</div>
    <div class="t" title="${esc(title)}">${esc(title)}</div><div class="s" title="${esc(sub)}">${esc(sub)}</div></a>`;
}

// ------------------------------------------------------------- views ---
const cache = new Map(); // route -> last JSON, for instant back navigation
const scrolls = new Map();

async function cached(key, path, render) {
  const hit = cache.get(key);
  if (hit) render(hit.data, true);
  let data;
  try { data = await api(path); } catch (e) {
    if (!(e instanceof Unauthorized) && !hit) view.innerHTML = `<p class="error">${esc(e.message)}</p>`;
    return;
  }
  const json = JSON.stringify(data);
  if (hit && hit.json === json) return;
  cache.set(key, { data, json });
  if (currentRoute === key) render(data, !hit);
}

let libraries = [];
function renderLibs() {
  $("#libs").innerHTML = libraries.map((l) =>
    `<a href="#/lib/${encodeURIComponent(l.name)}" class="${currentRoute === "/lib/" + l.name ? "active" : ""}">${esc(l.name)}</a>`).join("");
}

function viewHome() {
  cached("/", "/api/home", (d, restore) => {
    libraries = d.libraries; renderLibs(); showStatus(d.status);
    const row = (title, items, fn, cls = "row-scroll") => items.length
      ? `<h2>${title}</h2><div class="${cls}">${items.map(fn).join("")}</div>` : "";
    const recent = (r) => r.type === "series" ? seriesCard(r) : movieCard(r);
    let html = row("Continue watching", d.continue, (it) => itemCard(it, true)) + row("Next up", d.next_up, (it) => itemCard(it))
      + row("Recently added", d.recent, recent, "grid");
    if (!html) {
      html = `<p class="empty">${d.libraries.length ? "Nothing here yet. The first scan may still be running." :
        "No libraries configured. Mount media under /media or set LIBRARIES."}</p>`;
    }
    const notice = d.problems ? `<a class="notice" href="#/problems">⚠ ${d.problems} file${d.problems === 1 ? "" : "s"} could not be read</a>` : "";
    view.innerHTML = notice + html;
    if (restore) restoreScroll();
  });
}

const JUMP_MIN = 40;  // items before a library grid gets the A–Z bar

// The × on a Continue watching card forgets the position, so it drops off the row.
view.addEventListener("click", async (e) => {
  const b = e.target.closest("[data-unresume]");
  if (!b) return;
  e.preventDefault();
  b.closest(".card").remove();
  try { await api(`/api/items/${b.dataset.unresume}/progress`, { body: { position: 0 } }); } catch (err) { /* re-rendered below */ }
  cache.clear();
  if (currentRoute === "/") viewHome();
});

function viewLibrary(name) {
  const key = "/lib/" + name;
  cached(key, "/api/libraries/" + encodeURIComponent(name), (d, restore) => {
    const state = { sort: "title", filter: "", show: "all", ...JSON.parse(sessionStorage.getItem("lib:" + name) || "{}") };
    view.innerHTML = `<div class="toolbar"><h1>${esc(d.name)}</h1>
      <input id="lib-filter" type="search" placeholder="Filter" value="${esc(state.filter)}">
      <select id="lib-show" title="Show">
        <option value="all">All</option><option value="unwatched">Hide watched</option>
        <option value="progress">In progress</option>
      </select>
      <select id="lib-sort">
        <option value="title">A–Z</option><option value="added">Recently added</option>
        <option value="played">Recently watched</option><option value="unwatched">Unwatched first</option>
      </select></div><div id="lib-jump" class="jump" hidden></div><div id="lib-grid" class="grid"></div>`;
    $("#lib-sort").value = state.sort;
    $("#lib-show").value = state.show;
    const draw = () => {
      const f = $("#lib-filter").value.trim().toLowerCase();
      const sort = $("#lib-sort").value;
      const show = $("#lib-show").value;
      sessionStorage.setItem("lib:" + name, JSON.stringify({ sort, filter: f, show }));
      let all = [
        ...d.series.map((s) => ({ ...s, _t: "s", _added: s.added_at, _played: s.played_at || 0, _left: s.episodes - s.watched,
          _started: s.watched > 0 || !!s.played_at })),
        ...d.movies.map((m) => ({ ...m, _t: "m", _added: m.added_at, _played: 0, _left: m.watched ? 0 : 1, _started: m.position > 0 })),
      ];
      if (f) all = all.filter((x) => x.title.toLowerCase().includes(f));
      if (show === "unwatched") all = all.filter((x) => x._left > 0);
      else if (show === "progress") all = all.filter((x) => x._left > 0 && x._started);
      const by = {
        title: () => 0,
        added: (a, b) => b._added - a._added,
        played: (a, b) => b._played - a._played,
        unwatched: (a, b) => (b._left > 0) - (a._left > 0),
      }[sort];
      if (sort === "title") all.sort((a, b) => (a.sort_name || a.title).localeCompare(b.sort_name || b.title));
      else all.sort(by);
      const grid = $("#lib-grid");
      grid.innerHTML = all.map((x) => (x._t === "s" ? seriesCard(x) : movieCard(x))).join("")
        || `<p class="empty">Nothing found.</p>`;
      // A–Z jump bar for long alphabetical grids: one button per first letter present.
      const jump = $("#lib-jump");
      const letters = [];
      if (sort === "title" && all.length >= JUMP_MIN) {
        all.forEach((x, i) => {
          const c = (x.sort_name || x.title).charAt(0).toUpperCase();
          const l = c >= "A" && c <= "Z" ? c : "#";
          if (!letters.length || letters[letters.length - 1][0] !== l) letters.push([l, i]);
        });
      }
      jump.hidden = letters.length < 2;
      jump.innerHTML = letters.map(([l, i]) => `<button data-at="${i}">${l}</button>`).join("");
      jump.onclick = (e) => {
        const b = e.target.closest("[data-at]");
        if (b) grid.children[Number(b.dataset.at)].scrollIntoView({ block: "start" });
      };
    };
    $("#lib-filter").oninput = draw;
    $("#lib-sort").onchange = draw;
    $("#lib-show").onchange = draw;
    draw();
    if (restore) restoreScroll();
  });
}

function viewSeries(id) {
  const key = "/series/" + id;
  cached(key, "/api/series/" + id, (d, restore) => {
    const seasons = [...new Set(d.episodes.map((e) => e.season))].sort((a, b) => (a === 0) - (b === 0) || a - b);
    const next = d.episodes.find((e) => e.id === d.next_id);
    let season = Number(sessionStorage.getItem("season:" + id) ?? (next ? next.season : seasons[0]));
    if (!seasons.includes(season)) season = seasons[0];
    const playLabel = next ? (next.position > 0 ? `▶ Resume ${epLabel(next)}` : `▶ Play ${epLabel(next) || next.title}`) : "";
    const watched = d.episodes.filter((e) => e.watched).length;
    view.innerHTML = `<div class="series-head">${`<img class="poster" src="${art("series", d)}" alt="" onerror="this.style.visibility='hidden'">`}
      <div><h1>${esc(d.title)}</h1>
      <div class="meta">${[d.year, `${d.episodes.length} episodes`, `${watched} watched`].filter(Boolean).join(" · ")}</div>
      ${d.overview ? `<p class="overview">${esc(d.overview)}</p>` : ""}
      ${d.episodes.length ? `<div class="actions">${next ? `<a class="btn" href="#/play/${next.id}">${esc(playLabel)}</a>` : ""}
        <button class="secondary" id="mark-season">Mark season watched</button>
        <button class="secondary" id="unmark-season">Mark season unwatched</button></div>`
        : `<div class="actions"><span class="badge missing">MISSING</span></div>`}</div></div>
      ${seasons.length > 1 ? `<div class="tabs">${seasons.map((s) =>
        `<button data-season="${s}" class="${s === season ? "active" : ""}">${s === 0 ? "Specials" : "Season " + s}</button>`).join("")}</div>` : ""}
      <div class="episodes"></div>`;
    const drawEpisodes = () => {
      if (!d.episodes.length) {
        $(".episodes").innerHTML = `<p class="empty">No episodes yet: the folder is empty.</p>`;
        return;
      }
      $(".episodes").innerHTML = d.episodes.filter((e) => e.season === season).map((e) => `
        <div class="ep ${e.watched ? "watched" : ""}" data-id="${e.id}">
          <div class="img">${img("/img/item/" + e.id)}${bar(pct(e))}</div>
          <div><div class="name">${esc([e.episode != null ? e.episode + "." : "", e.title].join(" "))}</div>
            <div class="s">${esc([fmtDur(e.duration), e.height ? e.height + "p" : "", e.video_codec].filter(Boolean).join(" · "))}</div>
            <div class="file">${esc(e.file)}</div></div>
          <button class="ghost check" data-watch="${e.id}" title="Toggle watched">${e.watched ? "✓" : "○"}</button>
        </div>`).join("");
    };
    drawEpisodes();
    view.querySelectorAll(".tabs button").forEach((b) => b.onclick = () => {
      season = Number(b.dataset.season);
      sessionStorage.setItem("season:" + id, season);
      view.querySelectorAll(".tabs button").forEach((x) => x.classList.toggle("active", x === b));
      drawEpisodes();
    });
    $(".episodes").onclick = async (ev) => {
      const w = ev.target.closest("[data-watch]");
      if (w) {
        ev.stopPropagation();
        const e = d.episodes.find((x) => x.id === w.dataset.watch);
        await api(`/api/items/${e.id}/watched`, { body: { watched: !e.watched } });
        cache.delete(key); viewSeries(id);
        return;
      }
      const row = ev.target.closest(".ep");
      if (row) location.hash = "#/play/" + row.dataset.id;
    };
    const markSeason = async (watched) => {
      await api(`/api/series/${id}/watched`, { body: { watched, season } });
      cache.delete(key); viewSeries(id);
    };
    if (d.episodes.length) {
      $("#mark-season").onclick = () => markSeason(true);
      $("#unmark-season").onclick = () => markSeason(false);
    }
    if (restore) restoreScroll();
  });
}

async function viewSearch(q) {
  $("#search").value = q;
  view.innerHTML = `<p class="empty">Searching…</p>`;
  const d = await api("/api/search?q=" + encodeURIComponent(q));
  const all = d.series.map(seriesCard).concat(d.movies.map(movieCard));
  view.innerHTML = `<h2>Results for “${esc(q)}”</h2>` +
    (all.length ? `<div class="grid">${all.join("")}</div>` : `<p class="empty">Nothing found.</p>`);
}

function viewProblems() {
  cached("/problems", "/api/problems", (d) => {
    const files = d.files.map((f) => `<div><div class="file">${esc(f.library)} / ${esc(f.path)}</div>
      <div class="why">${esc(f.error || "unknown error")}</div></div>`).join("");
    const missing = d.missing.map((l) => `<div><div class="file">Library ${esc(l.name)}: ${esc(l.path)}</div>
      <div class="why">Folder not found at the last scan. Nothing in it was removed.</div></div>`).join("");
    view.innerHTML = `<div class="toolbar"><h1>Problems</h1>
      <button id="retry" ${d.running ? "disabled" : ""}>${d.running ? "Scan running…" : "Retry now"}</button></div>
      ${missing ? `<h2>Missing libraries</h2><div class="problems">${missing}</div>` : ""}
      ${files ? `<h2>Files that could not be read (${d.files.length})</h2><div class="problems">${files}</div>` : ""}
      ${!files && !missing ? `<p class="empty">${d.last_error ? "Last scan: " + esc(d.last_error) : "No problems."}</p>` : ""}`;
    $("#retry").onclick = async () => {
      $("#retry").disabled = true;
      $("#retry").textContent = "Scan running…";
      await api("/api/scan", { body: {} });
      $("#scan-status").textContent = "Scanning…";
      setTimeout(pollStatus, 800);
    };
  });
}

function restoreScroll() {
  const y = scrolls.get(currentRoute);
  if (y) requestAnimationFrame(() => window.scrollTo(0, y));
}

// ------------------------------------------------------------ router ---
let currentRoute = null;
// How many entries of this app lie behind the current one in the tab's
// history. Kept in history.state, so it survives reloads and back/forward.
// At 0 the entry behind is another site, a wake-up page or nothing at all,
// and history.back() would not land on a view of this app.
let depth = 0;
function stampDepth() {
  const s = history.state;
  if (s && typeof s.depth === "number") { depth = s.depth; return; }
  depth = currentRoute === null ? 0 : depth + 1;
  history.replaceState({ ...(s || {}), depth }, "");
}
// Show another route in place of the current entry (no new history entry).
function replaceRoute(hash) {
  history.replaceState(history.state, "", hash);
  route();
}
// Leave the current view the way the browser's back button would, or, when
// there is no view of this app to go back to, replace it with `fallback`.
function goBack(fallback = "#/") {
  if (depth > 0) history.back();
  else replaceRoute(fallback);
}

function route() {
  stampDepth();
  const hash = location.hash.replace(/^#/, "") || "/";
  const [path, qs] = hash.split("?");
  if (currentRoute && !currentRoute.startsWith("/play/")) scrolls.set(currentRoute, window.scrollY);
  const prev = currentRoute;
  currentRoute = decodeURIComponent(path);
  const m = (re) => currentRoute.match(re);
  let r;
  if ((r = m(/^\/play\/([0-9a-f]+)$/))) { Player.open(r[1]); return; }
  Player.close();
  if (prev && prev.startsWith("/play/") && view.childElementCount) {
    // Coming back from the player: the page underneath is still there,
    // just refresh it (progress bars) instead of starting from blank.
  } else {
    window.scrollTo(0, 0);
  }
  renderLibs();
  if ((r = m(/^\/lib\/(.+)$/))) viewLibrary(r[1]);
  else if ((r = m(/^\/series\/([0-9a-f]+)$/))) viewSeries(r[1]);
  else if (m(/^\/problems$/)) viewProblems();
  else if (m(/^\/search$/)) viewSearch(new URLSearchParams(qs).get("q") || "");
  else viewHome();
}

// ---------------------------------------------------------- scanning ---
let statusTimer = null;
function showStatus(s) {
  if (!s) return;
  const el = $("#scan-status");
  if (s.running) {
    el.textContent = s.phase === "probing" ? `Scanning ${s.probed}/${s.to_probe}` :
      s.phase === "metadata" ? `Fetching metadata ${s.fetched}/${s.to_fetch}` :
      s.phase === "thumbnails" ? "Making thumbnails" : `Scanning ${s.files} files`;
  } else {
    el.textContent = s.error ? "⚠ " + s.error : "";
  }
  if (s.error && !s.running) el.href = "#/problems";
  else el.removeAttribute("href");
  clearTimeout(statusTimer);
  statusTimer = setTimeout(pollStatus, s.running ? 3000 : 60000);
}
async function pollStatus() {
  try {
    const s = await api("/api/status");
    const wasRunning = $("#scan-status").textContent.startsWith("Scanning");
    showStatus(s);
    if (wasRunning && !s.running && !Player.active) { cache.clear(); route(); }
  } catch (e) { /* login screen handles 401 */ }
}
$("#scan-btn").onclick = async () => {
  await api("/api/scan", { body: {} });
  $("#scan-status").textContent = "Scanning…";
  setTimeout(pollStatus, 800);
};
$("#search-form").onsubmit = (e) => {
  e.preventDefault();
  const q = $("#search").value.trim();
  if (q) location.hash = "#/search?q=" + encodeURIComponent(q);
};

// ------------------------------------------------------------- login ---
function showLogin() {
  $("#login").hidden = false;
  $("#login-key").focus();
}
$("#login-form").onsubmit = async (e) => {
  e.preventDefault();
  $("#login-error").textContent = "";
  const r = await fetch("/api/login", { method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ key: $("#login-key").value }), credentials: "same-origin" });
  if (!r.ok) { $("#login-error").textContent = "Wrong key"; return; }
  $("#login").hidden = true;
  $("#login-key").value = "";
  cache.clear();
  route();
};

// ------------------------------------------------- browser capabilities --
// Sent with every playback request so the server only copies what this
// browser can decode, and transcodes nothing it does not have to.
const CAPS = (() => {
  const v = document.createElement("video");
  const ok = (t) => {
    try { if (window.MediaSource && MediaSource.isTypeSupported(t)) return true; } catch (e) { /* ignore */ }
    return v.canPlayType(t) === "probably";
  };
  const vc = [], ac = [];
  if (ok('video/mp4; codecs="hvc1.1.6.L120.90"')) vc.push("hevc");
  if (ok('video/mp4; codecs="hvc1.2.4.L120.90"')) vc.push("hevc10");
  if (ok('video/mp4; codecs="av01.0.08M.08"')) vc.push("av1");
  if (ok('video/mp4; codecs="vp09.00.40.08"')) vc.push("vp9");
  if (ok('audio/mp4; codecs="opus"')) ac.push("opus");
  if (ok('audio/mp4; codecs="flac"')) ac.push("flac");
  if (ok('audio/mp4; codecs="ac-3"')) ac.push("ac3");
  if (ok('audio/mp4; codecs="ec-3"')) ac.push("eac3");
  return { v: vc.join(","), a: ac.join(",") };
})();

// ------------------------------------------------------------ player ---
const video = $("#video");
const RATES = [0.75, 1, 1.25, 1.5, 1.75, 2];
// A paused remux/transcode keeps ffmpeg and its few hundred MB alive. After
// this long the stream is dropped and restarted from the same spot on play.
const PARK_AFTER = 3 * 60 * 1000;

// Series id for an item, from views already loaded, so the remembered audio
// language can go along with the very first playback request.
function seriesOf(id) {
  for (const { data } of cache.values()) {
    for (const list of [data.continue, data.next_up, data.episodes]) {
      const it = (list || []).find((x) => x.id === id);
      if (it) return it.series_id || null;
    }
  }
  return null;
}
const Player = {
  active: false,
  id: null,
  info: null,
  offset: 0,          // stream start time; video.currentTime is relative to it
  attempt: 0,         // 0 = best, 1 = forced remux with safe codecs, 2 = forced transcode
  retries: 0,
  audioIndex: null,
  token: 0,
  pendingSeek: null,
  parked: false,      // stream dropped while paused; play() restarts it
  rate: 1,
  seriesHint: null,
  sid: Math.random().toString(36).slice(2, 12),
  timers: {},

  time() {
    if (this.pendingSeek != null) return this.pendingSeek;
    return this.info && this.info.mode === "direct" ? video.currentTime : this.offset + video.currentTime;
  },
  duration() {
    return (this.info && this.info.duration) || (this.info && this.info.mode === "direct" ? video.duration : 0) || 0;
  },

  open(id) {
    if (this.active && this.id === id) return;
    if (this.active) this.report();
    const prev = this.info;
    this.seriesHint = (prev && prev.next && prev.next.id === id ? prev.next.series_id : null) || seriesOf(id);
    this.stopStream();
    this.active = true;
    this.id = id;
    this.attempt = 0;
    this.retries = 0;
    this.audioIndex = null;
    this.info = null;
    $("#player").hidden = false;
    document.body.style.overflow = "hidden";
    $("#p-error").hidden = true;
    $("#p-next-overlay").hidden = true;
    $("#p-skip").hidden = true;
    $("#p-convert").hidden = true;
    $("#p-convert-btn").hidden = true;
    $("#p-title").textContent = "";
    $("#p-sub").textContent = "";
    this.load(null);
    this.poke();
  },

  close() {
    if (!this.active) return;
    this.report();
    this.active = false;
    this.stopStream();
    Object.values(this.timers).forEach(clearTimeout);
    clearInterval(this.timers.report);
    $("#player").hidden = true;
    document.body.style.overflow = "";
    if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
    cache.delete("/");
    if (this.info && this.info.item.series_id) cache.delete("/series/" + this.info.item.series_id);
  },

  stopStream() {
    video.pause();
    video.removeAttribute("src");
    video.load();
    beacon(`/api/streams/${this.sid}/stop`);
    // New id, so a late stop request can never hit the next stream.
    this.sid = Math.random().toString(36).slice(2, 12);
  },

  async load(t) {
    const token = ++this.token;
    this.parked = false;
    clearTimeout(this.timers.park);
    const q = new URLSearchParams({ sid: this.sid });
    if (t != null) q.set("t", t.toFixed(2));
    const compat = localStorage.getItem("compat") === "1";
    if (this.attempt === 0 && !compat) { q.set("vcaps", CAPS.v); q.set("acaps", CAPS.a); }
    if (this.attempt === 1) q.set("force", "remux");
    if (this.attempt >= 2) q.set("force", "transcode");
    if (this.audioIndex != null) q.set("audio", this.audioIndex);
    const auto = this.audioIndex == null;
    const hintLang = this.seriesHint && localStorage.getItem("audio:" + this.seriesHint);
    if (auto && hintLang) q.set("alang", hintLang);
    $("#spinner").hidden = false;
    let info;
    try {
      info = await api(`/api/items/${this.id}/playback?${q}`);
    } catch (e) {
      if (token === this.token && !(e instanceof Unauthorized)) this.fail(e.message);
      return;
    }
    if (token !== this.token || !this.active) return;
    const lang = auto && info.item.series_id && localStorage.getItem("audio:" + info.item.series_id);
    const cur = info.audio_tracks.find((x) => x.index === info.audio_index);
    const want = lang && cur && cur.language !== lang && info.audio_tracks.find((x) => x.language === lang);
    if (want) {
      // The series was not known up front (deep link): ask again with the remembered track.
      this.audioIndex = want.index;
      this.load(t);
      return;
    }
    this.info = info;
    this.audioIndex = info.audio_index;
    this.offset = info.mode === "direct" ? 0 : info.start;
    this.pendingSeek = null;
    this.renderInfo();
    video.src = info.url;
    video.defaultPlaybackRate = video.playbackRate = this.rate;
    if (info.mode === "direct" && info.start > 0) {
      video.addEventListener("loadedmetadata", () => { video.currentTime = info.start; }, { once: true });
    }
    video.volume = Number(localStorage.getItem("volume") ?? 1);
    video.play().catch(() => { $("#spinner").hidden = true; this.renderPlay(); });
    this.watchdog();
  },

  // A stream that never starts is treated like a decode error.
  watchdog() {
    clearTimeout(this.timers.watchdog);
    const token = this.token;
    this.timers.watchdog = setTimeout(() => {
      if (token === this.token && this.active && video.readyState < 3 && !video.paused) this.onError("timeout");
    }, 30000);
  },

  onError(why) {
    if (!this.active || !this.info) return;
    const at = this.time();
    if (this.attempt < 2) {
      this.attempt += 1;
      console.warn(`playback failed (${why}) in ${this.info.mode}, falling back to attempt ${this.attempt}`);
      this.load(at);
    } else if (this.retries < 2) {
      this.retries += 1;
      this.load(at);
    } else {
      this.fail(`This file could not be played (${why}). Try the download button.`);
    }
  },

  play() {
    if (this.parked) { this.load(this.pendingSeek); return; }
    video.play().catch(() => {});
  },
  togglePlay() { video.paused ? this.play() : video.pause(); },

  armPark() {
    clearTimeout(this.timers.park);
    if (!this.info || this.info.mode === "direct" || this.parked) return;  // direct play costs nothing paused
    const token = this.token;
    this.timers.park = setTimeout(() => {
      if (token !== this.token || !this.active || !video.paused) return;
      const t = this.time();
      this.stopStream();
      this.pendingSeek = t;  // time() keeps reporting the paused position
      this.parked = true;
    }, PARK_AFTER);
  },

  setRate(r) {
    this.rate = r;
    video.defaultPlaybackRate = video.playbackRate = r;
    this.renderRate();
  },
  stepRate(dir) {
    const i = RATES.indexOf(this.rate);
    this.setRate(RATES[Math.max(0, Math.min(RATES.length - 1, (i < 0 ? 1 : i) + dir))]);
  },
  renderRate() {
    $("#p-speed-btn").textContent = `${this.rate}×`;
    $("#p-speed-menu").innerHTML = RATES.map((r) =>
      `<button data-rate="${r}" class="${r === this.rate ? "active" : ""}">${r}×</button>`).join("");
  },

  fail(msg) {
    $("#spinner").hidden = true;
    $("#p-error").textContent = msg;
    $("#p-error").hidden = false;
  },

  renderInfo() {
    const it = this.info.item;
    $("#p-title").textContent = it.series_title || it.title;
    $("#p-sub").textContent = it.series_title ? [epLabel(it), it.title].filter(Boolean).join(" · ") : (it.year || "");
    const mode = $("#p-mode");
    mode.textContent = { direct: "Direct", remux: "Remux", transcode: "Transcode" }[this.info.mode];
    mode.className = "badge " + this.info.mode;
    mode.title = `${this.info.reason}\nvideo: ${this.info.video}, audio: ${this.info.audio}`;
    const conv = this.info.convert;
    $("#p-convert-btn").hidden = !conv;
    if (!conv) $("#p-convert").hidden = true;
    else {
      $("#p-convert-reason").textContent = conv.reason;
      $("#p-convert-folder").textContent = conv.folder;
      $("#p-convert-file").textContent = conv.file_command;
      $("#p-convert-all").hidden = conv.files < 2;
      $("#p-convert-count").textContent = conv.files;
      $("#p-convert-folder-cmd").textContent = conv.folder_command;
    }
    $("#p-prev").hidden = !this.info.prev_id;
    $("#p-next").hidden = !this.info.next_id;
    $("#p-download").href = `/file/${it.id}?download=1`;
    const tracks = this.info.audio_tracks;
    $("#p-audio-btn").hidden = tracks.length < 2;
    $("#p-audio-menu").innerHTML = tracks.map((t) =>
      `<button data-index="${t.index}" class="${t.index === this.audioIndex ? "active" : ""}">${esc(t.label)}</button>`).join("");
    document.title = `${it.series_title ? it.series_title + " " + epLabel(it) : it.title} – Watcher`;
    if ("mediaSession" in navigator) {
      navigator.mediaSession.metadata = new MediaMetadata({
        title: it.series_title ? `${epLabel(it)} ${it.title}` : it.title,
        artist: it.series_title || "",
        artwork: [{ src: `/img/item/${it.id}`, type: "image/jpeg" }],
      });
    }
  },

  renderPlay() { $("#p-play").textContent = video.paused ? "▶" : "⏸"; },

  renderTime() {
    const d = this.duration(), t = this.time();
    $("#p-time").textContent = `${fmtTime(t)} / ${fmtTime(d)}`;
    $("#p-played").style.width = d ? `${Math.min(100, (t / d) * 100)}%` : "0";
    let buf = 0;
    if (video.buffered.length) {
      const end = video.buffered.end(video.buffered.length - 1);
      buf = this.info && this.info.mode === "direct" ? end : this.offset + end;
    }
    $("#p-buffer").style.width = d ? `${Math.min(100, (buf / d) * 100)}%` : "0";
    this.renderSkip();
  },

  // Intro/credits range (from the file's chapters) the play position is in.
  currentSkip() {
    if (!this.info || !this.info.skips || !$("#p-next-overlay").hidden) return null;
    const t = this.time();
    return this.info.skips.find((x) => t >= x.start && t < x.end - 1) || null;
  },
  // Credits that run to the end of the file: skipping them means the episode is done.
  creditsAtEnd(s) {
    const d = this.duration();
    return s.kind === "credits" && d && s.end >= d - 10;
  },
  renderSkip() {
    const s = this.currentSkip(), b = $("#p-skip");
    b.hidden = !s;
    if (s) b.textContent = s.kind === "intro" ? "Skip intro" : this.creditsAtEnd(s) && this.info.next_id ? "Next episode" : "Skip credits";
  },
  skip() {
    const s = this.currentSkip();
    if (!s) return;
    $("#p-skip").hidden = true;
    if (!this.creditsAtEnd(s)) { this.seek(s.end); return; }
    this.info.done = true;  // every later report of this item says "watched"
    this.report(true);
    if (this.info.next_id) this.go(this.info.next_id);
    else this.exit();
  },

  seek(t) {
    if (!this.info) return;
    const d = this.duration();
    t = Math.max(0, d ? Math.min(t, d - 2) : t);
    if (this.info.mode === "direct") { video.currentTime = t; return; }
    // Within what is already buffered: seek locally, no new stream needed.
    const rel = t - this.offset;
    for (let i = 0; i < video.buffered.length; i++) {
      if (rel >= video.buffered.start(i) && rel <= video.buffered.end(i) - 1) { video.currentTime = rel; return; }
    }
    this.pendingSeek = t;
    this.renderTime();
    clearTimeout(this.timers.seek);
    this.timers.seek = setTimeout(() => this.load(t), 350);
  },

  report(ended = false) {
    if (!this.info || !this.id) return;
    const pos = this.time();
    ended = ended || !!this.info.done;
    if (!ended && pos < 5) return;
    beacon(`/api/items/${this.info.item.id}/progress`, { position: pos, duration: this.duration(), ended });
  },

  ended() {
    const d = this.duration();
    if (this.info.mode !== "direct" && d && this.time() < d - 30) {
      // The stream stopped early (ffmpeg died, network blip): pick it up again.
      this.onError("stream ended early");
      return;
    }
    this.report(true);
    if (!this.info.next_id) { this.exit(); return; }
    const total = 8;
    let left = total;
    const it = this.info;
    const n = it.next || {};
    $("#p-next-img").style.visibility = "";
    $("#p-next-img").src = `/img/item/${it.next_id}`;
    $("#p-next-label").textContent = epLabel(n);
    $("#p-next-title").textContent = n.title || "";
    const draw = () => {
      $("#p-next-count").textContent = left;
      $("#p-next-ring").style.setProperty("--p", left / total);
    };
    draw();
    $("#p-next-overlay").hidden = false;
    const tick = () => {
      if (!this.active || this.info !== it) return;
      left -= 1;
      draw();
      if (left <= 0) this.go(it.next_id);
      else this.timers.next = setTimeout(tick, 1000);
    };
    this.timers.next = setTimeout(tick, 1000);
  },

  go(id) {
    if (id) replaceRoute("#/play/" + id);
  },

  // Back to where the player was opened from; for a deep link, the episode's series.
  exit() {
    const sid = this.info ? this.info.item.series_id : this.seriesHint;
    goBack(sid ? "#/series/" + sid : "#/");
  },

  // Show the controls, hide them again after a few idle seconds.
  poke() {
    const p = $("#player");
    p.classList.remove("idle");
    clearTimeout(this.timers.idle);
    this.timers.idle = setTimeout(() => {
      if (!video.paused && $("#p-audio-menu").hidden && $("#p-speed-menu").hidden && $("#p-convert").hidden) p.classList.add("idle");
    }, 3000);
  },

  toggleFullscreen() {
    const p = $("#player");
    if (document.fullscreenElement) document.exitFullscreen();
    else if (p.requestFullscreen) p.requestFullscreen().catch(() => {});
    else if (video.webkitEnterFullscreen) video.webkitEnterFullscreen();
  },
};

video.addEventListener("playing", () => {
  $("#spinner").hidden = true;
  $("#p-error").hidden = true;
  Player.retries = 0;
  Player.renderPlay();
});
video.addEventListener("waiting", () => { $("#spinner").hidden = false; });
video.addEventListener("pause", () => { Player.renderPlay(); Player.report(); Player.poke(); Player.armPark(); });
video.addEventListener("play", () => { clearTimeout(Player.timers.park); Player.renderPlay(); });
video.addEventListener("timeupdate", () => Player.renderTime());
video.addEventListener("progress", () => Player.renderTime());
video.addEventListener("ended", () => Player.ended());
video.addEventListener("error", () => {
  if (!Player.active || !video.getAttribute("src")) return;
  const e = video.error;
  Player.onError(e ? `media error ${e.code}${e.message ? ": " + e.message : ""}` : "media error");
});
video.addEventListener("volumechange", () => {
  localStorage.setItem("volume", video.volume);
  $("#p-volume").value = video.muted ? 0 : video.volume;
});
setInterval(() => { if (Player.active && !video.paused) Player.report(); }, 10000);

$("#p-play").onclick = () => Player.togglePlay();
$("#p-back").onclick = () => Player.exit();
$("#p-next").onclick = () => { Player.report(); Player.go(Player.info && Player.info.next_id); };
$("#p-prev").onclick = () => { Player.report(); Player.go(Player.info && Player.info.prev_id); };
$("#p-fs").onclick = () => Player.toggleFullscreen();
$("#p-volume").value = localStorage.getItem("volume") ?? 1;
$("#p-volume").oninput = (e) => { video.muted = false; video.volume = Number(e.target.value); };
$("#p-skip").onclick = () => Player.skip();
$("#p-next-now").onclick = () => Player.go(Player.info.next_id);
$("#p-next-cancel").onclick = () => { clearTimeout(Player.timers.next); $("#p-next-overlay").hidden = true; };
$("#p-convert-btn").onclick = () => { $("#p-convert").hidden = !$("#p-convert").hidden; };
$("#p-convert-close").onclick = () => { $("#p-convert").hidden = true; };
$("#p-convert").addEventListener("click", async (e) => {
  const b = e.target.closest("button[data-copy]");
  if (!b) return;
  const pre = $("#" + b.dataset.copy);
  try {
    await navigator.clipboard.writeText(pre.textContent);  // only on https or localhost
  } catch (err) {
    const range = document.createRange();
    range.selectNodeContents(pre);
    getSelection().removeAllRanges();
    getSelection().addRange(range);
    document.execCommand("copy");
  }
  b.textContent = "Copied";
  setTimeout(() => { b.textContent = "Copy"; }, 1500);
});
$("#p-audio-btn").onclick = (e) => {
  e.stopPropagation();
  $("#p-speed-menu").hidden = true;
  $("#p-audio-menu").hidden = !$("#p-audio-menu").hidden;
};
$("#p-audio-menu").onclick = (e) => {
  const b = e.target.closest("button");
  if (!b) return;
  $("#p-audio-menu").hidden = true;
  Player.audioIndex = Number(b.dataset.index);
  // Remember the language for the rest of the series.
  const sid = Player.info && Player.info.item.series_id;
  const track = Player.info && Player.info.audio_tracks.find((x) => x.index === Player.audioIndex);
  if (sid && track && track.language) localStorage.setItem("audio:" + sid, track.language);
  else if (sid) localStorage.removeItem("audio:" + sid);
  Player.load(Player.time());
};
$("#p-speed-btn").onclick = (e) => {
  e.stopPropagation();
  $("#p-audio-menu").hidden = true;
  $("#p-speed-menu").hidden = !$("#p-speed-menu").hidden;
};
$("#p-speed-menu").onclick = (e) => {
  const b = e.target.closest("button");
  if (!b) return;
  $("#p-speed-menu").hidden = true;
  Player.setRate(Number(b.dataset.rate));
};
Player.renderRate();

video.addEventListener("click", () => {
  if (!$("#p-audio-menu").hidden || !$("#p-speed-menu").hidden) {
    $("#p-audio-menu").hidden = $("#p-speed-menu").hidden = true;
    return;
  }
  Player.togglePlay();
});
video.addEventListener("dblclick", () => Player.toggleFullscreen());
$("#player").addEventListener("pointermove", () => Player.poke());

// Seek bar: click or drag.
(() => {
  const seekEl = $("#p-seek"), hover = $("#p-hover");
  const at = (e) => {
    const r = seekEl.getBoundingClientRect();
    return Math.max(0, Math.min(1, (e.clientX - r.left) / r.width)) * Player.duration();
  };
  let dragging = false;
  seekEl.addEventListener("pointerdown", (e) => { dragging = true; seekEl.setPointerCapture(e.pointerId); });
  seekEl.addEventListener("pointermove", (e) => {
    const t = at(e);
    hover.hidden = false;
    hover.textContent = fmtTime(t);
    hover.style.left = `${(t / (Player.duration() || 1)) * 100}%`;
    if (dragging) $("#p-played").style.width = `${(t / (Player.duration() || 1)) * 100}%`;
  });
  seekEl.addEventListener("pointerleave", () => { hover.hidden = true; });
  seekEl.addEventListener("pointerup", (e) => { if (dragging) { dragging = false; Player.seek(at(e)); } });
})();

document.addEventListener("keydown", (e) => {
  if (!Player.active || e.target.tagName === "INPUT") return;
  const k = e.key;
  if (k === "Escape" && !$("#p-convert").hidden) { $("#p-convert").hidden = true; e.preventDefault(); return; }
  const step = (s) => Player.seek(Player.time() + s);
  if (k === " " || k === "k") Player.togglePlay();
  else if (k === "ArrowRight" || k === "l") step(k === "l" ? 30 : 10);
  else if (k === "ArrowLeft" || k === "j") step(k === "j" ? -30 : -10);
  else if (k === "ArrowUp") video.volume = Math.min(1, video.volume + 0.05);
  else if (k === "ArrowDown") video.volume = Math.max(0, video.volume - 0.05);
  else if (k === "f") Player.toggleFullscreen();
  else if (k === "m") video.muted = !video.muted;
  else if (k === "s" && !$("#p-skip").hidden) Player.skip();
  else if (k === "<" || k === ">") Player.stepRate(k === ">" ? 1 : -1);
  else if (k === "n" && Player.info && Player.info.next_id) { Player.report(); Player.go(Player.info.next_id); }
  else if (k === "Escape" && !document.fullscreenElement) $("#p-back").click();
  else return;
  e.preventDefault();
  Player.poke();
});

if ("mediaSession" in navigator) {
  navigator.mediaSession.setActionHandler("play", () => Player.play());
  navigator.mediaSession.setActionHandler("pause", () => video.pause());
  navigator.mediaSession.setActionHandler("seekforward", () => Player.seek(Player.time() + 10));
  navigator.mediaSession.setActionHandler("seekbackward", () => Player.seek(Player.time() - 10));
  navigator.mediaSession.setActionHandler("nexttrack", () => Player.go(Player.info && Player.info.next_id));
}

window.addEventListener("pagehide", () => { if (Player.active) { Player.report(); beacon(`/api/streams/${Player.sid}/stop`); } });
document.addEventListener("visibilitychange", () => { if (document.hidden && Player.active) Player.report(); });
window.addEventListener("hashchange", route);
route();
