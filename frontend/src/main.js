import './style.css';

// Frequency band the spectrum bins map onto (keep in sync with engine.go).
const SPEC_FMIN = 300;
const SPEC_FMAX = 1100;

// --- Backend bridge (Wails injects window.go and window.runtime) -------------
const App = window.go?.main?.App;
const RT = window.runtime;
const haveBackend = !!(App && RT);

function call(method, ...args) {
  if (App && typeof App[method] === 'function') return App[method](...args);
  console.warn(`[stub] App.${method}`, args); // running the frontend without Wails
  return Promise.resolve(method === 'ListInputDevices' ? ['Built-in mic'] : undefined);
}

// --- Minimal inline icons (no external font dependency) ----------------------
const SVG = (paths, extra = '') =>
  `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" ${extra}>${paths}</svg>`;
const icons = {
  mic: SVG('<rect x="9" y="3" width="6" height="11" rx="3"/><path d="M5 11a7 7 0 0 0 14 0"/><line x1="12" y1="18" x2="12" y2="21"/>'),
  folder: SVG('<path d="M3 7a2 2 0 0 1 2-2h3.5l2 2H19a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>'),
  export: SVG('<path d="M14 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8"/><path d="M11 12h10"/><path d="M18 9l3 3-3 3"/>'),
  chevron: SVG('<path d="M6 9l6 6 6-6"/>'),
  wave: SVG('<path d="M3 12h2l2-6 4 12 3-9 2 3h5"/>'),
  sun: SVG('<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5L19 19M19 5l-1.5 1.5M6.5 17.5L5 19"/>'),
  moon: SVG('<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/>'),
  play: SVG('<polygon points="5 3 19 12 5 21 5 3" fill="currentColor" stroke="none"/>'),
  pause: SVG('<rect x="5" y="3" width="5" height="18" rx="1.5" fill="currentColor" stroke="none"/><rect x="14" y="3" width="5" height="18" rx="1.5" fill="currentColor" stroke="none"/>'),
};
const $ = (id) => document.getElementById(id);
$('micBtn').innerHTML = icons.mic;
$('fileBtn').innerHTML = icons.folder;
$('exportBtn').innerHTML = icons.export;
$('filtChevron').innerHTML = icons.chevron;
$('waveIcon').innerHTML = icons.wave;
// recBtn icon is set by renderRecBtn() below

// --- State -------------------------------------------------------------------
const md = $('md');
const state = {
  sq: 3, wpm: 20, auto: false, running: false,
  ftype: 'Bandpass', fcenter: 700, fbw: 200, nr: true, agc: false,
  theme: 'light', device: null, detected: 0,
};
const cfg = {
  sq: { min: 0, max: 9, step: 1 },
  wpm: { min: 5, max: 50, step: 1 },
  fcenter: { min: 300, max: 1000, step: 10 },
  fbw: { min: 50, max: 600, step: 10 },
};

function pushFilter() {
  call('SetFilter', {
    type: state.ftype, center: state.fcenter, bandwidth: state.fbw,
    squelch: state.sq, noiseRed: state.nr, agc: state.agc,
  });
}
function pushSpeed() { call('SetSpeed', { wpm: state.wpm, auto: state.auto }); }

// --- Theme -------------------------------------------------------------------
function applyTheme(name) {
  state.theme = name;
  md.classList.toggle('dark', name === 'dark');
  $('themeBtn').innerHTML = name === 'light' ? icons.moon : icons.sun;
}
$('themeBtn').addEventListener('click', () => applyTheme(state.theme === 'light' ? 'dark' : 'light'));
applyTheme('light');

// --- Toolbar summary + WPM control ------------------------------------------
function summary() {
  $('filtSummary').textContent =
    (state.ftype === 'None' ? 'None' : `${state.ftype} · ${state.fbw} Hz`) + ` · sq ${state.sq}`;
}
function renderWpm() {
  const v = $('wpmVal'), step = $('wpmStep');
  // Stepper buttons are only active in manual mode.
  step.querySelectorAll('button').forEach((b) => {
    b.style.opacity = state.auto ? 0.35 : 1;
    b.style.pointerEvents = state.auto ? 'none' : 'auto';
  });
  if (state.running && state.detected > 0) {
    // When running, always show the live-detected WPM in accent colour
    // regardless of auto/manual — the number reflects what the engine is
    // actually using for timing.
    v.style.color = 'var(--acc)';
    v.textContent = state.detected;
  } else if (state.auto) {
    v.style.color = 'var(--acc)';
    v.textContent = state.running ? '…' : '—';
  } else {
    v.style.color = 'var(--tx)';
    v.textContent = state.wpm;
  }
}

