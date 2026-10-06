// IG Link Sync - background service worker.
// While Chrome is open, it checks once an hour whether a sync is due
// (last success older than 12 h => about twice a day) and runs it.

const CHECK_EVERY_MIN = 60;
const MIN_GAP_OK_MS = 12 * 3600 * 1000;   // success -> next sync after 12 h
const MIN_GAP_TRY_MS = 6 * 3600 * 1000;   // failure -> wait 6 h before trying again

chrome.runtime.onInstalled.addListener(ensureAlarm);
chrome.runtime.onStartup.addListener(() => {
  ensureAlarm();
  maybeRun();
});

chrome.alarms.onAlarm.addListener((a) => {
  if (a.name === 'tick') maybeRun();
});

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (msg && msg.type === 'run') {
    runSync({ deep: !!msg.deep }).then(sendResponse);
    return true; // async response
  }
});

async function ensureAlarm() {
  const a = await chrome.alarms.get('tick');

  if (!a) {
    chrome.alarms.create('tick', {
      delayInMinutes: 1,
      periodInMinutes: CHECK_EVERY_MIN
    });
  }
}

async function maybeRun() {
  const s = await chrome.storage.local.get([
    'apiUrl',
    'lastOkAt',
    'lastTryAt'
  ]);

  if (!s.apiUrl) return;

  const now = Date.now();

  if (now - (s.lastOkAt || 0) < MIN_GAP_OK_MS) return;
  if (now - (s.lastTryAt || 0) < MIN_GAP_TRY_MS) return;

  await runSync({ deep: false });
}

let running = false;

async function runSync({ deep }) {
  if (running) {
    return {
      ok: false,
      message: 'A sync is already running.'
    };
  }

  running = true;

  const cfg = await chrome.storage.local.get([
    'apiUrl',
    'apiKey',
    'profile'
  ]);

  try {
    if (!cfg.apiUrl || !cfg.apiKey || !cfg.profile) {
      throw new Error(
        'Open the extension options and fill in API URL, API key and profile.'
      );
    }

    await chrome.storage.local.set({
      lastTryAt: Date.now()
    });

    const posts = await collectPosts(cfg.profile, deep);

    const res = await post(cfg, {
      profile: cfg.profile,
      posts: posts
    });

    await chrome.storage.local.set({
      lastOkAt: Date.now(),
      last: {
        ok: true,
        at: Date.now(),
        found: res.found,
        added: res.added,
        deep
      }
    });

    chrome.action.setBadgeText({ text: '' });

    return {
      ok: true,
      found: res.found,
      added: res.added
    };

  } catch (e) {
    const message = String((e && e.message) || e);

    await chrome.storage.local.set({
      last: {
        ok: false,
        at: Date.now(),
        message
      }
    });

    chrome.action.setBadgeText({ text: '!' });
    chrome.action.setBadgeBackgroundColor({ color: '#c00' });

    // Tell the API so /status and the WordPress settings page can show the problem.
    try {
      await post(cfg, {
        profile: cfg.profile,
        posts: [],
        error: message
      });
    } catch (_) {}

    return {
      ok: false,
      message
    };

  } finally {
    running = false;
  }
}


// ------------------------------------------------------------
// Open Instagram profile and collect post URLs + image URLs
// ------------------------------------------------------------

async function collectPosts(profile, deep) {
  const url =
    'https://www.instagram.com/' +
    encodeURIComponent(profile) +
    '/';

  // Normal daily check: background tab, reads the posts that load with the page.
  // Deep import: visible tab so scrolling can load more posts.
  const tab = await chrome.tabs.create({
    url,
    active: deep
  });

  try {
    await waitForComplete(tab.id);

    const [inj] = await chrome.scripting.executeScript({
      target: {
        tabId: tab.id
      },
      func: scrapePage,
      args: [deep ? 80 : 0]
    });

    const out = inj && inj.result;

    if (!out) {
      throw new Error('Could not read the page.');
    }

    if (out.error) {
      throw new Error(out.error);
    }

    return out.posts;

  } finally {
    chrome.tabs.remove(tab.id).catch(() => {});
  }
}


