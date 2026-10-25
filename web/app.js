"use strict";

// Live HDHomeRun signal, built for aiming an antenna on a roof: one big
// number, peak hold, trend, a history graph, optional beeps, and a loud
// warning when the data is stale.

const POLL_MS = 500;
const TIMEOUT_MS = 1500;
const STALE_MS = 1500;
const SMOOTH_MS = 2200; // the big number averages this much history
const HISTORY_MS = 15 * 60 * 1000;
const HOLD_MS = 2000;
const UNLOCK_MS = 60 * 1000;

const $ = (sel, root = document) => root.querySelector(sel);

// ---- per-viewer settings (best effort; the page works without storage) ----

function load(store, key, fallback) {
  try {
    const v = store.getItem(key);
    return v === null ? fallback : JSON.parse(v);
  } catch {
    return fallback;
  }
}

function save(store, key, value) {
  try {
    store.setItem(key, JSON.stringify(value));
  } catch {
    // storage full or blocked; settings just won't stick
  }
}

function newClientId() {
  const a = new Uint8Array(8);
  crypto.getRandomValues(a);
  return Array.from(a, (b) => b.toString(16).padStart(2, "0")).join("");
}

// One id per tab, kept across reloads so a refresh keeps the tuner.
const clientId = load(sessionStorage, "hdhr-client", null) || newClientId();
save(sessionStorage, "hdhr-client", clientId);

const prefs = {
  theme: load(localStorage, "hdhr-theme", "auto"),
  vibrate: load(localStorage, "hdhr-vibrate", false),
  awake: load(localStorage, "hdhr-awake", true),
  range: load(localStorage, "hdhr-range", 60),
  allLines: load(localStorage, "hdhr-all-lines", false),
  device: load(localStorage, "hdhr-device", null),
  view: load(localStorage, "hdhr-view", 0),
};

function setPref(key, value) {
  prefs[key] = value;
  save(localStorage, "hdhr-" + key.replace(/[A-Z]/g, (c) => "-" + c.toLowerCase()), value);
}

// ---- state ----

const state = {
  devices: [],
  device: null, // current device object from /api/devices
  lineup: [],
  favorites: [],
  status: null,
  lastOk: 0, // when the last poll succeeded (client clock)
  lastTime: 0, // device read time of the last sample
  pollTimer: null,
  polling: false,
  generation: 0, // bumps on device change, so stale responses are dropped
  history: {}, // tuner -> samples
  prev: {}, // tuner -> {te, t, freq} for error rates
  peak: {}, // tuner -> {freq, value}
  unlockedUntil: {}, // tuner -> ms; a busy tuner may be taken until then
  tuning: {}, // tuner -> ms; shows "Tuning…" rather than "No lock" until then
  sound: false, // always starts off
  soundTuner: 0,
  panels: [], // {tuner, el, actionKey}
  sheetTuner: null,
  infoTuner: null, // tuner shown in the details sheet
};

// ---- API ----

async function api(path, opts = {}) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), opts.timeout || 5000);
  try {
    const resp = await fetch(path, {
      method: opts.method || "GET",
      headers: opts.body ? { "Content-Type": "application/json" } : undefined,
      body: opts.body ? JSON.stringify(opts.body) : undefined,
      cache: "no-store",
      signal: ctl.signal,
    });
    let data = null;
    try {
      data = await resp.json();
    } catch {
      // not JSON (for example a proxy error page)
    }
    if (!resp.ok) {
      const err = new Error((data && data.error) || resp.statusText || "Request failed");
      err.status = resp.status;
      err.data = data;
      throw err;
    }
    return data;
  } finally {
    clearTimeout(timer);
  }
}

const devPath = () => "/api/devices/" + encodeURIComponent(state.device.id);

// ---- polling: the page drives it, one request at a time ----

function schedulePoll(delay) {
  clearTimeout(state.pollTimer);
  state.pollTimer = setTimeout(poll, delay);
}

async function poll() {
  if (document.hidden || !state.device || state.polling) return;
  state.polling = true;
  const gen = state.generation;
  const started = performance.now();
  try {
    const s = await api(devPath() + "/status?client=" + clientId, { timeout: TIMEOUT_MS });
    if (gen === state.generation) applyStatus(s);
  } catch {
    // a lost request is skipped, not retried; the stale warning covers it
  } finally {
    state.polling = false;
  }
  if (gen === state.generation) {
    schedulePoll(Math.max(0, POLL_MS - (performance.now() - started)));
  }
}

function applyStatus(s) {
  state.lastOk = Date.now();
  state.status = s;
  const off = $("#offline");
  if (!s.online) {
    off.textContent = "Can't reach the HDHomeRun: " + (s.error || "no answer");
    off.hidden = false;
    renderAll();
    return;
  }
  off.hidden = true;

  if (s.time !== state.lastTime) {
    // The server's cache can hand back the same reading twice; record each
    // reading once.
    state.lastTime = s.time;
    for (const t of s.tuners) recordSample(t);
  }

  if (state.panels.length === 0 || needsRebuild()) buildPanels();
  renderAll();
}

// ---- history ----

function recordSample(t) {
  const now = Date.now();
  const prev = state.prev[t.index];
  let eps = 0;
  if (prev && prev.freq === t.frequency && t.transportErrors >= prev.te && now > prev.t) {
    eps = (t.transportErrors - prev.te) / ((now - prev.t) / 1000);
  }
  state.prev[t.index] = { te: t.transportErrors, t: now, freq: t.frequency };

  const hist = (state.history[t.index] ||= []);
  const last = hist[hist.length - 1];
  const sample = {
    t: now,
    snq: t.snq,
    ss: t.strength,
    seq: t.symbolQuality,
    locked: t.locked,
    eps,
    freq: t.frequency,
  };
  hist.push(sample);
  while (hist.length && hist[0].t < now - HISTORY_MS) hist.shift();

  // Peak hold per channel: retuning starts a new peak.
  const pk = state.peak[t.index];
  if (!pk || pk.freq !== t.frequency) {
    state.peak[t.index] = { freq: t.frequency, value: t.locked ? t.snq : 0 };
  } else if (t.locked && t.snq > pk.value) {
    pk.value = t.snq;
  }

  if (last && last.locked && !t.locked && last.freq === t.frequency) {
    onLockLost(t.index);
  } else if (last && !last.locked && t.locked && last.freq === t.frequency) {
    onLockGained(t.index);
  }
  if (t.index === state.soundTuner) beepFor(sample);
}

function historyKey() {
  return "hdhr-hist:" + (state.device ? state.device.id : "");
}

function saveHistory() {
  if (!state.device) return;
  save(sessionStorage, historyKey(), { history: state.history, peak: state.peak });
}

