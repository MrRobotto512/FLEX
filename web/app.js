// FLEX browser client: a Home screen of rows, Movies and TV grids, all driven
// by arrow keys / D-pad, an episode list per show, and a player that asks the
// server how to play each item (docs/api.md).
const API = '/api/v1';
// web: a browser. android: the Android app. player: FLEX.exe --player, the
// full-screen window on a media PC (it sets window.FLEX_CLIENT before the page runs).
const CLIENT = window.FLEX_CLIENT || (/FLEX-Android\//.test(navigator.userAgent) ? 'android' : 'web');

const grid = document.getElementById('grid');
const empty = document.getElementById('empty');
const episodesView = document.getElementById('episodes');
const eplist = document.getElementById('eplist');
const player = document.getElementById('player');
const video = document.getElementById('video');
const osd = document.getElementById('osd');
const search = document.getElementById('search');
const homeView = document.getElementById('home');
const shelves = document.getElementById('shelves');

let mode = 'home';        // home | movies | tv
let shelfRows = [];       // home rows: { title, items, cont } (cont: Continue Watching items)
let hero = null;          // the server's featured title
let shelf = 0;            // focused home row
let shelfCol = [];        // focused card in each home row
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

// --- home (rows of posters, the focused title up top) ---

async function loadHome() {
  const home = await api('/home');
  hero = home.hero;
  shelfRows = [];
  if (home.continue.length) shelfRows.push({ title: 'Continue Watching', items: home.continue, cont: true });
  shelfRows.push(...home.rows);
  shelf = Math.min(shelf, Math.max(0, shelfRows.length - 1));
  renderHome();
}

function renderHome() {
  shelves.innerHTML = '';
  empty.hidden = shelfRows.length > 0;
  empty.textContent = 'Nothing here yet. Add library folders and press Scan.';
  shelfRows.forEach((row, r) => {
    const sec = document.createElement('section');
    sec.className = 'shelf';
    sec.innerHTML = `<h3>${esc(row.title)}</h3><div class="strip"></div>`;
    const strip = sec.querySelector('.strip');
    row.items.forEach((it, c) => {
      const card = document.createElement('div');
      card.className = 'card';
      card.dataset.r = r;
      card.dataset.c = c;
      const pct = it.runtime_s && !it.watched ? Math.min(100, Math.round(100 * (it.resume_s || 0) / it.runtime_s)) : 0;
      const sub = row.cont ? it.subtitle || (it.resume_s ? `${fmtMin(it.runtime_s - it.resume_s)} left` : 'next episode')
        : it.kind === 'show' ? `${it.seasons || 1} season${it.seasons === 1 ? '' : 's'}`
        : it.kind === 'episode' ? it.subtitle
        : `${it.year || ''}${it.watched ? ' · watched' : ''}`;
      card.innerHTML = (it.poster ? `<img src="${it.poster}" alt="" loading="lazy">` : `<div class="noart">${esc(it.title)}</div>`) +
        (pct > 0 ? `<div class="progress"><i style="width:${pct}%"></i></div>` : '') +
        `<div class="title">${esc(it.title)}</div><div class="year">${esc(sub)}</div>`;
      card.addEventListener('click', () => { focusShelf(r, c); activateShelf(); });
      strip.appendChild(card);
    });
    shelves.appendChild(sec);
  });
  showHero(hero, shelfRows.find(r => r.items.some(it => hero && it.id === hero.id && it.kind === hero.kind))?.title);
  focusShelf(shelf, shelfCol[shelf] || 0);
}

// focusShelf moves the focus to card c of row r and shows that title up top.
function focusShelf(r, c) {
  const row = shelfRows[r];
  if (!row) return;
  shelf = r;
  shelfCol[r] = Math.max(0, Math.min(row.items.length - 1, c));
  shelves.querySelectorAll('.card.focused').forEach(el => el.classList.remove('focused'));
  const el = shelves.querySelector(`.card[data-r="${r}"][data-c="${shelfCol[r]}"]`);
  if (el) {
    el.classList.add('focused');
    el.closest('.shelf').scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    const strip = el.parentElement;
    strip.scrollTo({ left: el.offsetLeft - (strip.clientWidth - el.clientWidth) / 2, behavior: 'smooth' });
  }
  showHero(row.items[shelfCol[r]], row.title);
}

// showHero puts a title's backdrop and details up top (keeping the last one
// for items without a backdrop, like Continue Watching).
function showHero(it, tag) {
  if (!it || !it.backdrop) return;
  document.getElementById('hero').style.backgroundImage = `url("${it.backdrop}")`;
  document.getElementById('hero-tag').textContent = tag || '';
  document.getElementById('hero-title').textContent = it.title;
  document.getElementById('hero-meta').textContent = [it.kind === 'episode' ? it.subtitle : it.year, (it.genres || []).slice(0, 3).join(', '),
    it.kind === 'show' ? `${it.seasons || 1} season${it.seasons === 1 ? '' : 's'}` : fmtMin(it.runtime_s)].filter(Boolean).join(' · ');
  document.getElementById('hero-overview').textContent = it.overview || '';
}

// Enter on a home card: resume, play the movie or open the show.
function activateShelf() {
  const row = shelfRows[shelf];
  const it = row && row.items[shelfCol[shelf]];
  if (!it) return;
  if (row.cont || it.kind !== 'show') play(it.kind, it);
  else openShow(it.id);
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
  homeView.hidden = m !== 'home';
  grid.hidden = m === 'home';
  reload();
}

// reload refreshes whichever of Home and the grids is showing.
function reload() {
  if (mode === 'home') loadHome();
  else loadGrid(search.value);
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
  homeView.hidden = true;
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
  homeView.hidden = mode !== 'home';
  grid.hidden = mode === 'home';
  show = null;
  reload();
}

// --- playback (movies and episodes share everything but the URL prefix) ---

async function play(kind, item) {
  const detail = await api(`/${kind}s/${item.id}`);
  const start = detail.resume_s && !detail.watched ? detail.resume_s : 0;
  // a browser gets the file as it is; the player and the Android app say what
  // they decode and get a stream sized for a TV when the file won't do
  const prof = CLIENT === 'player' ? 'player' : CLIENT === 'android' ? 'ps5' : '';
  const q = `start=${start}` + (prof ? `&profile=${prof}&caps=${clientCaps()}` : '');
  const info = await api(`/${kind}s/${item.id}/play?${q}`);
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
    setTimeout(reload, 300);
  }
}