function waitForComplete(tabId, timeoutMs = 30000) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      chrome.tabs.onUpdated.removeListener(listener);
      reject(
        new Error('The Instagram page took too long to load.')
      );
    }, timeoutMs);

    function listener(id, info) {
      if (id === tabId && info.status === 'complete') {
        clearTimeout(timer);
        chrome.tabs.onUpdated.removeListener(listener);
        resolve();
      }
    }

    chrome.tabs.onUpdated.addListener(listener);

    chrome.tabs
      .get(tabId)
      .then((t) => {
        if (t.status === 'complete') {
          listener(tabId, { status: 'complete' });
        }
      })
      .catch(() => {});
  });
}


// ------------------------------------------------------------
// This function runs INSIDE the Instagram page.
// It must be completely self-contained.
// ------------------------------------------------------------

async function scrapePage(maxRounds) {
  const sleep = (ms) =>
    new Promise((r) => setTimeout(r, ms));

  const re =
    /^\/(?:[A-Za-z0-9_.]+\/)?(?:p|reel)\/([A-Za-z0-9_-]+)\/?$/;

  const seen = new Map();

  function add() {
    let n = 0;

    for (const a of document.querySelectorAll('a[href]')) {
      let path;

      try {
        path = new URL(a.href).pathname;
      } catch (_) {
        continue;
      }

      const m = path.match(re);

      if (!m) continue;

      const shortcode = m[1];

      const postUrl =
        'https://www.instagram.com/p/' +
        shortcode +
        '/';

      // Try to find the image belonging to this post.
      //
      // Most Instagram profile-grid post links contain an <img>
      // inside the same <a>. We check several possibilities.
      let imageUrl = '';

      const img =
        a.querySelector('img') ||
        a.querySelector('picture img') ||
        a.closest('article')?.querySelector('img');

      if (img) {
        imageUrl =
          img.currentSrc ||
          img.src ||
          img.getAttribute('src') ||
          '';
      }

      // Also try srcset if src/currentSrc isn't available.
      if (!imageUrl && img) {
        const srcset =
          img.getAttribute('srcset') ||
          img.getAttribute('data-srcset') ||
          '';

        if (srcset) {
          const candidates = srcset
            .split(',')
            .map((x) => x.trim())
            .filter(Boolean);

          if (candidates.length) {
            const last = candidates[candidates.length - 1];
            imageUrl = last.split(/\s+/)[0] || '';
          }
        }
      }

      // Normalize relative image URLs.
      if (imageUrl) {
        try {
          imageUrl = new URL(imageUrl, location.href).href;
        } catch (_) {
          imageUrl = '';
        }
      }

      if (!seen.has(shortcode)) {
        seen.set(shortcode, {
          shortcode,
          url: postUrl,
          imageUrl
        });

        n++;
      } else {
        // If we previously found the post without an image,
        // update it if we can find one now.
        const existing = seen.get(shortcode);

        if (!existing.imageUrl && imageUrl) {
          existing.imageUrl = imageUrl;
        }
      }
    }

    return n;
  }


  // Wait for Instagram posts to appear.
  let waited = 0;

  while (add() === 0 && waited < 15000) {
    if (location.pathname.startsWith('/accounts/login')) {
      return {
        error:
          'Instagram showed its login page. Log in to Instagram in this Chrome once, then try again.'
      };
    }

    await sleep(500);
    waited += 500;
  }

  if (seen.size === 0) {
    return {
      error:
        'No posts found (login wall, private profile, or wrong username).'
    };
  }

  // Give Instagram a little time to finish loading images.
  await sleep(1500);

  add();


  // Deep import:
  // scroll through the profile and keep collecting posts.
  let idle = 0;

  for (
    let i = 0;
    i < maxRounds && idle < 4;
    i++
  ) {
    window.scrollTo(
      0,
      document.body.scrollHeight
    );

    await sleep(
      1500 + Math.random() * 1000
    );

    const before = seen.size;

    add();

    if (seen.size === before) {
      idle++;
    } else {
      idle = 0;
    }
  }


  return {
    posts: [...seen.values()]
  };
}


// ------------------------------------------------------------
// Send data to Go API
// ------------------------------------------------------------

async function post(cfg, body) {
  const res = await fetch(
    cfg.apiUrl.replace(/\/+$/, '') + '/ingest',
    {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-API-Key': cfg.apiKey
      },
      body: JSON.stringify(body)
    }
  );

  if (!res.ok) {
    throw new Error(
      'API returned HTTP ' + res.status
    );
  }

  return res.json();
}