function loadHistory() {
  const saved = load(sessionStorage, historyKey(), null);
  const cutoff = Date.now() - HISTORY_MS;
  state.history = {};
  state.peak = {};
  state.prev = {};
  if (saved && saved.history) {
    for (const [k, v] of Object.entries(saved.history)) {
      if (Array.isArray(v)) state.history[k] = v.filter((s) => s.t >= cutoff);
    }
    state.peak = saved.peak || {};
  }
}

// Average of the readings in the last SMOOTH_MS on the current channel:
// readings jump around from sample to sample even with the antenna still.
function smoothed(tuner, field) {
  const hist = state.history[tuner] || [];
  const last = hist[hist.length - 1];
  if (!last) return null;
  let sum = 0;
  let n = 0;
  for (let i = hist.length - 1; i >= 0; i--) {
    const s = hist[i];
    if (s.t < last.t - SMOOTH_MS || s.freq !== last.freq) break;
    if (field !== "eps" && !s.locked) continue;
    sum += s[field];
    n++;
  }
  return n ? sum / n : null;
}

// Rising, falling or steady: the last two seconds against the two before.
function trend(tuner) {
  const hist = state.history[tuner] || [];
  const last = hist[hist.length - 1];
  if (!last || !last.locked) return "";
  const avg = (from, to) => {
    let sum = 0;
    let n = 0;
    for (const s of hist) {
      if (s.t > last.t - from && s.t <= last.t - to && s.locked && s.freq === last.freq) {
        sum += s.snq;
        n++;
      }
    }
    return n ? sum / n : null;
  };
  const now = avg(SMOOTH_MS, -1);
  const before = avg(2 * SMOOTH_MS, SMOOTH_MS);
  if (now === null || before === null) return "";
  if (now - before >= 2) return "up";
  if (now - before <= -2) return "down";
  return "flat";
}

// ---- rendering ----

function quality(v) {
  if (v === null || v === undefined) return "";
  if (v >= 70) return "good";
  if (v >= 50) return "fair";
  return "bad";
}

function symbolQuality(v) {
  if (v === null || v === undefined) return "";
  if (v >= 99.5) return "good";
  if (v >= 90) return "fair";
  return "bad";
}

function tunerCount() {
  if (state.status && state.status.online) return state.status.tuners.length;
  return (state.device && state.device.tunerCount) || 0;
}

function shownTuners() {
  const n = tunerCount();
  if (prefs.view === "both") return Array.from({ length: n }, (_, i) => i);
  const t = Number(prefs.view) || 0;
  return n ? [t < n ? t : 0] : [];
}

function needsRebuild() {
  return shownTuners().join(",") !== state.panels.map((p) => p.tuner).join(",");
}

let resizeObs = null;

function buildPanels() {
  const main = $("#panels");
  main.textContent = "";
  main.classList.toggle("both", prefs.view === "both");
  document.body.classList.toggle("fit", prefs.view !== "both");
  if (resizeObs) resizeObs.disconnect();
  resizeObs = new ResizeObserver(() => {
    for (const p of state.panels) drawGraph(p);
  });

  state.panels = shownTuners().map((tuner) => {
    const el = $("#panelTpl").content.firstElementChild.cloneNode(true);
    el.dataset.tuner = tuner;
    $(".tuner-label", el).textContent = "Tuner " + tuner;
    $(".station-btn", el).addEventListener("click", () => {
      if (guardBusy(tuner)) openSheet(tuner);
    });
    $(".prev", el).addEventListener("click", () => step(tuner, -1));
    $(".next", el).addEventListener("click", () => step(tuner, 1));
    $(".fav-toggle", el).addEventListener("click", () => {
      const t = tunerData(tuner);
      if (t && (t.vchannel || t.rf)) toggleFavorite(currentFavorite(t), t.name);
      else toast("Tune a channel first");
    });
    $(".peak", el).addEventListener("click", () => {
      const pk = state.peak[tuner];
      const cur = smoothed(tuner, "snq");
      if (pk) pk.value = cur === null ? 0 : Math.round(cur);
      toast("Peak reset");
      renderAll();
    });
    for (const b of el.querySelectorAll(".ranges button")) {
      b.addEventListener("click", () => {
        setPref("range", Number(b.dataset.range));
        renderAll();
      });
    }
    $(".info-btn", el).addEventListener("click", () => openInfo(tuner));
    $(".lines-btn", el).addEventListener("click", () => {
      setPref("allLines", !prefs.allLines);
      renderAll();
    });
    // In the dual view, tapping a panel picks which tuner the sound follows.
    el.addEventListener("click", (e) => {
      if (prefs.view !== "both" || e.target.closest("button, summary, details")) return;
      state.soundTuner = tuner;
      if (state.sound) toast("Sound follows tuner " + tuner);
      renderAll();
    });
    main.appendChild(el);
    resizeObs.observe($(".graph", el));
    return { tuner, el, actionKey: "" };
  });
  if (prefs.view !== "both" && state.panels[0]) state.soundTuner = state.panels[0].tuner;
  renderTabs();
}

function renderTabs() {
  const nav = $("#tunerTabs");
  const tuners = state.status && state.status.online ? state.status.tuners : [];
  const count = tunerCount();
  const views = Array.from({ length: count }, (_, i) => String(i));
  if (count > 1) views.push("both");

  if ([...nav.children].map((b) => b.dataset.view).join(",") !== views.join(",")) {
    nav.textContent = "";
    for (const v of views) {
      const b = document.createElement("button");
      b.dataset.view = v;
      b.innerHTML = '<span class="tab-main"></span><span class="tab-sub"></span>';
      b.addEventListener("click", () => {
        setPref("view", v === "both" ? "both" : Number(v));
        buildPanels();
        renderAll();
      });
      nav.appendChild(b);
    }
  }
  for (const b of nav.children) {
    const v = b.dataset.view;
    b.setAttribute("aria-pressed", String(String(prefs.view) === v));
    if (v === "both") {
      $(".tab-main", b).textContent = "Both";
      $(".tab-sub", b).textContent = "two stations";
      continue;
    }
    const t = tuners[Number(v)];
    $(".tab-main", b).textContent = "Tuner " + v;
    $(".tab-sub", b).textContent = !t ? "" : t.locked ? (t.vchannel || "RF " + t.rf) + " · " + tabSymbol(t) : stateLabel(t);
  }
}

// Symbol quality for the tab, averaged like the tiles so it doesn't flicker.
function tabSymbol(t) {
  const v = smoothed(t.index, "seq");
  return Math.round(v === null ? t.symbolQuality : v);
}

function stateLabel(t) {
  switch (t.state) {
    case "yours":
      return "yours";
    case "app":
      return "other viewer";
    case "in_use":
      return "in use";
    default:
      return t.channel === "none" ? "free" : "no signal";
  }
}

function isStale() {
  return !state.lastOk || Date.now() - state.lastOk > STALE_MS || !(state.status && state.status.online);
}

function renderAll() {
  renderTabs();
  for (const p of state.panels) renderPanel(p);
  renderConn();
  const sb = $("#soundBtn");
  sb.setAttribute("aria-pressed", String(state.sound));
  $(".label", sb).textContent = state.sound ? "Sound on" : "Sound";
}

