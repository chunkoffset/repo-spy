#!/usr/bin/env python3
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

try:
    from plyer import notification as plyer_notification
except Exception:
    plyer_notification = None

UA = "repo-monitor/1.0"
GREEN, ORANGE, RED, BLUE = 0x57F287, 0xFAA61A, 0xED4245, 0x5865F2
MAX_REPOS = 10
MIN_INTERVAL = 30

etag_cache = {}


class RateLimited(Exception):
    def __init__(self, wait):
        super().__init__(f"rate limited, retry in {wait}s")
        self.wait = wait


class ApiError(Exception):
    def __init__(self, status, message):
        super().__init__(message)
        self.status = status


def log(msg):
    print(f"{datetime.now().strftime('%Y-%m-%d %H:%M:%S')}  {msg}", flush=True)


def clip(s, n):
    s = s or ""
    return s if len(s) <= n else s[: n - 1] + "…"


def bullets(items, limit):
    out = "\n".join("• " + i for i in items[:limit])
    if len(items) > limit:
        out += f"\n… and {len(items) - limit} more"
    return out


def event(title, body, url, color):
    return {"title": title, "body": body, "url": url, "color": color}


def parse_repo(s):
    s = s.strip()
    s = re.sub(r"^https?://github\.com/", "", s)
    s = re.sub(r"\.git$", "", s).strip("/")
    m = re.fullmatch(r"([\w.-]+)/([\w.-]+)", s)
    if not m:
        raise ValueError(f"invalid repo '{s}' (expected owner/name)")
    return m.group(1), m.group(2)


def load_config(path):
    with open(path, "r", encoding="utf-8") as f:
        raw = json.load(f)
    repos = [parse_repo(r) for r in raw.get("repos", [])]
    if not 1 <= len(repos) <= MAX_REPOS:
        raise ValueError(f"'repos' must contain between 1 and {MAX_REPOS} entries")
    interval = int(raw.get("poll_interval_seconds", 60))
    if interval < MIN_INTERVAL:
        raise ValueError(f"'poll_interval_seconds' must be at least {MIN_INTERVAL}")
    return {
        "token": os.environ.get("GITHUB_TOKEN") or raw.get("github_token", ""),
        "webhook": raw.get("discord_webhook_url", ""),
        "desktop": bool(raw.get("desktop_notifications", True)),
        "interval": interval,
        "api": raw.get("api_url", "https://api.github.com").rstrip("/"),
        "repos": repos,
    }


def load_state(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except FileNotFoundError:
        return {}
    except ValueError:
        log(f"state file {path} is corrupt, starting fresh")
        return {}


def save_state(path, state):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(state, f)
    os.replace(tmp, path)


def gh(cfg, path):
    url = cfg["api"] + path
    cacheable = "/git/trees/" not in path
    headers = {
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": UA,
    }
    if cfg["token"]:
        headers["Authorization"] = f"Bearer {cfg['token']}"
    cached = etag_cache.get(url) if cacheable else None
    if cached:
        headers["If-None-Match"] = cached[0]
    try:
        with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=30) as r:
            body = json.loads(r.read().decode("utf-8"))
            etag = r.headers.get("ETag")
            if etag and cacheable:
                etag_cache[url] = (etag, body)
            return body
    except urllib.error.HTTPError as e:
        if e.code == 304 and cached:
            return cached[1]
        raw = e.read().decode("utf-8", "replace")
        try:
            msg = json.loads(raw).get("message", raw)
        except ValueError:
            msg = raw
        h = e.headers
        if e.code in (403, 429) and (
            h.get("X-RateLimit-Remaining") == "0" or h.get("Retry-After") or "rate limit" in msg.lower()
        ):
            if h.get("Retry-After"):
                wait = int(h["Retry-After"])
            elif h.get("X-RateLimit-Reset"):
                wait = max(int(h["X-RateLimit-Reset"]) - int(time.time()), 1)
            else:
                wait = 60
            raise RateLimited(wait + 1)
        raise ApiError(e.code, msg)


