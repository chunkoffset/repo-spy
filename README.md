# repo-spy

monitors gitHub repos and sends a notification or discord webhook message when something changes.
## Setup Information:

1. Copy `config.example.json` to `config.json` (same folder as the program) and fill it in.
2. Install and run:

| Required | Install | Run |
|---|---|---|
| Python 3.8+ | `pip install -r requirements.txt` | `python monitor.py` |
| Node 18+ | `npm install` | `node monitor.js` |
| Go | `go mod tidy` | `go run .` |

## Config Information:

| Key | Meaning |
|---|---|
| `github_token` | Mostly recommended. |
| `discord_webhook_url` | Leave empty to disable discord webhook messages |
| `desktop_notifications` | `true` / `false`. |
| `repos` | 1 to 10, `owner/name` or a full GitHub URL. |
| `api_url` | Only for GitHub Enterprise (default `https://api.github.com`). |

State is stored in `state.json` next to the config, so changes that happen while repo-spy is stopped are reported on the next start.

## What it monitors:

- New commits, new releases, new pull requests, new issues
- New files, modified files, deleted files
- Branches created, deleted, or updated
- Repo renamed, owner changed, made private/public, archived/unarchived, disabled, default branch changed, description changed
