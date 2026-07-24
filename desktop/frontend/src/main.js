import './style.css';
import './app.css';

import { StartJob, GetStatus, CancelJob, PickFile, PickOutputDir, OpenOutputDir } from '../wailsjs/go/main/App';
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

$('start').addEventListener('click', async () => {
  const source = $('source').value.trim();
  if (!source) { log('⚠ Please provide a source URL or file.'); return; }
  const variants = parseInt($('variants').value || '1', 10);
  const seed = parseInt($('seed').value || '0', 10);
  const output = $('output').value.trim();

  setBusy(true);
  $('log').textContent = '';
  log(`▶ Starting: ${source} (variants=${variants}, seed=${seed})`);
  log(`  output: ${output || './output'}`);
  try {
    currentJob = await StartJob(source, variants, seed, output);
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