function showOSD() {
  osd.classList.remove('hidden');
  clearTimeout(osdTimer);
  osdTimer = setTimeout(() => osd.classList.add('hidden'), 3000);
}

// --- input: arrow keys (PS5 D-pad arrives as arrows), Enter, Escape/Backspace, Tab switches Home/Movies/TV ---

// The Android app's Back key: close what is open and say so; false at the home screen (the app leaves).
window.flexBack = () => {
  if (view === 'player') { stop(); return true; }
  if (view === 'episodes') { closeShow(); return true; }
  if (mode !== 'home') { setMode('home'); return true; }
  return false;
};

document.addEventListener('keydown', e => {
  if (e.target === search) {
    if (e.key === 'Escape' || e.key === 'ArrowDown') {
      search.blur();
      if (mode === 'home') focusShelf(shelf, shelfCol[shelf] || 0); else setFocus(focus);
    }
    return;
  }
  if (castView) { castKey(e); e.preventDefault(); return; }
  if (e.key === 'c' || e.key === 'C') { openCast(); e.preventDefault(); return; }
  if (CLIENT === 'player' && e.key === 'q' && view !== 'player') { if (window.flexQuit) flexQuit(); return; }
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
  const next = { home: 'movies', movies: 'tv', tv: 'home' };
  if (mode === 'home') {
    const col = shelfCol[shelf] || 0;
    switch (e.key) {
      case 'ArrowRight': focusShelf(shelf, col + 1); break;
      case 'ArrowLeft': focusShelf(shelf, col - 1); break;
      case 'ArrowDown': if (shelf + 1 < shelfRows.length) focusShelf(shelf + 1, shelfCol[shelf + 1] || 0); break;
      case 'ArrowUp': if (shelf === 0) search.focus(); else focusShelf(shelf - 1, shelfCol[shelf - 1] || 0); break;
      case 'Enter': activateShelf(); break;
      case 'Tab': setMode(next[mode]); break;
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
    case 'Tab': setMode(next[mode]); break;
    default: return;
  }
  e.preventDefault();
});

video.addEventListener('ended', stop);
video.addEventListener('mousemove', showOSD);

document.querySelectorAll('#tabs button').forEach(b => b.addEventListener('click', () => setMode(b.dataset.mode)));
search.addEventListener('input', e => {
  focus = 0;
  if (mode === 'home') setMode('movies'); // search goes through the grids
  else loadGrid(e.target.value);
});
document.getElementById('scan').addEventListener('click', async () => {
  await api('/library/scan', { method: 'POST' }).catch(() => {});
  const poll = setInterval(async () => {
    const st = await api('/library/status');
    document.getElementById('server').textContent = st.scanning
      ? `scanning… ${st.files_seen} files`
      : `${st.movies} movies · ${st.shows} shows`;
    if (!st.scanning) { clearInterval(poll); reload(); }
  }, 1500);
});

// --- what this client decodes (the player and the Android app; a browser plays the file as it is) ---

function clientCaps() {
  const caps = [];
  const v = document.createElement('video');
  if (v.canPlayType('video/mp4; codecs="hvc1.1.6.L153.B0"') || v.canPlayType('video/mp4; codecs="hev1.1.6.L153.B0"')) caps.push('hevc');
  if (Math.max(screen.width, screen.height) * (devicePixelRatio || 1) >= 3000) caps.push('2160p');
  return caps.join(',');
}

// --- Cast to… : the server finds the Chromecasts and sends them the stream; this is the remote ---

const castEl = document.getElementById('cast');
const castList = document.getElementById('cast-list');
let castView = null;     // null | 'devices' | 'remote'
let castDevices = [];
let castFocus = 0;
let castTarget = null;   // { kind, id, start, title } the device list is for
let castStatus = null;   // the server's answer to GET /cast
let castPoll = null;

// focusedItem is what C would cast: the focused card, episode, or what is playing.
function focusedItem() {
  if (view === 'player' && current) return { kind: current.kind, id: current.item.id, start: position() };
  if (view === 'episodes') {
    const r = rows[epFocus];
    return r && r.id ? { kind: 'episode', id: r.id, start: r.resume_s && !r.watched ? r.resume_s : 0 } : null;
  }
  let it = null;
  if (mode === 'home') { const row = shelfRows[shelf]; it = row && row.items[shelfCol[shelf]]; }
  else if (mode === 'movies') it = items[focus];
  if (!it || it.kind === 'show') return null;
  return { kind: it.kind || 'movie', id: it.id, start: it.resume_s && !it.watched ? it.resume_s : 0 };
}

async function openCast() {
  castStatus = await api('/cast').catch(() => null);
  if (castStatus && castStatus.casting) return showCastRemote();
  castTarget = focusedItem();
  if (!castTarget) return;
  castView = 'devices';
  castDevices = [];
  castFocus = 0;
  renderCast('Searching the network…');
  castEl.hidden = false;
  castDevices = await api('/cast/devices').catch(() => []);
  if (castView === 'devices') renderCast(castDevices.length ? '' : 'No Chromecast found. Is it on the same network?');
}

function showCastRemote() {
  castView = 'remote';
  castEl.hidden = false;
  renderCast();
  clearInterval(castPoll);
  castPoll = setInterval(async () => {
    castStatus = await api('/cast').catch(() => null);
    if (castView === 'remote') renderCast();
  }, 2000);
}

function closeCast() {
  castView = null;
  castEl.hidden = true;
  clearInterval(castPoll);
  castPoll = null;
}

function renderCast(note = '') {
  const head = document.getElementById('cast-head'), sub = document.getElementById('cast-sub'), hint = document.getElementById('cast-hint');
  if (castView === 'devices') {
    head.textContent = 'Cast to…';
    sub.textContent = note;
    castList.innerHTML = castDevices.map((d, i) =>
      `<li class="${i === castFocus ? 'focused' : ''}">${esc(d.name)}<span class="muted">${esc(d.model || '')}</span></li>`).join('');
    hint.textContent = castDevices.length ? 'Enter casts · Esc closes' : 'Esc closes';
    return;
  }
  const st = castStatus || {};
  if (!st.casting) { closeCast(); return; }
  head.textContent = `Casting to ${st.device}`;
  sub.textContent = st.title || '';
  const pct = st.duration_s ? Math.min(100, 100 * st.position_s / st.duration_s) : 0;
  castList.innerHTML = `<li><strong>${esc(st.state || '')}</strong><span class="muted">${fmtTime(st.position_s)} / ${fmtTime(st.duration_s)}</span></li>` +
    `<div id="cast-bar"><div style="width:${pct}%"></div></div>`;
  hint.textContent = 'Enter pauses/plays · ←→ ±10 s · ↑↓ ±60 s · Backspace stops casting · Esc hides this';
}

async function castKey(e) {
  if (castView === 'devices') {
    switch (e.key) {
      case 'ArrowDown': castFocus = Math.min(castDevices.length - 1, castFocus + 1); renderCast(); break;
      case 'ArrowUp': castFocus = Math.max(0, castFocus - 1); renderCast(); break;
      case 'Enter': {
        const d = castDevices[castFocus];
        if (!d || !castTarget) break;
        renderCast(`Starting on ${d.name}…`);
        if (view === 'player') stop(); // the device takes over from where this was
        try {
          castStatus = await api('/cast', { method: 'POST', body: JSON.stringify({ device: d.id, kind: castTarget.kind, id: castTarget.id, start: castTarget.start }) });
          showCastRemote();
        } catch (err) {
          renderCast(`Couldn't cast: ${err.message}`);
        }
        break;
      }
      case 'Escape': case 'Backspace': closeCast(); break;
    }
    return;
  }
  const seek = async d => {
    const pos = Math.max(0, (castStatus.position_s || 0) + d);
    castStatus = await api('/cast/seek', { method: 'POST', body: JSON.stringify({ position_s: pos }) }).catch(() => castStatus);
    renderCast();
  };
  switch (e.key) {
    case 'Enter': case ' ':
      castStatus = await api(`/cast/${castStatus.state === 'paused' ? 'play' : 'pause'}`, { method: 'POST' }).catch(() => castStatus);
      renderCast();
      break;
    case 'ArrowRight': seek(10); break;
    case 'ArrowLeft': seek(-10); break;
    case 'ArrowUp': seek(60); break;
    case 'ArrowDown': seek(-60); break;
    case 'Backspace': case 's':
      await api('/cast/stop', { method: 'POST' }).catch(() => {});
      closeCast();
      setTimeout(reload, 300);
      break;
    case 'Escape': closeCast(); break;
  }
}

function fmtTime(s) {
  s = Math.max(0, Math.floor(s || 0));
  const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), sec = s % 60;
  return (h ? `${h}:${pad(m)}` : String(m)) + ':' + pad(sec);
}

