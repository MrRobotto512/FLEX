// FLEX browser client: Movies and TV grids driven by arrow keys / D-pad, an
// episode list per show, and a player that asks the server how to play each
// item (docs/api.md).
const API = '/api/v1';
const CLIENT = 'web';

const grid = document.getElementById('grid');
const empty = document.getElementById('empty');
const episodesView = document.getElementById('episodes');
const eplist = document.getElementById('eplist');
const player = document.getElementById('player');
const video = document.getElementById('video');
const osd = document.getElementById('osd');
const search = document.getElementById('search');

let mode = 'movies';      // movies | tv
let view = 'grid';        // grid | episodes | player
let items = [];           // movies or shows in the grid
let focus = 0;
let show = null;          // show detail while in the episodes view
let rows = [];            // episodes view rows: { season } headers or episodes
let epFocus = 0;
let current = null;       // { kind, item, info, hls, offset }
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

// --- grid (movies or shows) ---

async function loadGrid(q = '') {
  items = mode === 'movies'
    ? await api(`/movies?sort=title&q=${encodeURIComponent(q)}`)
    : await api(`/shows?q=${encodeURIComponent(q)}`);
  if (focus >= items.length) focus = Math.max(0, items.length - 1);
  renderGrid();
}

function renderGrid() {
  grid.innerHTML = '';
  empty.hidden = items.length > 0;
  empty.textContent = mode === 'movies'
    ? 'No movies yet. Add library folders and press Scan.'
    : 'No TV shows yet. Episodes are recognised by S01E02-style names.';
  items.forEach((it, i) => {
    const card = document.createElement('div');
    card.className = 'card' + (i === focus ? ' focused' : '');
    card.dataset.index = i;
    card.innerHTML = it.poster
      ? `<img src="${it.poster}" alt="" loading="lazy">`
      : `<div class="noart">${esc(it.title)}</div>`;
    if (mode === 'movies') {
      const pct = it.runtime_s ? Math.min(100, Math.round(100 * it.resume_s / it.runtime_s)) : 0;
      card.innerHTML += `
        ${pct > 0 && !it.watched ? `<div class="progress"><i style="width:${pct}%"></i></div>` : ''}
        <div class="title">${esc(it.title)}</div>
        <div class="year">${it.year || ''}${it.watched ? ' · watched' : ''}</div>`;
    } else {
      card.innerHTML += `
        ${it.unwatched ? `<span class="badge">${it.unwatched}</span>` : ''}
        <div class="title">${esc(it.title)}</div>
        <div class="year">${it.year ? it.year + ' · ' : ''}${it.seasons} season${it.seasons === 1 ? '' : 's'} · ${it.episodes} ep</div>`;
    }
    card.addEventListener('click', () => { setFocus(i); activate(); });
    grid.appendChild(card);
  });
  scrollIntoView(grid.querySelector('.card.focused'));
}

function setFocus(i) {
  if (!items.length) return;
  focus = Math.max(0, Math.min(items.length - 1, i));
  grid.querySelectorAll('.card').forEach(c => c.classList.toggle('focused', +c.dataset.index === focus));
  scrollIntoView(grid.querySelector('.card.focused'));
}