function renderRecBtn() {
  const recBtn = $('recBtn');
  recBtn.innerHTML = state.running ? icons.pause : icons.play;
  recBtn.setAttribute('aria-pressed', state.running ? 'true' : 'false');
  recBtn.setAttribute('aria-label', state.running ? 'Pause decoding' : 'Start decoding');
  $('dot').style.background = state.running ? 'var(--acc)' : 'var(--tx3)';
  $('statusText').textContent = state.running ? 'Listening' : 'Idle';
  renderWpm();
}

// Stop (if running) then start with whatever source is currently configured.
// Awaiting Stop() lets the Go goroutine finish before Start() is called.
async function startDecoding() {
  if (state.running) {
    state.running = false;
    renderRecBtn();
    await call('Stop');
  }
  state.running = true;
  renderRecBtn();
  call('Start');
}

document.querySelectorAll('.step').forEach((s) => {
  const k = s.dataset.key, c = cfg[k];
  const bump = (d) => {
    if (k === 'wpm' && state.auto) return;
    state[k] = Math.max(c.min, Math.min(c.max, state[k] + d * c.step));
    s.querySelector('.sv').textContent = state[k];
    if (k === 'wpm') { renderWpm(); pushSpeed(); }
    else { summary(); pushFilter(); }
  };
  s.querySelector('.dec').addEventListener('click', () => bump(-1));
  s.querySelector('.inc').addEventListener('click', () => bump(1));
});

$('ftype').addEventListener('change', function () { state.ftype = this.value; summary(); pushFilter(); });
$('autoWpm').addEventListener('click', function () {
  state.auto = this.getAttribute('aria-pressed') !== 'true';
  this.setAttribute('aria-pressed', state.auto ? 'true' : 'false');
  renderWpm(); pushSpeed();
});
function toggle(id, key) {
  $(id).addEventListener('click', function (e) {
    e.stopPropagation();
    const on = this.getAttribute('aria-pressed') !== 'true';
    this.setAttribute('aria-pressed', on ? 'true' : 'false');
    state[key] = on; pushFilter();
  });
}
toggle('nr', 'nr');
toggle('agc', 'agc');

// --- Popovers (filters + device picker share this) ---------------------------
const toolbar = $('toolbar'), pop = $('popFilters'), filtBtn = $('filtBtn');
function closeAllPops() {
  document.querySelectorAll('.pop').forEach((p) => { p.style.display = 'none'; });
  document.querySelectorAll('[aria-haspopup="true"]').forEach((b) => b.setAttribute('aria-expanded', 'false'));
}
function placePop(popEl, trigger) {
  popEl.style.display = 'block';
  const left = Math.min(trigger.offsetLeft, toolbar.clientWidth - popEl.offsetWidth - 12);
  popEl.style.left = Math.max(0, left) + 'px';
  popEl.style.top = trigger.offsetTop + trigger.offsetHeight + 6 + 'px';
  trigger.setAttribute('aria-expanded', 'true');
}
filtBtn.addEventListener('click', (e) => {
  e.stopPropagation();
  const open = filtBtn.getAttribute('aria-expanded') === 'true';
  closeAllPops();
  if (!open) placePop(pop, filtBtn);
});
$('filtDone').addEventListener('click', (e) => { e.stopPropagation(); closeAllPops(); });
document.addEventListener('click', (e) => {
  if (!e.target.closest('.pop') && !e.target.closest('[aria-haspopup="true"]')) closeAllPops();
});

// --- Source ------------------------------------------------------------------
const micBtn = $('micBtn'), fileBtn = $('fileBtn'), popDevices = $('popDevices'), devList = $('devList');
function setSourceUI(mic, label) {
  micBtn.setAttribute('aria-pressed', mic ? 'true' : 'false');
  fileBtn.setAttribute('aria-pressed', mic ? 'false' : 'true');
  $('srcLabel').textContent = label;
}
function renderDevList(devices) {
  devList.innerHTML = '';
  if (!devices.length) {
    const d = document.createElement('div');
    d.className = 'devempty';
    d.textContent = 'No input devices found';
    devList.appendChild(d);
    return;
  }
  devices.forEach((name) => {
    const b = document.createElement('button');
    b.className = 'devitem';
    b.textContent = name;
    if (name === state.device) b.setAttribute('aria-selected', 'true');
    b.addEventListener('click', async (ev) => {
      ev.stopPropagation();
      state.device = name;
      setSourceUI(true, name);
      localStorage.setItem('lastMicDevice', name);
      closeAllPops();
      await call('SetSource', 'mic', name);
      startDecoding();
    });
    devList.appendChild(b);
  });
}
micBtn.addEventListener('click', async (e) => {
  e.stopPropagation();
  const open = micBtn.getAttribute('aria-expanded') === 'true';
  closeAllPops();
  if (open) return;
  const devices = (await call('ListInputDevices')) || [];
  renderDevList(devices);
  placePop(popDevices, micBtn);
});
fileBtn.addEventListener('click', async (e) => {
  e.stopPropagation();
  closeAllPops();
  const path = await call('OpenWavFile');
  if (!path) return;
  setSourceUI(false, path.split(/[\\/]/).pop());
  await call('SetSource', 'file', path);
  startDecoding();
});