function tunerData(tuner) {
  const s = state.status;
  return s && s.online ? s.tuners[tuner] || null : null;
}

function renderPanel(p) {
  const el = p.el;
  const t = tunerData(p.tuner);
  el.classList.toggle("stale", isStale());
  el.classList.toggle("sound-source", state.sound && state.soundTuner === p.tuner);
  $(".stale-note", el).textContent = staleText();

  const big = $(".big", el);
  const trendEl = $(".trend", el);
  if (!t) {
    big.textContent = "–";
    big.className = "t-val big";
    trendEl.textContent = "";
    for (const m of el.querySelectorAll(".meter i")) m.style.width = "0";
    renderActions(p, null);
    drawGraph(p);
    return;
  }

  const busyLocked = isBusy(t) && !isUnlocked(p.tuner);
  $(".vch", el).textContent = t.vchannel || "";
  $(".aff", el).textContent = (chanInfo(t.vchannel) || {}).affiliate || "";
  $(".name", el).textContent = t.name || (t.channel === "none" ? "Not tuned" : t.vchannel ? "" : "Unknown station");
  $(".rf", el).textContent = t.rf ? "RF " + t.rf : t.frequency ? (t.frequency / 1e6).toFixed(0) + " MHz" : "";
  $(".hint", el).textContent = busyLocked ? "locked" : t.channel === "none" ? "tap to pick a channel" : "change";
  el.classList.toggle("busy-locked", busyLocked);

  const favBtn = $(".fav-toggle", el);
  const starred = !!(t.vchannel || t.rf) && state.favorites.some((f) => sameFav(f, currentFavorite(t)));
  favBtn.setAttribute("aria-pressed", String(starred));
  $(".label", favBtn).textContent = starred ? "Favorite" : "Save";
  favBtn.title = starred ? "Remove from favorites" : "Save as favorite";
  favBtn.hidden = !(t.vchannel || t.rf);
  renderFavStrip(p, t);

  const user = $(".user", el);
  const [label, cls] =
    t.state === "yours"
      ? ["Yours", "mine"]
      : t.state === "app"
        ? ["Another viewer", "busy"]
        : t.state === "in_use"
          ? ["In use · " + who(t), "busy"]
          : ["Free", "free"];
  user.textContent = label;
  user.className = "user badge " + cls;

  const snq = smoothed(p.tuner, "snq");
  const shown = snq === null ? t.snq : snq;
  if (!t.locked) {
    const tuning = t.channel !== "none" && (state.tuning[p.tuner] || 0) > Date.now();
    // The station header already says "Not tuned".
    big.textContent = t.channel === "none" ? "–" : tuning ? "Tuning…" : "No lock";
    big.className = t.channel === "none" ? "t-val big" : tuning ? "t-val big nolock" : "t-val big nolock bad";
    trendEl.textContent = "";
    meter(el, "snq", null);
  } else {
    big.textContent = Math.round(shown);
    big.className = "t-val big " + quality(shown);
    const tr = trend(p.tuner);
    trendEl.textContent = tr === "up" ? "▲" : tr === "down" ? "▼" : tr === "flat" ? "▬" : "";
    trendEl.className = "trend " + (tr === "up" ? "good" : tr === "down" ? "bad" : tr === "flat" ? "flat" : "");
    meter(el, "snq", shown, quality(shown));
  }

  const pk = state.peak[p.tuner];
  const peakText = $(".peak-text", el);
  $(".peak", el).hidden = !(pk && pk.freq === t.frequency && pk.value > 0 && t.locked);
  const peakMark = $(".m-peak", el);
  peakText.textContent = "";
  if (pk && pk.freq === t.frequency && pk.value > 0 && t.locked) {
    const diff = Math.round(shown - pk.value);
    const note = document.createElement("span");
    note.className = diff < 0 ? "below" : "at";
    note.textContent = diff < 0 ? " " + diff : " ✓";
    peakText.append(String(pk.value), note);
    peakMark.style.display = "block";
    peakMark.style.left = pk.value + "%";
  } else {
    peakText.textContent = "–";
    peakMark.style.display = "none";
  }

  const ss = smoothed(p.tuner, "ss");
  const seq = smoothed(p.tuner, "seq");
  const eps = smoothed(p.tuner, "eps");
  const ssVal = t.locked ? Math.round(ss === null ? t.strength : ss) : t.strength || null;
  const seqVal = t.locked ? Math.round(seq === null ? t.symbolQuality : seq) : null;
  setMetric($(".ss", el), ssVal, quality);
  setMetric($(".seq", el), seqVal, symbolQuality);
  meter(el, "ss", ssVal, ssVal === null ? "" : quality(ssVal));
  meter(el, "seq", seqVal, seqVal === null ? "" : symbolQuality(seqVal));
  const epsEl = $(".eps", el);
  if (t.locked && eps !== null) {
    const grade = eps === 0 ? "good" : eps < 5 ? "fair" : "bad";
    epsEl.textContent = eps >= 10 ? Math.round(eps) : eps.toFixed(1);
    epsEl.className = "t-val eps " + grade;
    // Log scale: 1/s fills a third, 100/s and up fills it.
    meter(el, "eps", eps === 0 ? 0 : Math.min(100, (Math.log10(eps + 1) / 2) * 100), grade);
  } else {
    epsEl.textContent = "–";
    epsEl.className = "t-val eps";
    meter(el, "eps", null);
  }

  for (const b of el.querySelectorAll(".ranges button")) {
    b.setAttribute("aria-pressed", String(Number(b.dataset.range) === prefs.range));
  }
  $(".lines-btn", el).setAttribute("aria-pressed", String(prefs.allLines));

  if (state.infoTuner === p.tuner && !$("#info").hidden) renderDetails($("#info"), t);
  renderActions(p, t);
  drawGraph(p);
}

function setMetric(el, v, grade) {
  el.textContent = v === null ? "–" : v;
  el.className = el.className.split(" ").slice(0, 2).join(" ") + " " + (v === null ? "" : grade(v));
}

// meter sets the bar under a tile: v is 0–100, or null for empty.
function meter(el, name, v, grade = "") {
  const bar = $(".m-" + name, el);
  bar.style.width = (v === null ? 0 : Math.max(0, Math.min(100, v))) + "%";
  bar.className = "m-" + name + " " + grade;
}

function who(t) {
  if (t.lockedBy && t.lockedBy !== "none") return t.lockedBy;
  const m = /\/\/([^:/]+)/.exec(t.target || "");
  return m ? m[1] : "another device";
}

function staleText() {
  if (state.status && !state.status.online) return "HDHomeRun not answering";
  if (!state.lastOk) return "Connecting…";
  const age = (Date.now() - state.lastOk) / 1000;
  return "No update for " + age.toFixed(1) + " s: don't trust this reading";
}