def desktop_notify(title, body):
    if plyer_notification is None:
        log("plyer is not installed, skipping desktop notification")
        return
    try:
        plyer_notification.notify(
            title=clip(title, 60), message=clip(body, 250) or " ", app_name="Repo Monitor", timeout=10
        )
    except Exception as e:
        log(f"desktop notification failed: {e}")


def post_discord(url, payload):
    data = json.dumps(payload).encode("utf-8")
    for attempt in range(2):
        req = urllib.request.Request(
            url, data=data, method="POST", headers={"Content-Type": "application/json", "User-Agent": UA}
        )
        try:
            urllib.request.urlopen(req, timeout=20).read()
            return
        except urllib.error.HTTPError as e:
            if e.code == 429 and attempt == 0:
                try:
                    wait = float(json.loads(e.read().decode("utf-8")).get("retry_after", 2))
                except Exception:
                    wait = 2
                time.sleep(min(wait, 30))
                continue
            log(f"discord webhook failed: HTTP {e.code}")
            return
        except Exception as e:
            log(f"discord webhook failed: {e}")
            return


def dispatch(cfg, label, events):
    if not events:
        return
    for e in events:
        log(f"[{label}] {e['title']}")
        if cfg["desktop"]:
            desktop_notify(f"{label} — {e['title']}", e["body"])
    if cfg["webhook"]:
        now = datetime.now(timezone.utc).isoformat()
        for i in range(0, len(events), 10):
            embeds = []
            for e in events[i : i + 10]:
                embed = {
                    "title": clip(e["title"], 256),
                    "description": clip(e["body"], 3900),
                    "color": e["color"],
                    "footer": {"text": label},
                    "timestamp": now,
                }
                if e["url"]:
                    embed["url"] = e["url"]
                embeds.append(embed)
            post_discord(
                cfg["webhook"],
                {"username": "Repo Monitor", "allowed_mentions": {"parse": []}, "embeds": embeds},
            )


def diff_meta(old, new, url, full):
    ev = []
    if old["owner"].lower() != new["owner"].lower():
        ev.append(event("Repository owner changed", f"{old['owner']} → {new['owner']}\nNow: {full}", url, RED))
    if old["name"] != new["name"]:
        ev.append(event("Repository renamed", f"{old['name']} → {new['name']}\nNow: {full}", url, RED))
    if old["private"] != new["private"]:
        ev.append(event("Repository is now private" if new["private"] else "Repository is now public", full, url, RED))
    if old["archived"] != new["archived"]:
        ev.append(event("Repository archived" if new["archived"] else "Repository unarchived", full, url, ORANGE))
    if old["disabled"] != new["disabled"]:
        ev.append(event("Repository disabled" if new["disabled"] else "Repository re-enabled", full, url, RED))
    if old["default_branch"] != new["default_branch"]:
        ev.append(event("Default branch changed", f"{old['default_branch']} → {new['default_branch']}", url, ORANGE))
    if old["description"] != new["description"]:
        ev.append(event("Description changed", f"“{clip(old['description'], 300)}” → “{clip(new['description'], 300)}”", url, ORANGE))
    return ev


def fetch_branches(cfg, full):
    out = {}
    for page in range(1, 6):
        data = gh(cfg, f"/repos/{full}/branches?per_page=100&page={page}")
        for b in data:
            out[b["name"]] = b["commit"]["sha"]
        if len(data) < 100:
            break
    return out


def optional_list(cfg, path):
    try:
        return gh(cfg, path)
    except ApiError as e:
        if e.status in (409, 410):
            return []
        raise