// --- Record / start-stop -----------------------------------------------------
$('recBtn').addEventListener('click', () => {
  state.running = !state.running;
  renderRecBtn();
  call(state.running ? 'Start' : 'Stop');
});

// --- View toggle -------------------------------------------------------------
$('viewSeg').addEventListener('click', (e) => {
  const b = e.target.closest('button'); if (!b) return;
  $('viewSeg').querySelectorAll('button').forEach((x) => x.setAttribute('aria-pressed', x === b ? 'true' : 'false'));
  const v = b.dataset.view, m = $('morseOut');
  $('textOut').style.display = v === 'morse' ? 'none' : 'block';
  m.style.display = v === 'text' ? 'none' : 'block';
  m.style.borderTop = v === 'both' ? '1px dashed var(--line)' : 'none';
  m.style.marginTop = v === 'both' ? '10px' : '0';
  m.style.paddingTop = v === 'both' ? '10px' : '0';
});

// --- Clear / export ----------------------------------------------------------
$('clearBtn').addEventListener('click', () => {
  $('textOut').textContent = '';
  $('morseOut').textContent = '';
  call('Clear');
});
$('exportBtn').addEventListener('click', () => call('ExportText', $('textOut').textContent));

// --- Spectrum rendering ------------------------------------------------------
const cv = $('spec'), ctx = cv.getContext('2d');
let latestBins = [];
function css(name) { return getComputedStyle(md).getPropertyValue(name).trim(); }
function drawSpectrum() {
  const W = cv.width, H = cv.height;
  ctx.clearRect(0, 0, W, H);
  const acc = css('--acc'), dim = css('--tx3');
  const span = SPEC_FMAX - SPEC_FMIN;
  if (state.ftype !== 'None') {
    const x0 = ((state.fcenter - state.fbw / 2 - SPEC_FMIN) / span) * W;
    const x1 = ((state.fcenter + state.fbw / 2 - SPEC_FMIN) / span) * W;
    ctx.globalAlpha = 0.14; ctx.fillStyle = acc; ctx.fillRect(x0, 0, x1 - x0, H); ctx.globalAlpha = 1;
  }
  const n = latestBins.length || 1, bp = W / n;
  for (let i = 0; i < n; i++) {
    const v = latestBins[i] || 0, h = v * (H - 3);
    ctx.fillStyle = v > 0.55 ? acc : dim;
    ctx.globalAlpha = v > 0.55 ? 0.9 : 0.5;
    ctx.fillRect(i * bp + 0.5, H - h, bp - 1, h);
  }
  ctx.globalAlpha = 1;
  if (state.ftype !== 'None') {
    const cx = ((state.fcenter - SPEC_FMIN) / span) * W;
    ctx.strokeStyle = acc; ctx.setLineDash([3, 3]);
    ctx.beginPath(); ctx.moveTo(cx, 0); ctx.lineTo(cx, H); ctx.stroke(); ctx.setLineDash([]);
  }
}

// --- Events from the backend -------------------------------------------------
if (haveBackend) {
  RT.EventsOn('spectrum', (frame) => { latestBins = frame.bins || []; drawSpectrum(); });
  RT.EventsOn('decoded', (chunk) => {
    const t = $('textOut'), m = $('morseOut'), pane = t.parentElement;
    const atBottom = pane.scrollHeight - pane.scrollTop - pane.clientHeight < 24;
    if (chunk.text) t.textContent += chunk.text;
    if (chunk.morse) m.textContent += chunk.morse;
    if (atBottom) pane.scrollTop = pane.scrollHeight;
  });
  RT.EventsOn('status', (s) => {
    $('freqRo').textContent = s.freq;
    $('meter').textContent = s.levelDb + ' dB';
    state.detected = s.wpm;
    renderWpm();
  });
  RT.EventsOn('done', () => {
    // File decode finished — reset to idle so the button returns to play.
    state.running = false;
    state.detected = 0;
    renderRecBtn();
    $('statusText').textContent = 'Done';
  });
  RT.EventsOn('error', (msg) => {
    // Capture failed / stopped unexpectedly — reset to idle and show why.
    state.running = false;
    state.detected = 0;
    renderRecBtn();
    $('dot').style.background = 'var(--rec)';
    $('statusText').textContent = String(msg).slice(0, 80);
  });
}

// --- Init --------------------------------------------------------------------
summary();
renderRecBtn();
drawSpectrum();
pushFilter();
pushSpeed();

// Restore last mic device selection (if any) — show it in the UI but don't
// start decoding. File paths are intentionally not restored (they go stale).
const savedMic = localStorage.getItem('lastMicDevice');
if (savedMic && haveBackend) {
  setSourceUI(true, savedMic);
  call('SetSource', 'mic', savedMic);
}
