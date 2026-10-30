// FLEX browser client: a 10-foot grid driven by arrow keys / D-pad, and a
// player that asks the server how to play each movie (docs/api.md).
const API = '/api/v1';
const CLIENT = 'web';

const grid = document.getElementById('grid');
const empty = document.getElementById('empty');
const player = document.getElementById('player');
const video = document.getElementById('video');
const osd = document.getElementById('osd');

let movies = [];
let focus = 0;
let current = null;   // { movie, info, hls }
let progressTimer = null;
let osdTimer = null;

const api = (path, opts = {}) =>
  fetch(API + path, { ...opts, headers: { 'X-Flex-Client': CLIENT, 'Content-Type': 'application/json', ...(opts.headers || {}) } })
    .then(r => { if (!r.ok) throw new Error(`${r.status} ${path}`); return r.status === 204 ? null : r.json(); });

async function loadServer() {
  try {
    const s = await api('/server');
    document.getElementById('server').textContent = `${s.name}${s.remote ? ' · remote' : ''}`;
  } catch (e) {
    document.getElementById('server').textContent = 'server unreachable';
  }
}

async function loadMovies(q = '') {
  movies = await api(`/movies?sort=title&q=${encodeURIComponent(q)}`);
  render();
}

function render() {
  grid.innerHTML = '';
  empty.hidden = movies.length > 0;
  movies.forEach((m, i) => {
    const card = document.createElement('div');
    card.className = 'card' + (i === focus ? ' focused' : '');
    card.tabIndex = -1;
    card.dataset.index = i;
    const pct = m.runtime_s ? Math.min(100, Math.round(100 * m.resume_s / m.runtime_s)) : 0;
    card.innerHTML = m.poster
      ? `<img src="${m.poster}" alt="" loading="lazy">`
      : `<div class="noart">${esc(m.title)}</div>`;
    card.innerHTML += `
      ${pct > 0 && !m.watched ? `<div class="progress"><i style="width:${pct}%"></i></div>` : ''}
      <div class="title">${esc(m.title)}</div>
      <div class="year">${m.year || ''}${m.watched ? ' · watched' : ''}</div>`;
    card.addEventListener('click', () => { setFocus(i); play(m); });
    grid.appendChild(card);
  });
  scrollFocusIntoView();
}

function setFocus(i) {
  if (!movies.length) return;
  focus = Math.max(0, Math.min(movies.length - 1, i));
  grid.querySelectorAll('.card').forEach(c => c.classList.toggle('focused', +c.dataset.index === focus));
  scrollFocusIntoView();
}

function scrollFocusIntoView() {
  const el = grid.querySelector('.card.focused');
  if (el) el.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
}

function columns() {
  const cards = grid.querySelectorAll('.card');
  if (cards.length < 2) return 1;
  const top = cards[0].offsetTop;
  let n = 0;
  for (const c of cards) { if (c.offsetTop !== top) break; n++; }
  return n || 1;
}

// --- playback ---

async function play(m) {
  const detail = await api(`/movies/${m.id}`);
  const start = detail.resume_s && !detail.watched ? detail.resume_s : 0;
  const info = await api(`/movies/${m.id}/play?start=${start}`);
  current = { movie: detail, info, hls: null };

  player.hidden = false;
  document.getElementById('osd-title').textContent = `${detail.title}${detail.year ? ` (${detail.year})` : ''}`;
  document.getElementById('osd-info').textContent =
    `${info.method} · ${info.video_codec} ${info.width}×${info.height} · ${info.audio_codec}`;
  showOSD();

  if (info.method === 'hls') {
    if (video.canPlayType('application/vnd.apple.mpegurl')) {
      video.src = info.url;
    } else if (window.Hls && Hls.isSupported()) {
      current.hls = new Hls();
      current.hls.loadSource(info.url);
      current.hls.attachMedia(video);
    } else {
      alert('This browser cannot play HLS.');
      return stop();
    }
    // the HLS session already starts at `start`; offset timestamps for progress
    current.offset = start;
  } else {
    video.src = info.url;
    current.offset = 0;
    if (start) video.currentTime = start;
  }
  video.play().catch(() => {});
  progressTimer = setInterval(sendProgress, 10000);
}

function position() {
  return Math.floor((current.offset || 0) + video.currentTime);
}

function sendProgress() {
  if (!current || !video.duration) return;
  api(`/movies/${current.movie.id}/progress`, {
    method: 'PUT',
    body: JSON.stringify({ position_s: position(), duration_s: Math.floor(current.info.duration_s) })
  }).catch(() => {});
}

function stop() {
  if (!current) return;
  sendProgress();
  clearInterval(progressTimer);
  video.pause();
  if (current.hls) current.hls.destroy();
  if (current.info.method === 'hls') {
    const session = current.info.url.split('/')[4];
    api(`/hls/${session}`, { method: 'DELETE' }).catch(() => {});
  }
  video.removeAttribute('src');
  video.load();
  player.hidden = true;
  current = null;
  loadMovies(document.getElementById('search').value);
}

function showOSD() {
  osd.classList.remove('hidden');
  clearTimeout(osdTimer);
  osdTimer = setTimeout(() => osd.classList.add('hidden'), 3000);
}

// --- input: arrow keys (PS5 D-pad arrives as arrows), Enter, Escape/Backspace ---

document.addEventListener('keydown', e => {
  if (e.target === document.getElementById('search')) {
    if (e.key === 'Escape' || e.key === 'ArrowDown') { e.target.blur(); setFocus(focus); }
    return;
  }
  if (current) {
    switch (e.key) {
      case 'Escape': case 'Backspace': stop(); break;
      case ' ': case 'Enter': video.paused ? video.play() : video.pause(); showOSD(); break;
      case 'ArrowRight': video.currentTime += 10; showOSD(); break;
      case 'ArrowLeft': video.currentTime -= 10; showOSD(); break;
      case 'ArrowUp': video.currentTime += 60; showOSD(); break;
      case 'ArrowDown': video.currentTime -= 60; showOSD(); break;
      default: return;
    }
    e.preventDefault();
    return;
  }
  const cols = columns();
  switch (e.key) {
    case 'ArrowRight': setFocus(focus + 1); break;
    case 'ArrowLeft': setFocus(focus - 1); break;
    case 'ArrowDown': setFocus(focus + cols); break;
    case 'ArrowUp': if (focus < cols) document.getElementById('search').focus(); else setFocus(focus - cols); break;
    case 'Enter': if (movies[focus]) play(movies[focus]); break;
    default: return;
  }
  e.preventDefault();
});

video.addEventListener('ended', stop);
video.addEventListener('mousemove', showOSD);

document.getElementById('search').addEventListener('input', e => { focus = 0; loadMovies(e.target.value); });
document.getElementById('scan').addEventListener('click', async () => {
  await api('/library/scan', { method: 'POST' }).catch(() => {});
  const poll = setInterval(async () => {
    const st = await api('/library/status');
    document.getElementById('server').textContent = st.scanning ? `scanning… ${st.files_seen} files` : `${st.movies} movies`;
    if (!st.scanning) { clearInterval(poll); loadMovies(); }
  }, 1500);
});

function esc(s) { return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }

loadServer();
loadMovies();