def check_repo(cfg, key, owner, name, state, events):
    st = state.get(key)
    first = st is None
    try:
        info = gh(cfg, f"/repos/{owner}/{name}" if first else f"/repositories/{st['id']}")
    except ApiError as e:
        if first:
            log(f"[{key}] cannot access repository (HTTP {e.status}: {e}); will retry")
            return
        if e.status in (403, 404, 410, 451):
            if not st.get("unavailable"):
                st["unavailable"] = True
                last = st.get("label", key)
                if e.status == 451:
                    title, why = "Repository taken down (legal block)", "GitHub returned HTTP 451 (blocked for legal reasons, e.g. DMCA)."
                elif e.status == 403:
                    title, why = "Repository access blocked", "GitHub returned HTTP 403."
                else:
                    title, why = "Repository unavailable", f"GitHub returned HTTP {e.status}: it was deleted, made private, taken down, or your token lost access."
                events.append(event(title, f"{last}\n{why}", f"https://github.com/{last}", RED))
            return
        log(f"[{key}] repository lookup failed (HTTP {e.status}: {e})")
        return

    full, html = info["full_name"], info["html_url"]
    default = info["default_branch"]
    meta = {
        "owner": info["owner"]["login"],
        "name": info["name"],
        "private": bool(info["private"]),
        "archived": bool(info.get("archived")),
        "disabled": bool(info.get("disabled")),
        "default_branch": default,
        "description": info.get("description") or "",
    }
    default_changed = False
    if first:
        st = {"id": info["id"], "meta": meta, "unavailable": False}
        state[key] = st
        log(f"[{key}] capturing baseline")
    else:
        if st.get("unavailable"):
            st["unavailable"] = False
            events.append(event("Repository is accessible again", full, html, GREEN))
        events.extend(diff_meta(st["meta"], meta, html, full))
        default_changed = st["meta"]["default_branch"] != default
        st["meta"] = meta
    st["label"] = full

    try:
        branches = fetch_branches(cfg, full)
        old = st.get("branches")
        if old is not None:
            created = sorted(set(branches) - set(old))
            deleted = sorted(set(old) - set(branches))
            moved = sorted(n for n in set(branches) & set(old) if branches[n] != old[n] and n != default)
            if created:
                events.append(event(f"{len(created)} new branch(es)", bullets(created, 10), f"{html}/branches", ORANGE))
            if deleted:
                events.append(event(f"{len(deleted)} branch(es) deleted", bullets(deleted, 10), f"{html}/branches", ORANGE))
            if moved:
                events.append(event(f"{len(moved)} branch(es) updated", bullets(moved, 10), f"{html}/branches", ORANGE))
        st["branches"] = branches
    except ApiError as e:
        log(f"[{key}] branches check failed (HTTP {e.status}: {e})")

    commits = None
    try:
        commits = optional_list(cfg, f"/repos/{full}/commits?per_page=30")
        seen = st.get("commits")
        shas = [c["sha"] for c in commits]
        if seen is not None and not default_changed:
            seen_set = set(seen)
            new = [c for c in commits if c["sha"] not in seen_set]
            if new:
                lines = [
                    f"{c['sha'][:7]} {clip(c['commit']['message'].split(chr(10))[0], 80)} — {c['commit']['author']['name']}"
                    for c in new
                ]
                count = f"{len(new)}+" if len(new) == len(commits) and len(commits) >= 30 else str(len(new))
                url = new[0]["html_url"] if len(new) == 1 else f"{html}/commits/{default}"
                events.append(event(f"{count} new commit(s) on {default}", bullets(lines, 5), url, GREEN))
        st["commits"] = (shas + [s for s in (seen or []) if s not in shas])[:200]
    except ApiError as e:
        commits = None
        log(f"[{key}] commits check failed (HTTP {e.status}: {e})")

    try:
        releases = optional_list(cfg, f"/repos/{full}/releases?per_page=30")
        ids = [r["id"] for r in releases]
        seen = st.get("releases")
        if seen is not None:
            new = [r for r in releases if r["id"] not in set(seen)]
            for r in new:
                label = r.get("name") or r["tag_name"]
                flags = " (pre-release)" if r.get("prerelease") else ""
                flags += " (draft)" if r.get("draft") else ""
                events.append(event(f"New release: {clip(label, 100)}{flags}", f"Tag: {r['tag_name']}", r["html_url"], GREEN))
        st["releases"] = (ids + [i for i in (seen or []) if i not in ids])[:200]
    except ApiError as e:
        log(f"[{key}] releases check failed (HTTP {e.status}: {e})")

    try:
        pulls = optional_list(cfg, f"/repos/{full}/pulls?state=all&sort=created&direction=desc&per_page=30")
        last = st.get("last_pull")
        if last is not None:
            for p in sorted((p for p in pulls if p["number"] > last), key=lambda p: p["number"]):
                draft = " (draft)" if p.get("draft") else ""
                body = f"{p['user']['login']}: {p['head']['ref']} → {p['base']['ref']}"
                events.append(event(f"New pull request #{p['number']}{draft}: {clip(p['title'], 150)}", body, p["html_url"], GREEN))
        st["last_pull"] = max([p["number"] for p in pulls] + [last or 0])
    except ApiError as e:
        log(f"[{key}] pull requests check failed (HTTP {e.status}: {e})")

    try:
        items = optional_list(cfg, f"/repos/{full}/issues?state=all&sort=created&direction=desc&per_page=30")
        issues = [i for i in items if "pull_request" not in i]
        last = st.get("last_issue")
        if last is not None:
            for i in sorted((i for i in issues if i["number"] > last), key=lambda i: i["number"]):
                events.append(event(f"New issue #{i['number']}: {clip(i['title'], 150)}", f"Opened by {i['user']['login']}", i["html_url"], GREEN))
        st["last_issue"] = max([i["number"] for i in issues] + [last or 0])
    except ApiError as e:
        log(f"[{key}] issues check failed (HTTP {e.status}: {e})")

    if commits:
        head = commits[0]["sha"]
        if st.get("tree_head") != head:
            try:
                tree = gh(cfg, f"/repos/{full}/git/trees/{head}?recursive=1")
                if tree.get("truncated"):
                    log(f"[{key}] file tree is too large for the API (truncated); file monitoring skipped")
                else:
                    files = {t["path"]: t["sha"] for t in tree["tree"] if t["type"] == "blob"}
                    old = st.get("files")
                    if old is not None and not default_changed:
                        added = sorted(set(files) - set(old))
                        removed = sorted(set(old) - set(files))
                        modified = sorted(p for p in set(files) & set(old) if files[p] != old[p])
                        if added:
                            events.append(event(f"{len(added)} new file(s)", bullets(added, 10), f"{html}/tree/{default}", GREEN))
                        if modified:
                            events.append(event(f"{len(modified)} file(s) modified", bullets(modified, 10), f"{html}/commit/{head}", ORANGE))
                        if removed:
                            events.append(event(f"{len(removed)} file(s) deleted", bullets(removed, 10), f"{html}/tree/{default}", ORANGE))
                    st["files"] = files
                    st["tree_head"] = head
            except ApiError as e:
                log(f"[{key}] file tree check failed (HTTP {e.status}: {e})")


