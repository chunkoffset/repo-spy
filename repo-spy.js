#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');

let notifier = null;
try {
  notifier = require('node-notifier');
} catch (_) {}

const UA = 'repo-monitor/1.0';
const GREEN = 0x57f287, ORANGE = 0xfaa61a, RED = 0xed4245, BLUE = 0x5865f2;
const MAX_REPOS = 10;
const MIN_INTERVAL = 30;

const etagCache = new Map();

class RateLimited extends Error {
  constructor(wait) {
    super(`rate limited, retry in ${wait}s`);
    this.wait = wait;
  }
}

class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function pad(n) {
  return String(n).padStart(2, '0');
}

function log(msg) {
  const d = new Date();
  const ts = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  console.log(`${ts}  ${msg}`);
}

function clip(s, n) {
  s = s || '';
  return s.length <= n ? s : s.slice(0, n - 1) + '…';
}

function bullets(items, limit) {
  let out = items.slice(0, limit).map((i) => '• ' + i).join('\n');
  if (items.length > limit) out += `\n… and ${items.length - limit} more`;
  return out;
}

const event = (title, body, url, color) => ({ title, body, url, color });

function parseRepo(s) {
  s = String(s).trim().replace(/^https?:\/\/github\.com\//, '').replace(/\.git$/, '').replace(/^\/+|\/+$/g, '');
  const m = /^([\w.-]+)\/([\w.-]+)$/.exec(s);
  if (!m) throw new Error(`invalid repo '${s}' (expected owner/name)`);
  return [m[1], m[2]];
}

function loadConfig(file) {
  const raw = JSON.parse(fs.readFileSync(file, 'utf8'));
  const repos = (raw.repos || []).map(parseRepo);
  if (repos.length < 1 || repos.length > MAX_REPOS) {
    throw new Error(`'repos' must contain between 1 and ${MAX_REPOS} entries`);
  }
  const interval = parseInt(raw.poll_interval_seconds ?? 60, 10);
  if (!(interval >= MIN_INTERVAL)) throw new Error(`'poll_interval_seconds' must be at least ${MIN_INTERVAL}`);
  return {
    token: process.env.GITHUB_TOKEN || raw.github_token || '',
    webhook: raw.discord_webhook_url || '',
    desktop: raw.desktop_notifications !== false,
    interval,
    api: (raw.api_url || 'https://api.github.com').replace(/\/+$/, ''),
    repos,
  };
}

function loadState(file) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (e) {
    if (e.code !== 'ENOENT') log(`state file ${file} is unreadable, starting fresh`);
    return {};
  }
}

function saveState(file, state) {
  const tmp = file + '.tmp';
  fs.writeFileSync(tmp, JSON.stringify(state));
  fs.renameSync(tmp, file);
}

async function gh(cfg, apiPath) {
  const url = cfg.api + apiPath;
  const cacheable = !apiPath.includes('/git/trees/');
  const headers = {
    Accept: 'application/vnd.github+json',
    'X-GitHub-Api-Version': '2022-11-28',
    'User-Agent': UA,
  };
  if (cfg.token) headers.Authorization = `Bearer ${cfg.token}`;
  const cached = cacheable ? etagCache.get(url) : undefined;
  if (cached) headers['If-None-Match'] = cached.etag;

  const res = await fetch(url, { headers, signal: AbortSignal.timeout(30000) });
  if (res.status === 304 && cached) return cached.body;
  const text = await res.text();
  if (res.ok) {
    const body = JSON.parse(text);
    const etag = res.headers.get('etag');
    if (etag && cacheable) etagCache.set(url, { etag, body });
    return body;
  }
  let msg = text;
  try {
    msg = JSON.parse(text).message ?? text;
  } catch (_) {}
  if (
    (res.status === 403 || res.status === 429) &&
    (res.headers.get('x-ratelimit-remaining') === '0' || res.headers.get('retry-after') || /rate limit/i.test(msg))
  ) {
    let wait = 60;
    if (res.headers.get('retry-after')) wait = parseInt(res.headers.get('retry-after'), 10);
    else if (res.headers.get('x-ratelimit-reset')) {
      wait = Math.max(parseInt(res.headers.get('x-ratelimit-reset'), 10) - Math.floor(Date.now() / 1000), 1);
    }
    throw new RateLimited(wait + 1);
  }
  throw new ApiError(res.status, msg);
}