function renderDetails(el, t) {
  const rows = [
    ["Lock", t.lock],
    ["Channel", t.channel],
    ["Frequency", t.frequency ? (t.frequency / 1e6).toFixed(3) + " MHz" : "–"],
    ["Strength", t.strength + "%"],
    ["SNR quality", t.snq + "%"],
    ["Symbol quality", t.symbolQuality + "%"],
    ["Stream", (t.streamBps / 1e6).toFixed(2) + " Mbps"],
    ["Raw", (t.deviceBps / 1e6).toFixed(2) + " Mbps"],
    ["Transport errors", t.transportErrors],
    ["CRC errors", t.crcErrors],
    ["Streaming to", t.target],
    ["Locked by", t.lockedBy],
  ];
  const dl = $(".details", el);
  if (dl.childElementCount !== rows.length * 2) {
    dl.textContent = "";
    for (const [k] of rows) {
      const dt = document.createElement("dt");
      dt.textContent = k;
      dl.append(dt, document.createElement("dd"));
    }
  }
  rows.forEach(([, v], i) => {
    dl.children[i * 2 + 1].textContent = v;
  });
}

// ---- actions: change channel, release, take over, favorites ----

function isBusy(t) {
  return !!t && (t.state === "in_use" || t.state === "app");
}

function isUnlocked(tuner) {
  return (state.unlockedUntil[tuner] || 0) > Date.now();
}

function freeTuner() {
  const s = state.status;
  if (!s || !s.online) return null;
  const t = s.tuners.find((x) => x.state === "free" && x.channel === "none") || s.tuners.find((x) => x.state === "free");
  return t ? t.index : null;
}

function unlockLeft(tuner) {
  return Math.max(0, Math.ceil((state.unlockedUntil[tuner] - Date.now()) / 1000)) + " s";
}

// Rebuilt only when what it shows changes, so a status update can't cut off
// a press-and-hold in progress. Changing channel itself is in the station
// header (tap, or ◀ ▶) and the favorites strip.
function renderActions(p, t) {
  const busy = isBusy(t);
  const unlocked = busy && isUnlocked(p.tuner);
  const free = busy && !unlocked ? freeTuner() : null;
  const key = [t ? t.state : "none", unlocked, free].join("|");

  const box = $(".actions", p.el);
  if (key === p.actionKey) {
    const left = $(".unlock-left", box);
    if (left) left.textContent = unlockLeft(p.tuner);
    return;
  }
  p.actionKey = key;
  box.textContent = "";
  if (!t) return;

  const button = (label, cls, onClick) => {
    const b = document.createElement("button");
    b.className = cls;
    const span = document.createElement("span");
    span.textContent = label;
    b.appendChild(span);
    if (onClick) b.addEventListener("click", onClick);
    box.appendChild(b);
    return b;
  };

  if (unlocked) {
    const b = button("Pick channel (", "danger", () => openSheet(p.tuner));
    const left = document.createElement("span");
    left.className = "unlock-left";
    left.textContent = unlockLeft(p.tuner);
    b.append(left, document.createTextNode(")"));
  } else if (busy) {
    if (free !== null) {
      button("Use tuner " + free, "primary", () => {
        if (prefs.view !== "both") setPref("view", free);
        buildPanels();
        renderAll();
        openSheet(free);
      });
    }
    holdButton(box, "Hold to take over", () => {
      state.unlockedUntil[p.tuner] = Date.now() + UNLOCK_MS;
      vibrate(60);
      toast("Unlocked for 60 s. Changing the channel will cut off " + (t.state === "app" ? "the other viewer" : who(t)) + ".");
      p.actionKey = "";
      renderAll();
      openSheet(p.tuner);
    });
  }
  if (t.state === "yours") {
    button("Release tuner " + p.tuner, "", () => release(p.tuner));
  }
}

function renderFavStrip(p, t) {
  const strip = $(".fav-strip", p.el);
  const key = state.favorites.map((f) => favLabel(f) + "=" + favName(f)).join(",") + "|" + t.vchannel + "|" + t.rf;
  if (strip.dataset.key === key) return;
  strip.dataset.key = key;
  strip.textContent = "";
  for (const f of state.favorites) {
    const b = document.createElement("button");
    if (f.vchannel ? f.vchannel === t.vchannel : f.rf === t.rf) b.className = "current";
    const num = document.createElement("b");
    num.textContent = favLabel(f);
    b.append(num, favName(f));
    b.addEventListener("click", () => {
      if (guardBusy(p.tuner)) tune(p.tuner, f.vchannel ? { vchannel: f.vchannel } : { rf: f.rf });
    });
    strip.appendChild(b);
  }
  const cur = $(".current", strip);
  if (cur) cur.scrollIntoView({ block: "nearest", inline: "nearest" });
}

// guardBusy reports whether the tuner may be changed now, and explains why
// not when it can't.
function guardBusy(tuner) {
  if (!isBusy(tunerData(tuner)) || isUnlocked(tuner)) return true;
  toast("Tuner " + tuner + " is in use. Hold “take over” first, or use a free tuner.");
  return false;
}

// The stations ◀ ▶ step through: favorites when there are at least two,
// otherwise every station in the lineup.
function stepList() {
  if (state.favorites.length >= 2) return state.favorites.map((f) => (f.vchannel ? { vchannel: f.vchannel } : { rf: f.rf }));
  return channelGroups().map((g) => ({ vchannel: g.chans[0].number }));
}

function step(tuner, dir) {
  if (!guardBusy(tuner)) return;
  const list = stepList();
  if (!list.length) {
    toast("No channels to step through. Run a channel scan in the HDHomeRun app.");
    return;
  }
  const t = tunerData(tuner);
  const major = (v) => (v || "").split(".")[0];
  let i = -1;
  if (t) {
    i = list.findIndex((x) => (x.vchannel ? x.vchannel === t.vchannel : x.rf === t.rf));
    if (i < 0) i = list.findIndex((x) => x.vchannel && t.vchannel && major(x.vchannel) === major(t.vchannel));
  }
  const next = i < 0 ? (dir > 0 ? 0 : list.length - 1) : (i + dir + list.length) % list.length;
  tune(tuner, list[next]);
}

function holdButton(box, label, onDone) {
  const b = document.createElement("button");
  b.className = "hold danger";
  b.style.setProperty("--hold-ms", HOLD_MS + "ms");
  const span = document.createElement("span");
  span.textContent = label;
  b.appendChild(span);
  let timer = null;
  let done = false;
  const cancel = () => {
    clearTimeout(timer);
    timer = null;
    b.classList.remove("holding");
  };
  b.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    done = false;
    try {
      b.setPointerCapture(e.pointerId);
    } catch {
      // not supported; pointerleave isn't watched, so the hold still works
    }
    b.classList.add("holding");
    timer = setTimeout(() => {
      timer = null;
      done = true;
      b.classList.remove("holding");
      onDone();
    }, HOLD_MS);
  });
  for (const ev of ["pointerup", "pointercancel", "lostpointercapture"]) b.addEventListener(ev, cancel);
  b.addEventListener("contextmenu", (e) => e.preventDefault());
  b.addEventListener("click", () => {
    if (!done) toast("Press and hold for 2 seconds");
  });
  box.appendChild(b);
}

