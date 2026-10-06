const $ = (id) => document.getElementById(id);

function render(last) {
  const el = $('status');
  el.className = '';
  if (!last) { el.textContent = 'No sync has run yet.'; return; }
  const when = new Date(last.at).toLocaleString();
  if (last.ok) {
    el.textContent = 'Last sync ' + when + ': found ' + last.found + ' links, ' + last.added + ' new.';
  } else {
    el.className = 'bad';
    el.textContent = 'Last sync FAILED (' + when + '): ' + last.message;
  }
}

chrome.storage.local.get('last').then((s) => render(s.last));

async function run(deep) {
  $('now').disabled = $('deep').disabled = true;
  $('status').className = '';
  $('status').textContent = deep ? 'Importing... keep the Instagram tab open until it closes.' : 'Checking...';
  const res = await chrome.runtime.sendMessage({ type: 'run', deep });
  const s = await chrome.storage.local.get('last');
  render(s.last);
  $('now').disabled = $('deep').disabled = false;
}

$('now').addEventListener('click', () => run(false));
$('deep').addEventListener('click', () => run(true));
$('opts').addEventListener('click', () => chrome.runtime.openOptionsPage());