function desktopNotify(title, body) {
  if (!notifier) {
    log('node-notifier is not installed, skipping desktop notification');
    return;
  }
  try {
    notifier.notify({ title: clip(title, 60), message: clip(body, 250) || ' ', wait: false }, (err) => {
      if (err) log(`desktop notification failed: ${err.message || err}`);
    });
  } catch (e) {
    log(`desktop notification failed: ${e.message}`);
  }
}

async function postDiscord(url, payload) {
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      const res = await fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'User-Agent': UA },
        body: JSON.stringify(payload),
        signal: AbortSignal.timeout(20000),
      });
      if (res.ok) return;
      if (res.status === 429 && attempt === 0) {
        let wait = 2;
        try {
          wait = Number((await res.json()).retry_after) || 2;
        } catch (_) {}
        await sleep(Math.min(wait, 30) * 1000);
        continue;
      }
      log(`discord webhook failed: HTTP ${res.status}`);
      return;
    } catch (e) {
      log(`discord webhook failed: ${e.message}`);
      return;
    }
  }
}

async function dispatch(cfg, label, events) {
  if (!events.length) return;
  for (const e of events) {
    log(`[${label}] ${e.title}`);
    if (cfg.desktop) desktopNotify(`${label} — ${e.title}`, e.body);
  }
  if (cfg.webhook) {
    const now = new Date().toISOString();
    for (let i = 0; i < events.length; i += 10) {
      const embeds = events.slice(i, i + 10).map((e) => {
        const embed = {
          title: clip(e.title, 256),
          description: clip(e.body, 3900),
          color: e.color,
          footer: { text: label },
          timestamp: now,
        };
        if (e.url) embed.url = e.url;
        return embed;
      });
      await postDiscord(cfg.webhook, { username: 'Repo Monitor', allowed_mentions: { parse: [] }, embeds });
    }
  }
}

function diffMeta(old, now, url, full) {
  const ev = [];
  if (old.owner.toLowerCase() !== now.owner.toLowerCase()) {
    ev.push(event('Repository owner changed', `${old.owner} → ${now.owner}\nNow: ${full}`, url, RED));
  }
  if (old.name !== now.name) ev.push(event('Repository renamed', `${old.name} → ${now.name}\nNow: ${full}`, url, RED));
  if (old.private !== now.private) {
    ev.push(event(now.private ? 'Repository is now private' : 'Repository is now public', full, url, RED));
  }
  if (old.archived !== now.archived) {
    ev.push(event(now.archived ? 'Repository archived' : 'Repository unarchived', full, url, ORANGE));
  }
  if (old.disabled !== now.disabled) {
    ev.push(event(now.disabled ? 'Repository disabled' : 'Repository re-enabled', full, url, RED));
  }
  if (old.default_branch !== now.default_branch) {
    ev.push(event('Default branch changed', `${old.default_branch} → ${now.default_branch}`, url, ORANGE));
  }
  if (old.description !== now.description) {
    ev.push(event('Description changed', `“${clip(old.description, 300)}” → “${clip(now.description, 300)}”`, url, ORANGE));
  }
  return ev;
}

async function fetchBranches(cfg, full) {
  const out = {};
  for (let page = 1; page <= 5; page++) {
    const data = await gh(cfg, `/repos/${full}/branches?per_page=100&page=${page}`);
    for (const b of data) out[b.name] = b.commit.sha;
    if (data.length < 100) break;
  }
  return out;
}

async function optionalList(cfg, apiPath) {
  try {
    return await gh(cfg, apiPath);
  } catch (e) {
    if (e instanceof ApiError && (e.status === 409 || e.status === 410)) return [];
    throw e;
  }
}

async function guarded(key, what, fn) {
  try {
    return await fn();
  } catch (e) {
    if (e instanceof ApiError) {
      log(`[${key}] ${what} check failed (HTTP ${e.status}: ${e.message})`);
      return undefined;
    }
    throw e;
  }
}

const diffSet = (a, b) => a.filter((x) => !b.includes(x));