function scrollIntoView(el) {
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

function setMode(m) {
  mode = m;
  focus = 0;
  document.querySelectorAll('#tabs button').forEach(b => b.classList.toggle('active', b.dataset.mode === m));
  loadGrid(search.value);
}

// Enter on a card: play the movie or open the show.
function activate() {
  const it = items[focus];
  if (!it) return;
  if (mode === 'movies') play('movie', it);
  else openShow(it.id);
}

// --- episodes view ---

async function openShow(id, keepFocus = false) {
  show = await api(`/shows/${id}`);
  rows = [];
  for (const s of show.season_list || []) {
    rows.push({ season: s.season });
    for (const ep of s.episodes) rows.push(ep);
  }
  if (!keepFocus) {
    const next = show.next_episode ? rows.findIndex(r => r.id === show.next_episode.id) : -1;
    epFocus = next >= 0 ? next : rows.findIndex(r => r.id);
  }
  if (epFocus >= rows.length) epFocus = rows.length - 1;
  document.getElementById('show-poster').src = show.poster || '';
  document.getElementById('show-title').textContent = `${show.title}${show.year ? ` (${show.year})` : ''}`;
  document.getElementById('show-meta').textContent =
    `${show.seasons} season${show.seasons === 1 ? '' : 's'} · ${show.episodes} episodes · ${show.unwatched} unwatched` +
    (show.next_episode ? ` · next: S${pad(show.next_episode.season)}E${pad(show.next_episode.episode)}` : '');
  document.getElementById('show-overview').textContent = show.overview || '';
  renderEpisodes();
  view = 'episodes';
  grid.hidden = true;
  empty.hidden = true;
  episodesView.hidden = false;
}

function renderEpisodes() {
  eplist.innerHTML = '';
  rows.forEach((r, i) => {
    const li = document.createElement('li');
    if (!r.id) {
      li.className = 'season';
      li.textContent = r.season === 0 ? 'Specials' : `Season ${r.season}`;
    } else {
      const pct = r.runtime_s ? Math.min(100, Math.round(100 * r.resume_s / r.runtime_s)) : 0;
      li.className = i === epFocus ? 'focused' : '';
      li.innerHTML = `
        <span class="num">S${pad(r.season)}E${pad(r.episode)}</span>
        <img class="still" src="${r.still || ''}" alt="" loading="lazy">
        <span class="ep-title">${esc(r.title)}<small>${esc(r.overview || '')}</small></span>
        <span class="state ${r.watched ? 'watched' : ''}">${r.watched ? 'watched' : pct ? pct + '%' : fmtMin(r.runtime_s)}</span>`;
      li.addEventListener('click', () => { epFocus = i; play('episode', r); });
    }
    li.dataset.index = i;
    eplist.appendChild(li);
  });
  scrollIntoView(eplist.querySelector('li.focused'));
}

function moveEpisode(dir) {
  let i = epFocus;
  do { i += dir; } while (i >= 0 && i < rows.length && !rows[i].id);
  if (i < 0 || i >= rows.length) return;
  epFocus = i;
  eplist.querySelectorAll('li').forEach(li => li.classList.toggle('focused', +li.dataset.index === epFocus));
  scrollIntoView(eplist.querySelector('li.focused'));
}

function closeShow() {
  view = 'grid';
  episodesView.hidden = true;
  grid.hidden = false;
  show = null;
  loadGrid(search.value);
}

// --- playback (movies and episodes share everything but the URL prefix) ---

async function play(kind, item) {
  const detail = await api(`/${kind}s/${item.id}`);
  const start = detail.resume_s && !detail.watched ? detail.resume_s : 0;
  const info = await api(`/${kind}s/${item.id}/play?start=${start}`);
  current = { kind, item: detail, info, hls: null, offset: 0 };

  view = 'player';
  player.hidden = false;
  const title = kind === 'movie'
    ? `${detail.title}${detail.year ? ` (${detail.year})` : ''}`
    : `${detail.show_title} · S${pad(detail.season)}E${pad(detail.episode)} · ${detail.title}`;
  document.getElementById('osd-title').textContent = title;
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
    current.offset = start; // the HLS session already starts at `start`
  } else {
    video.src = info.url;
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
  api(`/${current.kind}s/${current.item.id}/progress`, {
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
  const wasEpisode = current.kind === 'episode';
  current = null;
  if (wasEpisode && show) {
    view = 'episodes';
    setTimeout(() => openShow(show.id, true), 300); // let the progress PUT land first
  } else {
    view = 'grid';
    setTimeout(() => loadGrid(search.value), 300);
  }
}

function showOSD() {
  osd.classList.remove('hidden');
  clearTimeout(osdTimer);
  osdTimer = setTimeout(() => osd.classList.add('hidden'), 3000);
}

// --- input: arrow keys (PS5 D-pad arrives as arrows), Enter, Escape/Backspace, Tab switches Movies/TV ---

document.addEventListener('keydown', e => {
  if (e.target === search) {
    if (e.key === 'Escape' || e.key === 'ArrowDown') { search.blur(); setFocus(focus); }
    return;
  }
  if (view === 'player') {
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
  if (view === 'episodes') {
    switch (e.key) {
      case 'ArrowDown': moveEpisode(1); break;
      case 'ArrowUp': moveEpisode(-1); break;
      case 'Enter': if (rows[epFocus] && rows[epFocus].id) play('episode', rows[epFocus]); break;
      case 'Escape': case 'Backspace': closeShow(); break;
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
    case 'ArrowUp': if (focus < cols) search.focus(); else setFocus(focus - cols); break;
    case 'Enter': activate(); break;
    case 'Tab': setMode(mode === 'movies' ? 'tv' : 'movies'); break;
    default: return;
  }
  e.preventDefault();
});

video.addEventListener('ended', stop);
video.addEventListener('mousemove', showOSD);

document.querySelectorAll('#tabs button').forEach(b => b.addEventListener('click', () => setMode(b.dataset.mode)));
search.addEventListener('input', e => { focus = 0; loadGrid(e.target.value); });
document.getElementById('scan').addEventListener('click', async () => {
  await api('/library/scan', { method: 'POST' }).catch(() => {});
  const poll = setInterval(async () => {
    const st = await api('/library/status');
    document.getElementById('server').textContent = st.scanning
      ? `scanning… ${st.files_seen} files`
      : `${st.movies} movies · ${st.shows} shows`;
    if (!st.scanning) { clearInterval(poll); loadGrid(search.value); }
  }, 1500);
});

function pad(n) { return String(n).padStart(2, '0'); }
function fmtMin(s) { return s ? Math.round(s / 60) + ' min' : ''; }
function esc(s) { return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }

loadServer();
loadGrid();
