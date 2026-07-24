import './style.css';
import './app.css';

import { StartJob, GetStatus, CancelJob, PickFile, PickMusic, PickOutputDir, OpenOutputDir } from '../wailsjs/go/main/App';
import { EventsOn } from '../wailsjs/runtime/runtime';

document.querySelector('#app').innerHTML = `
  <div class="wrap">
    <h1>🎬 VideoRemix</h1>
    <p class="sub">One source video → many unique variant renders.</p>

    <div class="field">
      <label>Source (URL or local file)</label>
      <div class="row">
        <input id="source" type="text" placeholder="https://youtube.com/... or C:\\path\\video.mp4" />
        <button class="btn ghost" id="pick">Browse…</button>
      </div>
    </div>

    <div class="field">
      <label>Output folder</label>
      <div class="row">
        <input id="output" type="text" placeholder="Default: ./output" />
        <button class="btn ghost" id="pickOut">Browse…</button>
      </div>
    </div>

    <div class="grid">
      <div class="field">
        <label>Variants</label>
        <input id="variants" type="number" min="1" value="5" />
      </div>
      <div class="field">
        <label>Seed (0 = random)</label>
        <input id="seed" type="number" value="0" />
      </div>
    </div>

    <div class="section-title">Editing options</div>

    <div class="field">
      <label>Background music (optional)</label>
      <div class="row">
        <input id="music" type="text" placeholder="No music" />
        <button class="btn ghost" id="pickMusic">Browse…</button>
      </div>
    </div>

    <div class="grid">
      <div class="field">
        <label>Music volume (0–1)</label>
        <input id="musicVol" type="number" min="0" max="1" step="0.05" value="0.3" />
      </div>
      <div class="field">
        <label>Subtitle position</label>
        <select id="subPos">
          <option value="bottom">Bottom</option>
          <option value="top">Top</option>
          <option value="center">Center</option>
        </select>
      </div>
    </div>

    <div class="field">
      <label>Flip horizontal (mirror)</label>
      <select id="flip">
        <option value="random">Random (≈50% of variants)</option>
        <option value="always">Always (every variant)</option>
        <option value="off">Off (never)</option>
      </select>
    </div>

    <div class="field">
      <label>Subtitle text (optional)</label>
      <input id="subtitle" type="text" placeholder="Burned-in caption text" />
    </div>

    <div class="field checkbox-field">
      <label class="checkbox">
        <input id="autoSub" type="checkbox" />
        <span>Auto-subtitle from speech (Whisper) — local files only</span>
      </label>
    </div>

    <div class="field">
      <label>Auto-subtitle language</label>
      <select id="subLang">
        <option value="auto">Auto-detect</option>
        <option value="en">English</option>
        <option value="th">Thai</option>
        <option value="ja">Japanese</option>
        <option value="zh">Chinese</option>
      </select>
    </div>

    <div class="field checkbox-field">
      <label class="checkbox">
        <input id="extraFx" type="checkbox" checked />
        <span>Randomize extra effects (blur, zoom, hue, audio)</span>
      </label>
    </div>

    <div class="field checkbox-field">
      <label class="checkbox">
        <input id="noGPU" type="checkbox" />
        <span>Force CPU (disable GPU — GPU is used automatically by default)</span>
      </label>
    </div>

    <div class="row actions">
      <button class="btn" id="start">Start Remix</button>
      <button class="btn danger" id="cancel" disabled>Cancel</button>
      <button class="btn ghost" id="openOut">Open Output</button>
    </div>

    <div class="status">
      <div class="statusline">
        <span id="phase" class="badge">idle</span>
        <span id="counts" class="counts"></span>
      </div>
      <div class="bar"><div class="bar-fill" id="bar"></div></div>
    </div>

    <div class="field">
      <label>Log</label>
      <pre id="log" class="log"></pre>
    </div>
  </div>
`;

const $ = (id) => document.getElementById(id);
let currentJob = null;

function log(msg) {
  const el = $('log');
  el.textContent += msg + '\n';
  el.scrollTop = el.scrollHeight;
}

function setBusy(busy) {
  $('start').disabled = busy;
  $('cancel').disabled = !busy;
}

function renderStatus(u) {
  $('phase').textContent = u.phase;
  $('phase').className = 'badge ' + u.phase;
  const done = (u.completed || 0) + (u.failed || 0) + (u.cancelled || 0);
  $('counts').textContent = u.total > 0 ? `${done}/${u.total} (${u.percent.toFixed(0)}%)` : '';
  $('bar').style.width = (u.percent || 0) + '%';

  const terminal = ['completed', 'failed', 'cancelled'].includes(u.phase);
  if (terminal) {
    setBusy(false);
    if (u.error) log('❌ ' + u.phase + ': ' + u.error);
    else log('✅ ' + u.phase + (u.total ? ` — ${u.completed} variants rendered` : ''));
    currentJob = null;
  }
}

// Live events from the Go core.
EventsOn('job:status', renderStatus);
EventsOn('job:log', (line) => log('· ' + line));

$('pick').addEventListener('click', async () => {
  try {
    const path = await PickFile();
    if (path) $('source').value = path;
  } catch (e) { log('pick error: ' + e); }
});

$('pickOut').addEventListener('click', async () => {
  try {
    const dir = await PickOutputDir();
    if (dir) $('output').value = dir;
  } catch (e) { log('pick output error: ' + e); }
});

$('pickMusic').addEventListener('click', async () => {
  try {
    const path = await PickMusic();
    if (path) $('music').value = path;
  } catch (e) { log('pick music error: ' + e); }
});

$('start').addEventListener('click', async () => {
  const source = $('source').value.trim();
  if (!source) { log('⚠ Please provide a source URL or file.'); return; }

  const opts = {
    source,
    variants: parseInt($('variants').value || '1', 10),
    seed: parseInt($('seed').value || '0', 10),
    outputDir: $('output').value.trim(),
    musicPath: $('music').value.trim(),
    musicVolume: parseFloat($('musicVol').value || '0.3'),
    subtitle: $('subtitle').value.trim(),
    subtitlePos: $('subPos').value,
    extraEffects: $('extraFx').checked,
    flip: $('flip').value,
    noGPU: $('noGPU').checked,
    autoSubtitle: $('autoSub').checked,
    subLang: $('subLang').value,
  };

  setBusy(true);
  $('log').textContent = '';
  log(`▶ Starting: ${source} (variants=${opts.variants}, seed=${opts.seed})`);
  if (opts.musicPath) log(`  🎵 music: ${opts.musicPath} (vol ${opts.musicVolume})`);
  if (opts.subtitle) log(`  💬 subtitle: "${opts.subtitle}" (${opts.subtitlePos})`);
  if (opts.extraEffects) log(`  ✨ extra effects: on`);
  if (opts.flip !== 'random') log(`  🔄 flip: ${opts.flip}`);
  if (opts.autoSubtitle) log(`  🎙 auto-subtitle: ${opts.subLang}`);
  log(`  output: ${opts.outputDir || './output'}`);
  try {
    currentJob = await StartJob(opts);
    log('Job: ' + currentJob);
  } catch (e) {
    log('start error: ' + e);
    setBusy(false);
  }
});

$('cancel').addEventListener('click', async () => {
  if (!currentJob) return;
  try { await CancelJob(currentJob); log('⏹ Cancel requested'); }
  catch (e) { log('cancel error: ' + e); }
});

$('openOut').addEventListener('click', () => OpenOutputDir());