async function checkRepo(cfg, key, owner, name, state, events) {
  let st = state[key];
  const first = st === undefined;
  let info;
  try {
    info = await gh(cfg, first ? `/repos/${owner}/${name}` : `/repositories/${st.id}`);
  } catch (e) {
    if (!(e instanceof ApiError)) throw e;
    if (first) {
      log(`[${key}] cannot access repository (HTTP ${e.status}: ${e.message}); will retry`);
      return;
    }
    if ([403, 404, 410, 451].includes(e.status)) {
      if (!st.unavailable) {
        st.unavailable = true;
        const last = st.label || key;
        let title, why;
        if (e.status === 451) {
          title = 'Repository taken down (legal block)';
          why = 'GitHub returned HTTP 451 (blocked for legal reasons, e.g. DMCA).';
        } else if (e.status === 403) {
          title = 'Repository access blocked';
          why = 'GitHub returned HTTP 403.';
        } else {
          title = 'Repository unavailable';
          why = `GitHub returned HTTP ${e.status}: it was deleted, made private, taken down, or your token lost access.`;
        }
        events.push(event(title, `${last}\n${why}`, `https://github.com/${last}`, RED));
      }
      return;
    }
    log(`[${key}] repository lookup failed (HTTP ${e.status}: ${e.message})`);
    return;
  }

  const full = info.full_name;
  const html = info.html_url;
  const def = info.default_branch;
  const meta = {
    owner: info.owner.login,
    name: info.name,
    private: !!info.private,
    archived: !!info.archived,
    disabled: !!info.disabled,
    default_branch: def,
    description: info.description || '',
  };
  let defaultChanged = false;
  if (first) {
    st = { id: info.id, meta, unavailable: false };
    state[key] = st;
    log(`[${key}] capturing baseline`);
  } else {
    if (st.unavailable) {
      st.unavailable = false;
      events.push(event('Repository is accessible again', full, html, GREEN));
    }
    events.push(...diffMeta(st.meta, meta, html, full));
    defaultChanged = st.meta.default_branch !== def;
    st.meta = meta;
  }
  st.label = full;

  await guarded(key, 'branches', async () => {
    const branches = await fetchBranches(cfg, full);
    const old = st.branches;
    if (old !== undefined) {
      const created = diffSet(Object.keys(branches), Object.keys(old)).sort();
      const deleted = diffSet(Object.keys(old), Object.keys(branches)).sort();
      const moved = Object.keys(branches)
        .filter((n) => n in old && branches[n] !== old[n] && n !== def)
        .sort();
      if (created.length) events.push(event(`${created.length} new branch(es)`, bullets(created, 10), `${html}/branches`, ORANGE));
      if (deleted.length) events.push(event(`${deleted.length} branch(es) deleted`, bullets(deleted, 10), `${html}/branches`, ORANGE));
      if (moved.length) events.push(event(`${moved.length} branch(es) updated`, bullets(moved, 10), `${html}/branches`, ORANGE));
    }
    st.branches = branches;
  });

  const commits = await guarded(key, 'commits', async () => {
    const list = await optionalList(cfg, `/repos/${full}/commits?per_page=30`);
    const seen = st.commits;
    const shas = list.map((c) => c.sha);
    if (seen !== undefined && !defaultChanged) {
      const fresh = list.filter((c) => !seen.includes(c.sha));
      if (fresh.length) {
        const lines = fresh.map(
          (c) => `${c.sha.slice(0, 7)} ${clip(c.commit.message.split('\n')[0], 80)} — ${c.commit.author.name}`
        );
        const count = fresh.length === list.length && list.length >= 30 ? `${fresh.length}+` : String(fresh.length);
        const url = fresh.length === 1 ? fresh[0].html_url : `${html}/commits/${def}`;
        events.push(event(`${count} new commit(s) on ${def}`, bullets(lines, 5), url, GREEN));
      }
    }
    st.commits = [...shas, ...(seen || []).filter((s) => !shas.includes(s))].slice(0, 200);
    return list;
  });

  await guarded(key, 'releases', async () => {
    const list = await optionalList(cfg, `/repos/${full}/releases?per_page=30`);
    const ids = list.map((r) => r.id);
    const seen = st.releases;
    if (seen !== undefined) {
      for (const r of list.filter((r) => !seen.includes(r.id))) {
        const label = r.name || r.tag_name;
        const flags = (r.prerelease ? ' (pre-release)' : '') + (r.draft ? ' (draft)' : '');
        events.push(event(`New release: ${clip(label, 100)}${flags}`, `Tag: ${r.tag_name}`, r.html_url, GREEN));
      }
    }
    st.releases = [...ids, ...(seen || []).filter((i) => !ids.includes(i))].slice(0, 200);
  });

  await guarded(key, 'pull requests', async () => {
    const pulls = await optionalList(cfg, `/repos/${full}/pulls?state=all&sort=created&direction=desc&per_page=30`);
    const last = st.last_pull;
    if (last !== undefined) {
      for (const p of pulls.filter((p) => p.number > last).sort((a, b) => a.number - b.number)) {
        const draft = p.draft ? ' (draft)' : '';
        events.push(
          event(`New pull request #${p.number}${draft}: ${clip(p.title, 150)}`, `${p.user.login}: ${p.head.ref} → ${p.base.ref}`, p.html_url, GREEN)
        );
      }
    }
    st.last_pull = Math.max(last || 0, ...pulls.map((p) => p.number));
  });

  await guarded(key, 'issues', async () => {
    const items = await optionalList(cfg, `/repos/${full}/issues?state=all&sort=created&direction=desc&per_page=30`);
    const issues = items.filter((i) => !i.pull_request);
    const last = st.last_issue;
    if (last !== undefined) {
      for (const i of issues.filter((i) => i.number > last).sort((a, b) => a.number - b.number)) {
        events.push(event(`New issue #${i.number}: ${clip(i.title, 150)}`, `Opened by ${i.user.login}`, i.html_url, GREEN));
      }
    }
    st.last_issue = Math.max(last || 0, ...issues.map((i) => i.number));
  });

  if (commits && commits.length) {
    const head = commits[0].sha;
    if (st.tree_head !== head) {
      await guarded(key, 'file tree', async () => {
        const tree = await gh(cfg, `/repos/${full}/git/trees/${head}?recursive=1`);
        if (tree.truncated) {
          log(`[${key}] file tree is too large for the API (truncated); file monitoring skipped`);
          return;
        }
        const files = {};
        for (const t of tree.tree) if (t.type === 'blob') files[t.path] = t.sha;
        const old = st.files;
        if (old !== undefined && !defaultChanged) {
          const added = diffSet(Object.keys(files), Object.keys(old)).sort();
          const removed = diffSet(Object.keys(old), Object.keys(files)).sort();
          const modified = Object.keys(files)
            .filter((p) => p in old && files[p] !== old[p])
            .sort();
          if (added.length) events.push(event(`${added.length} new file(s)`, bullets(added, 10), `${html}/tree/${def}`, GREEN));
          if (modified.length) events.push(event(`${modified.length} file(s) modified`, bullets(modified, 10), `${html}/commit/${head}`, ORANGE));
          if (removed.length) events.push(event(`${removed.length} file(s) deleted`, bullets(removed, 10), `${html}/tree/${def}`, ORANGE));
        }
        st.files = files;
        st.tree_head = head;
      });
    }
  }
}