async function tune(tuner, target) {
  const force = isBusy(tunerData(tuner)) && isUnlocked(tuner);
  try {
    const s = await api(devPath() + "/tuners/" + tuner + "/tune", {
      method: "POST",
      body: { client: clientId, force, ...target },
    });
    delete state.unlockedUntil[tuner];
    state.tuning[tuner] = Date.now() + 4000;
    closeSheets();
    applyStatus(s);
  } catch (err) {
    if (err.status === 409) {
      toast("Tuner " + tuner + " is in use. Hold “take over” to interrupt it.");
    } else {
      toast("Couldn't tune: " + err.message);
    }
  }
}

async function release(tuner) {
  try {
    applyStatus(await api(devPath() + "/tuners/" + tuner + "/release", { method: "POST", body: { client: clientId } }));
    toast("Tuner " + tuner + " released");
  } catch (err) {
    toast("Couldn't release: " + err.message);
  }
}

function currentFavorite(t) {
  if (t.vchannel && state.lineup.some((c) => c.number === t.vchannel)) return { vchannel: t.vchannel };
  return t.rf ? { rf: t.rf } : { vchannel: t.vchannel };
}

function sameFav(a, b) {
  return (a.vchannel || "") === (b.vchannel || "") && (a.rf || 0) === (b.rf || 0);
}

function favLabel(f) {
  return f.vchannel || "RF " + f.rf;
}

function favName(f) {
  if (f.label) return f.label;
  const c = chanInfo(f.vchannel);
  return c ? c.affiliate || c.name : "";
}

// chanInfo returns the lineup entry for a virtual channel such as "13.1".
function chanInfo(vchannel) {
  return (vchannel && state.lineup.find((c) => c.number === vchannel)) || null;
}

async function toggleFavorite(fav, name) {
  const exists = state.favorites.some((f) => sameFav(f, fav));
  const added = { ...fav };
  if (fav.rf && name) added.label = name;
  const next = exists ? state.favorites.filter((f) => !sameFav(f, fav)) : [...state.favorites, added];
  try {
    state.favorites = await api(devPath() + "/favorites", { method: "PUT", body: next });
    toast(exists ? "Removed from favorites" : "Saved to favorites");
    renderAll();
    if (!$("#sheet").hidden) renderChannels();
  } catch (err) {
    toast("Couldn't save favorites: " + err.message);
  }
}

// ---- channel sheet ----

// Subchannels of one station (13.1, 13.2…) share a transmitter, so they share
// a signal: list each station once.
function channelGroups() {
  const minor = (c) => parseFloat(c.number.split(".")[1] || 0);
  const groups = new Map();
  for (const c of state.lineup) {
    const major = c.number.split(".")[0];
    if (!groups.has(major)) groups.set(major, []);
    groups.get(major).push(c);
  }
  return [...groups.entries()]
    .map(([major, chans]) => ({ major, chans: chans.sort((a, b) => minor(a) - minor(b)) }))
    .sort((a, b) => parseFloat(a.major) - parseFloat(b.major));
}

function openSheet(tuner) {
  state.sheetTuner = tuner;
  const t = tunerData(tuner);
  $("#sheetTitle").textContent = "Tune tuner " + tuner;
  const warn = $("#sheetWarn");
  if (isBusy(t) && isUnlocked(tuner)) {
    warn.textContent = "This will cut off " + (t.state === "app" ? "another viewer" : who(t)) + ".";
    warn.hidden = false;
  } else {
    warn.hidden = true;
  }
  $("#search").value = "";
  renderChannels();
  $("#sheet").hidden = false;
  // RF channels are learned as tuners visit stations; pick up new ones.
  const gen = state.generation;
  api(devPath() + "/lineup").then(
    (l) => {
      if (gen === state.generation && !$("#sheet").hidden) {
        state.lineup = l;
        renderChannels();
      }
    },
    () => {},
  );
}

function renderChannels() {
  const ul = $("#channels");
  const q = $("#search").value.trim().toLowerCase();
  const t = tunerData(state.sheetTuner);
  ul.textContent = "";

  const matches = (g) =>
    !q ||
    g.major.startsWith(q) ||
    g.chans.some(
      (c) => c.number.startsWith(q) || c.name.toLowerCase().includes(q) || (c.affiliate || "").toLowerCase().includes(q),
    );
  const groups = channelGroups().filter(matches);
  const favSet = new Set(state.favorites.filter((f) => f.vchannel).map((f) => f.vchannel));
  const favGroups = groups.filter((g) => g.chans.some((c) => favSet.has(c.number)));
  const rfFavs = q ? [] : state.favorites.filter((f) => f.rf);

  if (!groups.length && !rfFavs.length) {
    const li = document.createElement("li");
    li.className = "note";
    li.textContent = state.lineup.length ? "No matching channels" : "No channel lineup. Run a channel scan in the HDHomeRun app.";
    ul.appendChild(li);
    return;
  }

  const head = (text) => {
    const li = document.createElement("li");
    li.className = "group-head";
    li.textContent = text;
    ul.appendChild(li);
  };
  // While searching, list each match once rather than under both headings.
  if (!q && (favGroups.length || rfFavs.length)) {
    head("Favorites");
    for (const g of favGroups) ul.appendChild(stationRow(g, t, g.chans.find((c) => favSet.has(c.number))));
    for (const f of rfFavs) ul.appendChild(rfRow(f, t));
    head("All stations");
  }
  for (const g of groups) ul.appendChild(stationRow(g, t, null));
}

// One row per station: tapping tunes its first subchannel (they all share the
// station's signal).
function stationRow(g, t, favChan) {
  const target = favChan || g.chans[0];
  const li = document.createElement("li");
  li.className = "channel-row";

  const b = document.createElement("button");
  b.className = "ch-btn";
  if (t && t.vchannel && t.vchannel.split(".")[0] === g.major) b.classList.add("current");
  const major = document.createElement("span");
  major.className = "major";
  major.textContent = g.major;
  // The network ("FOX") is what people recognize; the call sign follows.
  const name = document.createElement("span");
  name.className = "ch-name";
  if (target.affiliate) {
    const aff = document.createElement("b");
    aff.textContent = target.affiliate;
    const call = document.createElement("span");
    call.className = "call";
    call.textContent = target.name;
    name.append(aff, call);
  } else {
    name.textContent = target.name;
  }
  const side = document.createElement("span");
  side.className = "ch-side";
  if (target.logo) {
    const img = document.createElement("img");
    img.className = "ch-logo";
    img.src = target.logo;
    img.alt = "";
    img.loading = "lazy";
    img.decoding = "async";
    img.addEventListener("error", () => img.remove());
    side.append(img);
  }
  const known = g.chans.find((c) => c.rf);
  if (known) {
    const rf = document.createElement("span");
    rf.className = "ch-rf";
    rf.textContent = "RF " + known.rf;
    side.append(rf);
  }
  b.append(major, name, side);
  const others = g.chans.filter((c) => c !== target);
  if (others.length) {
    const subs = document.createElement("span");
    subs.className = "subs";
    subs.textContent = others.map((c) => c.number + " " + (c.affiliate || c.name)).join(" · ");
    b.append(subs);
  }
  b.addEventListener("click", () => tune(state.sheetTuner, { vchannel: target.number }));

  li.append(b, starButton({ vchannel: target.number }, target.number));
  return li;
}