// --- the media-PC player: a controller works like the keys, and the mouse pointer hides when idle ---

if (CLIENT === 'player') {
  const padMap = { 12: 'ArrowUp', 13: 'ArrowDown', 14: 'ArrowLeft', 15: 'ArrowRight', 0: 'Enter', 1: 'Escape', 2: 'c', 3: 'Tab', 9: 'Tab', 8: 'Backspace' };
  const keys = ['ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'Enter', 'Escape', 'c', 'Tab', 'Backspace'];
  const next = {}; // key -> when it may repeat while held
  const sendKey = key => document.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true }));
  (function pollPads() {
    const now = performance.now();
    const pressed = {};
    for (const gp of (navigator.getGamepads ? navigator.getGamepads() : [])) {
      if (!gp) continue;
      gp.buttons.forEach((b, i) => { if (b.pressed && padMap[i]) pressed[padMap[i]] = true; });
      const ax = gp.axes[0] || 0, ay = gp.axes[1] || 0;
      if (ax < -0.6) pressed.ArrowLeft = true;
      if (ax > 0.6) pressed.ArrowRight = true;
      if (ay < -0.6) pressed.ArrowUp = true;
      if (ay > 0.6) pressed.ArrowDown = true;
    }
    for (const key of keys) {
      if (!pressed[key]) { delete next[key]; continue; }
      if (next[key] === undefined || now >= next[key]) {
        sendKey(key);
        next[key] = now + (next[key] === undefined ? 400 : 120);
      }
    }
    requestAnimationFrame(pollPads);
  })();

  let cursorTimer = null;
  document.body.classList.add('nocursor');
  document.addEventListener('mousemove', () => {
    document.body.classList.remove('nocursor');
    clearTimeout(cursorTimer);
    cursorTimer = setTimeout(() => document.body.classList.add('nocursor'), 3000);
  });
}

function pad(n) { return String(n).padStart(2, '0'); }
function fmtMin(s) { return s ? Math.round(s / 60) + ' min' : ''; }
function esc(s) { return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }

loadServer();
setMode('home');