async function runCycle(cfg, state, statePath) {
  let wait = 0;
  for (const [owner, name] of cfg.repos) {
    const key = `${owner}/${name}`.toLowerCase();
    const events = [];
    try {
      await checkRepo(cfg, key, owner, name, state, events);
    } catch (e) {
      if (e instanceof RateLimited) {
        wait = e.wait;
        log(`GitHub rate limit reached; pausing ${e.wait}s`);
        await dispatch(cfg, (state[key] || {}).label || key, events);
        break;
      }
      log(`[${key}] check failed: ${e.message}`);
    }
    await dispatch(cfg, (state[key] || {}).label || key, events);
  }
  saveState(statePath, state);
  return wait;
}

async function main() {
  const args = process.argv.slice(2).filter((a) => !a.startsWith('--'));
  const flags = new Set(process.argv.slice(2).filter((a) => a.startsWith('--')));
  const configPath = args[0] || 'config.json';
  let cfg;
  try {
    cfg = loadConfig(configPath);
  } catch (e) {
    console.error(`config error: ${e.message}`);
    process.exit(1);
  }
  const statePath = path.join(path.dirname(path.resolve(configPath)), 'state.json');

  if (flags.has('--test')) {
    await dispatch(cfg, 'Repo Monitor', [event('Test notification', 'If you can see this, notifications work.', '', BLUE)]);
    await sleep(1500);
    return;
  }

  if (!cfg.token) log('warning: no GitHub token set; unauthenticated requests are limited to 60/hour');
  log(`monitoring ${cfg.repos.length} repo(s) every ${cfg.interval}s`);
  const state = loadState(statePath);
  process.on('SIGINT', () => {
    log('stopped');
    process.exit(0);
  });

  for (;;) {
    const started = Date.now();
    const wait = await runCycle(cfg, state, statePath);
    if (flags.has('--once')) {
      await sleep(1500);
      return;
    }
    await sleep(Math.max(cfg.interval * 1000 - (Date.now() - started), wait * 1000, 1000));
  }
}

main();