function rfRow(f, t) {
  const li = document.createElement("li");
  li.className = "channel-row";
  const b = document.createElement("button");
  b.className = "ch-btn";
  if (t && t.rf === f.rf) b.classList.add("current");
  const major = document.createElement("span");
  major.className = "major";
  major.textContent = "RF";
  const name = document.createElement("span");
  name.className = "ch-name";
  name.textContent = f.rf + (f.label ? " · " + f.label : "");
  b.append(major, name);
  b.addEventListener("click", () => tune(state.sheetTuner, { rf: f.rf }));
  li.append(b, starButton({ rf: f.rf }, "RF " + f.rf));
  return li;
}

function starButton(fav, label) {
  const starred = state.favorites.some((f) => sameFav(f, fav));
  const star = document.createElement("button");
  star.className = "star-btn";
  star.setAttribute("aria-label", starred ? "Remove " + label + " from favorites" : "Save " + label + " as favorite");
  star.setAttribute("aria-pressed", String(starred));
  star.textContent = starred ? "★" : "☆";
  star.addEventListener("click", () => toggleFavorite(fav));
  return star;
}

function closeSheets() {
  $("#sheet").hidden = true;
  $("#menu").hidden = true;
  $("#info").hidden = true;
}

function openInfo(tuner) {
  state.infoTuner = tuner;
  $("#infoTitle").textContent = "Tuner " + tuner + " details";
  const t = tunerData(tuner);
  if (t) renderDetails($("#info"), t);
  else $("#infoDetails").textContent = "No data yet";
  $("#info").hidden = false;
}

// ---- graph ----

let colors = null;
function themeColors() {
  if (!colors) {
    const cs = getComputedStyle(document.documentElement);
    const v = (n) => cs.getPropertyValue(n).trim();
    colors = { text: v("--text"), muted: v("--muted"), grid: v("--grid"), lost: v("--lost"), fair: v("--fair"), accent: v("--accent") };
  }
  return colors;
}

// The SNR axis zooms to the readings (at least 10 points tall) so a change of
// a few points while turning the antenna stands out. It only moves when a
// reading falls outside it or the readings have settled into a much smaller
// band, so it doesn't jump around while you watch.
function snrScale(p, hist, peak) {
  let lo = Infinity;
  let hi = -Infinity;
  for (const s of hist) {
    if (!s.locked) continue;
    lo = Math.min(lo, s.snq);
    hi = Math.max(hi, s.snq);
  }
  if (peak !== null) hi = Math.max(hi, peak);
  if (lo > hi) return { lo: 0, hi: 100 };

  const cur = p.scale;
  const fits = cur && lo >= cur.lo && hi <= cur.hi;
  const tooBig = cur && cur.hi - cur.lo > 2 * Math.max(10, hi - lo + 6);
  if (fits && !tooBig) return cur;

  let nlo = Math.max(0, Math.floor((lo - 3) / 5) * 5);
  let nhi = Math.min(100, Math.ceil((hi + 3) / 5) * 5);
  while (nhi - nlo < 10) {
    if (nlo > 0) nlo -= 5;
    if (nhi - nlo < 10 && nhi < 100) nhi += 5;
  }
  p.scale = { lo: nlo, hi: nhi };
  return p.scale;
}

