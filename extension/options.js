const $ = (id) => document.getElementById(id);

chrome.storage.local.get(['apiUrl', 'apiKey', 'profile']).then((s) => {
  $('apiUrl').value = s.apiUrl || '';
  $('apiKey').value = s.apiKey || '';
  $('profile').value = s.profile || '';
});

$('save').addEventListener('click', async () => {
  const msg = $('msg');
  let apiUrl = $('apiUrl').value.trim().replace(/\/+$/, '');
  const apiKey = $('apiKey').value.trim();
  let profile = $('profile').value.trim();

  let u;
  try { u = new URL(apiUrl); } catch (_) { msg.textContent = 'API URL is not valid.'; return; }

  // Allow the extension to talk to your API server only.
  const granted = await chrome.permissions.request({ origins: [u.protocol + '//' + u.hostname + '/*'] });
  if (!granted) { msg.textContent = 'Permission to reach your API was not granted.'; return; }

  // Accept a full profile URL too.
  if (profile.includes('instagram.com')) {
    try { profile = new URL(profile.startsWith('http') ? profile : 'https://' + profile).pathname.split('/').filter(Boolean)[0] || ''; } catch (_) {}
  }
  profile = profile.replace(/^@/, '');

  await chrome.storage.local.set({ apiUrl, apiKey, profile });
  msg.textContent = 'Saved.';
});