def run_cycle(cfg, state, state_path):
    wait = 0
    for owner, name in cfg["repos"]:
        key = f"{owner}/{name}".lower()
        events = []
        try:
            check_repo(cfg, key, owner, name, state, events)
        except RateLimited as r:
            wait = r.wait
            log(f"GitHub rate limit reached; pausing {r.wait}s")
            break
        except Exception as e:
            log(f"[{key}] check failed: {e}")
        finally:
            label = state.get(key, {}).get("label", key)
            dispatch(cfg, label, events)
    save_state(state_path, state)
    return wait


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    flags = {a for a in sys.argv[1:] if a.startswith("--")}
    config_path = args[0] if args else "config.json"
    try:
        cfg = load_config(config_path)
    except Exception as e:
        sys.exit(f"config error: {e}")
    state_path = os.path.join(os.path.dirname(os.path.abspath(config_path)), "state.json")

    if "--test" in flags:
        dispatch(cfg, "Repo Monitor", [event("Test notification", "If you can see this, notifications work.", "", BLUE)])
        return

    if not cfg["token"]:
        log("warning: no GitHub token set; unauthenticated requests are limited to 60/hour")
    log(f"monitoring {len(cfg['repos'])} repo(s) every {cfg['interval']}s")
    state = load_state(state_path)
    try:
        while True:
            started = time.time()
            wait = run_cycle(cfg, state, state_path)
            if "--once" in flags:
                return
            time.sleep(max(cfg["interval"] - (time.time() - started), wait, 1))
    except KeyboardInterrupt:
        log("stopped")


if __name__ == "__main__":
    main()