function drawGraph(p) {
  const canvas = $(".graph", p.el);
  const w = canvas.clientWidth;
  const h = canvas.clientHeight;
  if (!w || !h) return;
  const dpr = window.devicePixelRatio || 1;
  if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(h * dpr);
  }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);

  const c = themeColors();
  const padL = 26; // SNR scale
  const padR = 28; // symbol quality (and strength) scale, 0–100
  const padT = 20; // legend
  const padB = 16; // time labels
  const gw = w - padL - padR;
  const gh = h - padT - padB;
  const now = Date.now();
  const range = prefs.range * 1000;
  const x = (t) => padL + ((t - (now - range)) / range) * gw;
  const hist = (state.history[p.tuner] || []).filter((s) => s.t >= now - range - 2000);
  const pk = state.peak[p.tuner];
  const last = hist[hist.length - 1];
  const showPeak = pk && last && pk.value > 0 && pk.freq === last.freq;
  if (last && p.scaleFreq !== last.freq) {
    p.scale = null; // new channel, new zoom
    p.scaleFreq = last.freq;
  }

  const { lo, hi } = snrScale(p, hist, showPeak ? pk.value : null);
  const yL = (v) => padT + gh - ((Math.max(lo, Math.min(hi, v)) - lo) / (hi - lo)) * gh;
  const yR = (v) => padT + gh - (Math.max(0, Math.min(100, v)) / 100) * gh;

  // Grid follows the SNR scale; the right axis labels its own 0–100.
  ctx.font = "11px system-ui, sans-serif";
  ctx.lineWidth = 1;
  ctx.strokeStyle = c.grid;
  ctx.fillStyle = c.text;
  const span = hi - lo;
  // As many labels as fit at ~22px apart, on round steps.
  const maxLabels = Math.max(2, Math.floor(gh / 22));
  const yStep = [1, 2, 5, 10, 20, 25, 50].find((st) => span / st <= maxLabels) || 50;
  for (let v = Math.ceil(lo / yStep) * yStep; v <= hi; v += yStep) {
    ctx.beginPath();
    ctx.moveTo(padL, yL(v));
    ctx.lineTo(padL + gw, yL(v));
    ctx.stroke();
    ctx.fillText(String(v), 2, yL(v) + 4);
  }
  ctx.fillStyle = c.fair;
  ctx.textAlign = "right";
  for (const v of [0, 50, 100]) ctx.fillText(String(v), w - 2, yR(v) + 4);

  ctx.fillStyle = c.muted;
  ctx.textAlign = "center";
  const step = prefs.range <= 60 ? 15 : prefs.range <= 300 ? 60 : 300;
  for (let s = step; s < prefs.range; s += step) {
    const gx = x(now - s * 1000);
    ctx.beginPath();
    ctx.moveTo(gx, padT);
    ctx.lineTo(gx, padT + gh);
    ctx.stroke();
    ctx.fillText("-" + (s >= 60 ? s / 60 + "m" : s + "s"), gx, h - 3);
  }

  // Legend across the top: each line and which side its scale is on.
  const legend = [
    ["◂ SNR", c.text, 2.5, []],
    ["Symbol ▸", c.fair, 2, []],
  ];
  if (prefs.allLines) legend.push(["Strength ▸", c.muted, 1.5, [4, 3]]);
  ctx.font = "600 11px system-ui, sans-serif";
  ctx.textAlign = "left";
  let lx = padL;
  for (const [label, color, width, dash] of legend) {
    ctx.strokeStyle = color;
    ctx.lineWidth = width;
    ctx.setLineDash(dash);
    ctx.beginPath();
    ctx.moveTo(lx, 9);
    ctx.lineTo(lx + 14, 9);
    ctx.stroke();
    ctx.setLineDash([]);
    ctx.fillStyle = color;
    ctx.fillText(label, lx + 18, 13);
    lx += 18 + ctx.measureText(label).width + 14;
  }

  if (!hist.length) return;
  ctx.save();
  ctx.beginPath();
  ctx.rect(padL, padT - 2, gw, gh + 4);
  ctx.clip();

  // Shade where the tuner had no lock, and mark retunes.
  ctx.fillStyle = c.lost;
  for (let i = 0; i < hist.length; i++) {
    if (hist[i].locked || hist[i].freq === 0) continue;
    const end = hist[i + 1] ? hist[i + 1].t : hist[i].t + POLL_MS;
    ctx.fillRect(x(hist[i].t), padT, Math.max(1, x(end) - x(hist[i].t)), gh);
  }
  ctx.strokeStyle = c.muted;
  ctx.lineWidth = 1;
  ctx.setLineDash([2, 3]);
  for (let i = 1; i < hist.length; i++) {
    if (hist[i].freq !== hist[i - 1].freq) {
      ctx.beginPath();
      ctx.moveTo(x(hist[i].t), padT);
      ctx.lineTo(x(hist[i].t), padT + gh);
      ctx.stroke();
    }
  }

  // Peak SNR for the current channel.
  if (showPeak) {
    ctx.strokeStyle = c.accent;
    ctx.lineWidth = 1.5;
    ctx.setLineDash([6, 4]);
    ctx.beginPath();
    ctx.moveTo(padL, yL(pk.value));
    ctx.lineTo(padL + gw, yL(pk.value));
    ctx.stroke();
  }

  const line = (field, y, color, width, dash) => {
    ctx.strokeStyle = color;
    ctx.lineWidth = width;
    ctx.setLineDash(dash || []);
    ctx.beginPath();
    let open = false;
    for (let i = 0; i < hist.length; i++) {
      const s = hist[i];
      // Break the line across gaps (page hidden, network loss) and no-lock
      // stretches.
      if (!s.locked || (i > 0 && s.t - hist[i - 1].t > 3000)) {
        open = false;
        if (!s.locked) continue;
      }
      if (open) ctx.lineTo(x(s.t), y(s[field]));
      else ctx.moveTo(x(s.t), y(s[field]));
      open = true;
    }
    ctx.stroke();
  };
  if (prefs.allLines) line("ss", yR, c.muted, 1.5, [4, 3]);
  line("seq", yR, c.fair, 2);
  line("snq", yL, c.text, 3);
  ctx.restore();
  ctx.setLineDash([]);
  ctx.textAlign = "left";
}

// ---- connection dot ----

function renderConn() {
  const dot = $("#conn");
  const age = Date.now() - state.lastOk;
  const down = state.status && !state.status.online;
  dot.className = "conn " + (!state.lastOk ? "" : down ? "down" : age < 1200 ? "ok" : age < 3000 ? "slow" : "down");
  $("#connText").textContent = !state.lastOk
    ? "Connecting…"
    : down
      ? "HDHomeRun not answering"
      : age < 1200
        ? "Live · " + (state.device ? state.device.host : "")
        : "No update for " + Math.round(age / 1000) + " s";
}

// ---- sound and vibration ----

let audio = null;

function beep(freq, ms, type = "sine", volume = 0.25) {
  if (!audio) return;
  const t0 = audio.currentTime;
  const osc = audio.createOscillator();
  const gain = audio.createGain();
  osc.type = type;
  osc.frequency.value = freq;
  gain.gain.setValueAtTime(0.0001, t0);
  gain.gain.exponentialRampToValueAtTime(volume, t0 + 0.01);
  gain.gain.exponentialRampToValueAtTime(0.0001, t0 + ms / 1000);
  osc.connect(gain).connect(audio.destination);
  osc.start(t0);
  osc.stop(t0 + ms / 1000 + 0.02);
}

let lostBeeps = 0;
function beepFor(sample) {
  if (!state.sound || !audio || sample.freq === 0) return;
  if (!sample.locked) {
    // A low buzz once a second while there's no lock.
    if (++lostBeeps % 2 === 0) beep(140, 160, "square", 0.12);
    return;
  }
  lostBeeps = 0;
  // 300 Hz at 0 up to about 2.4 kHz at 100: higher pitch, better signal.
  beep(300 * Math.pow(2, sample.snq / 33), 70);
}

function toggleSound() {
  state.sound = !state.sound;
  if (state.sound) {
    if (!audio) {
      const Ctx = window.AudioContext || window.webkitAudioContext;
      audio = Ctx ? new Ctx() : null;
    }
    if (!audio) {
      state.sound = false;
      toast("This browser can't play sounds");
    } else if (audio.state === "suspended") {
      audio.resume();
    }
  }
  renderAll();
}

function vibrate(pattern) {
  if (prefs.vibrate && navigator.vibrate) navigator.vibrate(pattern);
}

function onLockLost(tuner) {
  if (!shownTuners().includes(tuner)) return;
  vibrate([300, 120, 300]);
  if (state.sound && tuner === state.soundTuner) beep(110, 400, "square", 0.2);
}

function onLockGained(tuner) {
  if (shownTuners().includes(tuner)) vibrate(80);
}

// ---- keep the screen on ----

let wakeLock = null;

async function updateWake() {
  const note = $("#awakeNote");
  const video = $("#keepAwake");
  if (!prefs.awake || document.hidden) {
    if (wakeLock) wakeLock.release().catch(() => {});
    wakeLock = null;
    video.pause();
    note.hidden = true;
    return;
  }
  if ("wakeLock" in navigator && window.isSecureContext) {
    try {
      if (!wakeLock) {
        wakeLock = await navigator.wakeLock.request("screen");
        wakeLock.addEventListener("release", () => {
          wakeLock = null;
        });
      }
      note.hidden = true;
      return;
    } catch {
      // fall back to the video
    }
  }
  // Without HTTPS there's no wake lock API; a silent looping video keeps
  // Android awake. Browsers only start it from a tap.
  video.play().then(
    () => {
      note.textContent = "Keeping the screen on with a silent video (the page isn't on HTTPS).";
      note.hidden = false;
    },
    () => {
      note.textContent = "Tap anywhere on the page to keep the screen on.";
      note.hidden = false;
    }
  );
}

// ---- menus, theme, toast ----

let toastTimer = null;
function toast(msg, ms = 3500) {
  const el = $("#toast");
  el.textContent = msg;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    el.hidden = true;
  }, ms);
}

function applyTheme() {
  if (prefs.theme === "auto") delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = prefs.theme;
  for (const b of $("#themeSeg").children) b.setAttribute("aria-pressed", String(b.dataset.theme === prefs.theme));
  const bg = getComputedStyle(document.documentElement).getPropertyValue("--bg").trim();
  $('meta[name="theme-color"]').setAttribute("content", bg);
  colors = null;
  for (const p of state.panels) drawGraph(p);
}

function renderDeviceInfo() {
  const d = state.device;
  const dl = $("#deviceInfo");
  dl.textContent = "";
  if (!d) return;
  for (const [k, v] of [
    ["Device", d.name || "–"],
    ["Model", d.model || "–"],
    ["Firmware", d.firmware || "–"],
    ["Device ID", d.deviceId || "–"],
    ["Address", d.host],
    ["Tuners", d.tunerCount || "–"],
  ]) {
    const dt = document.createElement("dt");
    dt.textContent = k;
    const dd = document.createElement("dd");
    dd.textContent = v;
    dl.append(dt, dd);
  }
}

// ---- devices ----

function selectDevice(id) {
  saveHistory();
  state.generation++;
  clearTimeout(state.pollTimer);
  state.device = state.devices.find((d) => d.id === id) || state.devices[0];
  setPref("device", state.device.id);
  state.status = null;
  state.lastOk = 0;
  state.lastTime = 0;
  state.unlockedUntil = {};
  state.lineup = [];
  state.favorites = [];
  loadHistory();
  $("#deviceName").textContent = state.devices.length > 1 ? "" : shortName(state.device);
  $("#device").value = state.device.id;
  renderDeviceInfo();
  buildPanels();
  renderAll();

  const gen = state.generation;
  const refresh = () => {
    for (const p of state.panels) p.actionKey = "";
    renderAll();
    if (!$("#sheet").hidden) renderChannels();
  };
  api(devPath() + "/lineup").then(
    (l) => {
      if (gen === state.generation) {
        state.lineup = l;
        refresh();
      }
    },
    () => {}
  );
  api(devPath() + "/favorites").then(
    (f) => {
      if (gen === state.generation) {
        state.favorites = f;
        refresh();
      }
    },
    () => {}
  );
  poll();
}

// "HDHomeRun FLEX DUO" → "FLEX DUO": the app is all HDHomeRun.
function shortName(d) {
  return (d.name || "").replace(/^HDHomeRun\s+/i, "") || d.name || d.host;
}

async function loadDevices() {
  try {
    state.devices = await api("/api/devices");
  } catch (err) {
    $("#offline").textContent = "Can't reach the monitor: " + err.message;
    $("#offline").hidden = false;
    setTimeout(loadDevices, 3000);
    return;
  }
  const sel = $("#device");
  sel.textContent = "";
  for (const d of state.devices) {
    const o = document.createElement("option");
    o.value = d.id;
    o.textContent = shortName(d) + " · " + d.host + (d.online ? "" : " (offline)");
    sel.appendChild(o);
  }
  sel.hidden = state.devices.length < 2;
  if (!state.devices.length) {
    $("#offline").textContent = "No devices configured. Set HDHR_DEVICES.";
    $("#offline").hidden = false;
    return;
  }
  $("#offline").hidden = true;
  selectDevice(prefs.device && state.devices.some((d) => d.id === prefs.device) ? prefs.device : state.devices[0].id);
}

// ---- updates: reload when the server runs a new version ----

let appVersion = null;

async function checkVersion() {
  try {
    const { version } = await api("/api/version");
    if (appVersion === null) {
      appVersion = version;
      $("#version").textContent = version;
    } else if (version !== appVersion) {
      saveHistory();
      location.reload();
    }
  } catch {
    // server restarting; try again later
  }
}

// ---- wiring ----

function init() {
  applyTheme();
  $("#soundBtn").addEventListener("click", toggleSound);
  $("#menuBtn").addEventListener("click", () => {
    $("#vibrateOpt").checked = prefs.vibrate;
    $("#awakeOpt").checked = prefs.awake;
    $("#menu").hidden = false;
  });
  $("#device").addEventListener("change", (e) => selectDevice(e.target.value));
  for (const b of $("#themeSeg").children) {
    b.addEventListener("click", () => {
      setPref("theme", b.dataset.theme);
      applyTheme();
    });
  }
  $("#vibrateOpt").addEventListener("change", (e) => {
    setPref("vibrate", e.target.checked);
    if (e.target.checked && !navigator.vibrate) toast("This browser can't vibrate");
    else vibrate(80);
  });
  $("#awakeOpt").addEventListener("change", (e) => {
    setPref("awake", e.target.checked);
    updateWake();
  });
  for (const sheet of document.querySelectorAll(".sheet")) {
    sheet.addEventListener("click", (e) => {
      if (e.target === sheet || e.target.closest(".sheet-close")) closeSheets();
    });
  }
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") closeSheets();
  });
  $("#search").addEventListener("input", renderChannels);
  $("#rfForm").addEventListener("submit", (e) => {
    e.preventDefault();
    const rf = Number($("#rfInput").value);
    if (!(rf >= 2 && rf <= 69)) {
      toast("RF channel is 2–69");
      return;
    }
    tune(state.sheetTuner, { rf });
  });

  // Any tap can start the keep-awake video, which browsers only allow from a
  // gesture.
  document.addEventListener("pointerdown", () => {
    if (prefs.awake && !wakeLock && $("#keepAwake").paused) updateWake();
  });

  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      clearTimeout(state.pollTimer);
      saveHistory();
    } else {
      poll();
    }
    updateWake();
  });
  window.addEventListener("pagehide", saveHistory);
  setInterval(saveHistory, 5000);

  // Ages, countdowns and the stale warning move even when no data arrives.
  setInterval(() => {
    for (const p of state.panels) {
      p.el.classList.toggle("stale", isStale());
      $(".stale-note", p.el).textContent = staleText();
      if (state.unlockedUntil[p.tuner] && !isUnlocked(p.tuner)) {
        delete state.unlockedUntil[p.tuner];
        p.actionKey = "";
      }
      renderActions(p, tunerData(p.tuner));
    }
    renderConn();
  }, 250);

  matchMedia("(prefers-color-scheme: dark)").addEventListener("change", applyTheme);

  checkVersion();
  setInterval(checkVersion, 30000);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) checkVersion();
  });

  updateWake();
  loadDevices();
}

init();
