package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gen2brain/beeep"
)

const (
	userAgent   = "repo-monitor/1.0"
	colorGreen  = 0x57F287
	colorOrange = 0xFAA61A
	colorRed    = 0xED4245
	colorBlue   = 0x5865F2
	maxRepos    = 10
	minInterval = 30
)

type repoRef struct{ Owner, Name string }

type Config struct {
	Token    string
	Webhook  string
	Desktop  bool
	Interval time.Duration
	API      string
	Repos    []repoRef
}

type Event struct {
	Title string
	Body  string
	URL   string
	Color int
}

type Meta struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	Disabled      bool   `json:"disabled"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description"`
}

type RepoState struct {
	ID          int64             `json:"id"`
	Label       string            `json:"label"`
	Unavailable bool              `json:"unavailable"`
	Meta        Meta              `json:"meta"`
	Branches    map[string]string `json:"branches"`
	Commits     []string          `json:"commits"`
	Releases    []int64           `json:"releases"`
	LastPull    *int              `json:"last_pull"`
	LastIssue   *int              `json:"last_issue"`
	TreeHead    string            `json:"tree_head"`
	Files       map[string]string `json:"files"`
}

type ghRepo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
	Private       bool    `json:"private"`
	Archived      bool    `json:"archived"`
	Disabled      bool    `json:"disabled"`
	DefaultBranch string  `json:"default_branch"`
	Description   *string `json:"description"`
	HTMLURL       string  `json:"html_url"`
}

type ghBranch struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type ghCommit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"commit"`
}

type ghRelease struct {
	ID         int64   `json:"id"`
	TagName    string  `json:"tag_name"`
	Name       *string `json:"name"`
	HTMLURL    string  `json:"html_url"`
	Prerelease bool    `json:"prerelease"`
	Draft      bool    `json:"draft"`
}

type ghUser struct {
	Login string `json:"login"`
}

type ghPull struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	User    ghUser `json:"user"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type ghIssue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	HTMLURL     string          `json:"html_url"`
	User        ghUser          `json:"user"`
	PullRequest json.RawMessage `json:"pull_request"`
}

type ghTree struct {
	Truncated bool `json:"truncated"`
	Tree      []struct {
		Path string `json:"path"`
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"tree"`
}

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg) }

type rateLimited struct{ Wait int }

func (e *rateLimited) Error() string { return fmt.Sprintf("rate limited, retry in %ds", e.Wait) }

type cacheEntry struct {
	etag string
	body []byte
}

type Monitor struct {
	cfg       *Config
	client    *http.Client
	cache     map[string]cacheEntry
	state     map[string]*RepoState
	statePath string
}

func logf(format string, a ...interface{}) {
	fmt.Printf("%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func bullets(items []string, limit int) string {
	n := len(items)
	if n > limit {
		n = limit
	}
	lines := make([]string, 0, n+1)
	for _, it := range items[:n] {
		lines = append(lines, "• "+it)
	}
	if len(items) > limit {
		lines = append(lines, fmt.Sprintf("… and %d more", len(items)-limit))
	}
	return strings.Join(lines, "\n")
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func toSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, it := range items {
		s[it] = true
	}
	return s
}

func missingKeys(a, b map[string]string) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func isGone(status int) bool {
	return status == 403 || status == 404 || status == 410 || status == 451
}

var repoPattern = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)$`)

func parseRepo(s string) (repoRef, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://github.com/"), "http://github.com/")
	s = strings.Trim(strings.TrimSuffix(s, ".git"), "/")
	m := repoPattern.FindStringSubmatch(s)
	if m == nil {
		return repoRef{}, fmt.Errorf("invalid repo '%s' (expected owner/name)", s)
	}
	return repoRef{Owner: m[1], Name: m[2]}, nil
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Token    string   `json:"github_token"`
		Webhook  string   `json:"discord_webhook_url"`
		Desktop  *bool    `json:"desktop_notifications"`
		Interval *int     `json:"poll_interval_seconds"`
		APIURL   string   `json:"api_url"`
		Repos    []string `json:"repos"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	cfg := &Config{Token: raw.Token, Webhook: raw.Webhook, Desktop: true, API: "https://api.github.com"}
	if env := os.Getenv("GITHUB_TOKEN"); env != "" {
		cfg.Token = env
	}
	if raw.Desktop != nil {
		cfg.Desktop = *raw.Desktop
	}
	if raw.APIURL != "" {
		cfg.API = strings.TrimRight(raw.APIURL, "/")
	}
	interval := 60
	if raw.Interval != nil {
		interval = *raw.Interval
	}
	if interval < minInterval {
		return nil, fmt.Errorf("'poll_interval_seconds' must be at least %d", minInterval)
	}
	cfg.Interval = time.Duration(interval) * time.Second
	if len(raw.Repos) < 1 || len(raw.Repos) > maxRepos {
		return nil, fmt.Errorf("'repos' must contain between 1 and %d entries", maxRepos)
	}
	for _, r := range raw.Repos {
		ref, err := parseRepo(r)
		if err != nil {
			return nil, err
		}
		cfg.Repos = append(cfg.Repos, ref)
	}
	return cfg, nil
}

func loadState(path string) map[string]*RepoState {
	state := map[string]*RepoState{}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logf("state file %s is unreadable, starting fresh", path)
		}
		return state
	}
	if err := json.Unmarshal(data, &state); err != nil || state == nil {
		logf("state file %s is corrupt, starting fresh", path)
		return map[string]*RepoState{}
	}
	return state
}

func (m *Monitor) saveState() {
	data, err := json.Marshal(m.state)
	if err != nil {
		logf("could not encode state: %v", err)
		return
	}
	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		logf("could not write state: %v", err)
		return
	}
	if err := os.Rename(tmp, m.statePath); err != nil {
		logf("could not write state: %v", err)
	}
}

func (m *Monitor) get(ctx context.Context, path string, out interface{}) error {
	url := m.cfg.API + path
	cacheable := !strings.Contains(path, "/git/trees/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)
	if m.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.Token)
	}
	cached, hasCache := m.cache[url]
	if cacheable && hasCache {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusNotModified && cacheable && hasCache {
		return json.Unmarshal(cached.body, out)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if etag := resp.Header.Get("ETag"); etag != "" && cacheable {
			m.cache[url] = cacheEntry{etag: etag, body: body}
		}
		return json.Unmarshal(body, out)
	}

	msg := string(body)
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		msg = e.Message
	}
	if resp.StatusCode == 403 || resp.StatusCode == 429 {
		retryAfter := resp.Header.Get("Retry-After")
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || retryAfter != "" || strings.Contains(strings.ToLower(msg), "rate limit") {
			wait := 60
			if retryAfter != "" {
				if n, err := strconv.Atoi(retryAfter); err == nil {
					wait = n
				}
			} else if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
				if n, err := strconv.ParseInt(reset, 10, 64); err == nil {
					wait = int(n - time.Now().Unix())
					if wait < 1 {
						wait = 1
					}
				}
			}
			return &rateLimited{Wait: wait + 1}
		}
	}
	return &apiError{Status: resp.StatusCode, Msg: msg}
}

func (m *Monitor) getOptional(ctx context.Context, path string, out interface{}) error {
	err := m.get(ctx, path, out)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == 409 || ae.Status == 410) {
		return json.Unmarshal([]byte("[]"), out)
	}
	return err
}

func fatal(key, what string, err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		logf("[%s] %s check failed (%v)", key, what, ae)
		return false
	}
	return true
}

func (m *Monitor) desktopNotify(title, body string) {
	msg := clip(body, 250)
	if msg == "" {
		msg = " "
	}
	if err := beeep.Notify(clip(title, 60), msg, ""); err != nil {
		logf("desktop notification failed: %v", err)
	}
}

func (m *Monitor) postDiscord(ctx context.Context, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		logf("discord webhook failed: %v", err)
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.Webhook, bytes.NewReader(data))
		if err != nil {
			logf("discord webhook failed: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		resp, err := m.client.Do(req)
		if err != nil {
			logf("discord webhook failed: %v", err)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return
		}
		if resp.StatusCode == 429 && attempt == 0 {
			wait := 2.0
			var r struct {
				RetryAfter float64 `json:"retry_after"`
			}
			if json.Unmarshal(body, &r) == nil && r.RetryAfter > 0 {
				wait = r.RetryAfter
			}
			if wait > 30 {
				wait = 30
			}
			sleepCtx(ctx, time.Duration(wait*float64(time.Second)))
			continue
		}
		logf("discord webhook failed: HTTP %d", resp.StatusCode)
		return
	}
}

func (m *Monitor) dispatch(ctx context.Context, label string, events []Event) {
	if len(events) == 0 {
		return
	}
	for _, e := range events {
		logf("[%s] %s", label, e.Title)
		if m.cfg.Desktop {
			m.desktopNotify(label+" — "+e.Title, e.Body)
		}
	}
	if m.cfg.Webhook == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < len(events); i += 10 {
		end := i + 10
		if end > len(events) {
			end = len(events)
		}
		embeds := make([]map[string]interface{}, 0, end-i)
		for _, e := range events[i:end] {
			embed := map[string]interface{}{
				"title":       clip(e.Title, 256),
				"description": clip(e.Body, 3900),
				"color":       e.Color,
				"footer":      map[string]string{"text": label},
				"timestamp":   now,
			}
			if e.URL != "" {
				embed["url"] = e.URL
			}
			embeds = append(embeds, embed)
		}
		m.postDiscord(ctx, map[string]interface{}{
			"username":         "Repo Monitor",
			"allowed_mentions": map[string]interface{}{"parse": []string{}},
			"embeds":           embeds,
		})
	}
}

func diffMeta(old, cur Meta, url, full string) []Event {
	var ev []Event
	if !strings.EqualFold(old.Owner, cur.Owner) {
		ev = append(ev, Event{"Repository owner changed", fmt.Sprintf("%s → %s\nNow: %s", old.Owner, cur.Owner, full), url, colorRed})
	}
	if old.Name != cur.Name {
		ev = append(ev, Event{"Repository renamed", fmt.Sprintf("%s → %s\nNow: %s", old.Name, cur.Name, full), url, colorRed})
	}
	if old.Private != cur.Private {
		title := "Repository is now public"
		if cur.Private {
			title = "Repository is now private"
		}
		ev = append(ev, Event{title, full, url, colorRed})
	}
	if old.Archived != cur.Archived {
		title := "Repository unarchived"
		if cur.Archived {
			title = "Repository archived"
		}
		ev = append(ev, Event{title, full, url, colorOrange})
	}
	if old.Disabled != cur.Disabled {
		title := "Repository re-enabled"
		if cur.Disabled {
			title = "Repository disabled"
		}
		ev = append(ev, Event{title, full, url, colorRed})
	}
	if old.DefaultBranch != cur.DefaultBranch {
		ev = append(ev, Event{"Default branch changed", fmt.Sprintf("%s → %s", old.DefaultBranch, cur.DefaultBranch), url, colorOrange})
	}
	if old.Description != cur.Description {
		ev = append(ev, Event{"Description changed", fmt.Sprintf("“%s” → “%s”", clip(old.Description, 300), clip(cur.Description, 300)), url, colorOrange})
	}
	return ev
}

func (m *Monitor) checkBranches(ctx context.Context, st *RepoState, full, html, def string, events *[]Event) error {
	branches := map[string]string{}
	for page := 1; page <= 5; page++ {
		var data []ghBranch
		if err := m.get(ctx, fmt.Sprintf("/repos/%s/branches?per_page=100&page=%d", full, page), &data); err != nil {
			return err
		}
		for _, b := range data {
			branches[b.Name] = b.Commit.SHA
		}
		if len(data) < 100 {
			break
		}
	}
	if st.Branches != nil {
		created := missingKeys(branches, st.Branches)
		deleted := missingKeys(st.Branches, branches)
		var moved []string
		for n, sha := range branches {
			if old, ok := st.Branches[n]; ok && old != sha && n != def {
				moved = append(moved, n)
			}
		}
		sort.Strings(moved)
		if len(created) > 0 {
			*events = append(*events, Event{fmt.Sprintf("%d new branch(es)", len(created)), bullets(created, 10), html + "/branches", colorOrange})
		}
		if len(deleted) > 0 {
			*events = append(*events, Event{fmt.Sprintf("%d branch(es) deleted", len(deleted)), bullets(deleted, 10), html + "/branches", colorOrange})
		}
		if len(moved) > 0 {
			*events = append(*events, Event{fmt.Sprintf("%d branch(es) updated", len(moved)), bullets(moved, 10), html + "/branches", colorOrange})
		}
	}
	st.Branches = branches
	return nil
}

func (m *Monitor) checkCommits(ctx context.Context, st *RepoState, full, html, def string, defaultChanged bool, events *[]Event) ([]ghCommit, error) {
	var list []ghCommit
	if err := m.getOptional(ctx, "/repos/"+full+"/commits?per_page=30", &list); err != nil {
		return nil, err
	}
	shas := make([]string, len(list))
	for i, c := range list {
		shas[i] = c.SHA
	}
	if st.Commits != nil && !defaultChanged {
		seen := toSet(st.Commits)
		var fresh []ghCommit
		for _, c := range list {
			if !seen[c.SHA] {
				fresh = append(fresh, c)
			}
		}
		if len(fresh) > 0 {
			lines := make([]string, len(fresh))
			for i, c := range fresh {
				first := strings.SplitN(c.Commit.Message, "\n", 2)[0]
				lines[i] = fmt.Sprintf("%s %s — %s", shortSHA(c.SHA), clip(first, 80), c.Commit.Author.Name)
			}
			count := strconv.Itoa(len(fresh))
			if len(fresh) == len(list) && len(list) >= 30 {
				count += "+"
			}
			url := html + "/commits/" + def
			if len(fresh) == 1 {
				url = fresh[0].HTMLURL
			}
			*events = append(*events, Event{fmt.Sprintf("%s new commit(s) on %s", count, def), bullets(lines, 5), url, colorGreen})
		}
	}
	merged := append([]string{}, shas...)
	inNew := toSet(shas)
	for _, s := range st.Commits {
		if !inNew[s] {
			merged = append(merged, s)
		}
	}
	if len(merged) > 200 {
		merged = merged[:200]
	}
	st.Commits = merged
	return list, nil
}

func (m *Monitor) checkReleases(ctx context.Context, st *RepoState, full string, events *[]Event) error {
	var list []ghRelease
	if err := m.getOptional(ctx, "/repos/"+full+"/releases?per_page=30", &list); err != nil {
		return err
	}
	ids := make([]int64, len(list))
	inNew := map[int64]bool{}
	for i, r := range list {
		ids[i] = r.ID
		inNew[r.ID] = true
	}
	if st.Releases != nil {
		seen := map[int64]bool{}
		for _, id := range st.Releases {
			seen[id] = true
		}
		for _, r := range list {
			if seen[r.ID] {
				continue
			}
			label := r.TagName
			if r.Name != nil && *r.Name != "" {
				label = *r.Name
			}
			flags := ""
			if r.Prerelease {
				flags += " (pre-release)"
			}
			if r.Draft {
				flags += " (draft)"
			}
			*events = append(*events, Event{fmt.Sprintf("New release: %s%s", clip(label, 100), flags), "Tag: " + r.TagName, r.HTMLURL, colorGreen})
		}
	}
	merged := append([]int64{}, ids...)
	for _, id := range st.Releases {
		if !inNew[id] {
			merged = append(merged, id)
		}
	}
	if len(merged) > 200 {
		merged = merged[:200]
	}
	st.Releases = merged
	return nil
}

func (m *Monitor) checkPulls(ctx context.Context, st *RepoState, full string, events *[]Event) error {
	var pulls []ghPull
	if err := m.getOptional(ctx, "/repos/"+full+"/pulls?state=all&sort=created&direction=desc&per_page=30", &pulls); err != nil {
		return err
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
	last := 0
	if st.LastPull != nil {
		last = *st.LastPull
	}
	highest := last
	for _, p := range pulls {
		if p.Number > highest {
			highest = p.Number
		}
		if st.LastPull == nil || p.Number <= last {
			continue
		}
		draft := ""
		if p.Draft {
			draft = " (draft)"
		}
		*events = append(*events, Event{
			fmt.Sprintf("New pull request #%d%s: %s", p.Number, draft, clip(p.Title, 150)),
			fmt.Sprintf("%s: %s → %s", p.User.Login, p.Head.Ref, p.Base.Ref),
			p.HTMLURL, colorGreen,
		})
	}
	st.LastPull = &highest
	return nil
}

func (m *Monitor) checkIssues(ctx context.Context, st *RepoState, full string, events *[]Event) error {
	var items []ghIssue
	if err := m.getOptional(ctx, "/repos/"+full+"/issues?state=all&sort=created&direction=desc&per_page=30", &items); err != nil {
		return err
	}
	var issues []ghIssue
	for _, it := range items {
		if len(it.PullRequest) == 0 || string(it.PullRequest) == "null" {
			issues = append(issues, it)
		}
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	last := 0
	if st.LastIssue != nil {
		last = *st.LastIssue
	}
	highest := last
	for _, it := range issues {
		if it.Number > highest {
			highest = it.Number
		}
		if st.LastIssue == nil || it.Number <= last {
			continue
		}
		*events = append(*events, Event{fmt.Sprintf("New issue #%d: %s", it.Number, clip(it.Title, 150)), "Opened by " + it.User.Login, it.HTMLURL, colorGreen})
	}
	st.LastIssue = &highest
	return nil
}

func (m *Monitor) checkFiles(ctx context.Context, key string, st *RepoState, full, html, def, head string, defaultChanged bool, events *[]Event) error {
	var tree ghTree
	if err := m.get(ctx, fmt.Sprintf("/repos/%s/git/trees/%s?recursive=1", full, head), &tree); err != nil {
		return err
	}
	if tree.Truncated {
		logf("[%s] file tree is too large for the API (truncated); file monitoring skipped", key)
		return nil
	}
	files := map[string]string{}
	for _, t := range tree.Tree {
		if t.Type == "blob" {
			files[t.Path] = t.SHA
		}
	}
	if st.Files != nil && !defaultChanged {
		added := missingKeys(files, st.Files)
		removed := missingKeys(st.Files, files)
		var modified []string
		for p, sha := range files {
			if old, ok := st.Files[p]; ok && old != sha {
				modified = append(modified, p)
			}
		}
		sort.Strings(modified)
		if len(added) > 0 {
			*events = append(*events, Event{fmt.Sprintf("%d new file(s)", len(added)), bullets(added, 10), html + "/tree/" + def, colorGreen})
		}
		if len(modified) > 0 {
			*events = append(*events, Event{fmt.Sprintf("%d file(s) modified", len(modified)), bullets(modified, 10), html + "/commit/" + head, colorOrange})
		}
		if len(removed) > 0 {
			*events = append(*events, Event{fmt.Sprintf("%d file(s) deleted", len(removed)), bullets(removed, 10), html + "/tree/" + def, colorOrange})
		}
	}
	st.Files = files
	st.TreeHead = head
	return nil
}

func (m *Monitor) checkRepo(ctx context.Context, key string, ref repoRef, events *[]Event) error {
	st := m.state[key]
	first := st == nil

	var info ghRepo
	var err error
	if first {
		err = m.get(ctx, "/repos/"+ref.Owner+"/"+ref.Name, &info)
	} else {
		err = m.get(ctx, fmt.Sprintf("/repositories/%d", st.ID), &info)
	}
	if err != nil {
		var ae *apiError
		if !errors.As(err, &ae) {
			return err
		}
		if first {
			logf("[%s] cannot access repository (%v); will retry", key, ae)
			return nil
		}
		if isGone(ae.Status) {
			if !st.Unavailable {
				st.Unavailable = true
				last := st.Label
				if last == "" {
					last = key
				}
				title, why := "Repository unavailable", fmt.Sprintf("GitHub returned HTTP %d: it was deleted, made private, taken down, or your token lost access.", ae.Status)
				if ae.Status == 451 {
					title, why = "Repository taken down (legal block)", "GitHub returned HTTP 451 (blocked for legal reasons, e.g. DMCA)."
				} else if ae.Status == 403 {
					title, why = "Repository access blocked", "GitHub returned HTTP 403."
				}
				*events = append(*events, Event{title, last + "\n" + why, "https://github.com/" + last, colorRed})
			}
			return nil
		}
		logf("[%s] repository lookup failed (%v)", key, ae)
		return nil
	}

	full, html, def := info.FullName, info.HTMLURL, info.DefaultBranch
	desc := ""
	if info.Description != nil {
		desc = *info.Description
	}
	meta := Meta{
		Owner:         info.Owner.Login,
		Name:          info.Name,
		Private:       info.Private,
		Archived:      info.Archived,
		Disabled:      info.Disabled,
		DefaultBranch: def,
		Description:   desc,
	}
	defaultChanged := false
	if first {
		st = &RepoState{ID: info.ID, Meta: meta}
		m.state[key] = st
		logf("[%s] capturing baseline", key)
	} else {
		if st.Unavailable {
			st.Unavailable = false
			*events = append(*events, Event{"Repository is accessible again", full, html, colorGreen})
		}
		*events = append(*events, diffMeta(st.Meta, meta, html, full)...)
		defaultChanged = st.Meta.DefaultBranch != def
		st.Meta = meta
	}
	st.Label = full

	if err := m.checkBranches(ctx, st, full, html, def, events); err != nil && fatal(key, "branches", err) {
		return err
	}

	commits, err := m.checkCommits(ctx, st, full, html, def, defaultChanged, events)
	if err != nil && fatal(key, "commits", err) {
		return err
	}

	if err := m.checkReleases(ctx, st, full, events); err != nil && fatal(key, "releases", err) {
		return err
	}
	if err := m.checkPulls(ctx, st, full, events); err != nil && fatal(key, "pull requests", err) {
		return err
	}
	if err := m.checkIssues(ctx, st, full, events); err != nil && fatal(key, "issues", err) {
		return err
	}

	if len(commits) > 0 && st.TreeHead != commits[0].SHA {
		if err := m.checkFiles(ctx, key, st, full, html, def, commits[0].SHA, defaultChanged, events); err != nil && fatal(key, "file tree", err) {
			return err
		}
	}
	return nil
}

func (m *Monitor) runCycle(ctx context.Context) time.Duration {
	var wait time.Duration
	for _, ref := range m.cfg.Repos {
		if ctx.Err() != nil {
			break
		}
		key := strings.ToLower(ref.Owner + "/" + ref.Name)
		var events []Event
		err := m.checkRepo(ctx, key, ref, &events)
		label := key
		if st := m.state[key]; st != nil && st.Label != "" {
			label = st.Label
		}
		m.dispatch(ctx, label, events)
		if err != nil {
			var rl *rateLimited
			if errors.As(err, &rl) {
				wait = time.Duration(rl.Wait) * time.Second
				logf("GitHub rate limit reached; pausing %ds", rl.Wait)
				break
			}
			if ctx.Err() == nil {
				logf("[%s] check failed: %v", key, err)
			}
		}
	}
	m.saveState()
	return wait
}

func main() {
	var args []string
	flags := map[string]bool{}
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "--") {
			flags[a] = true
		} else {
			args = append(args, a)
		}
	}
	configPath := "config.json"
	if len(args) > 0 {
		configPath = args[0]
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		absConfig = configPath
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := &Monitor{
		cfg:       cfg,
		client:    &http.Client{Timeout: 30 * time.Second},
		cache:     map[string]cacheEntry{},
		statePath: filepath.Join(filepath.Dir(absConfig), "state.json"),
	}

	if flags["--test"] {
		m.dispatch(ctx, "Repo Monitor", []Event{{"Test notification", "If you can see this, notifications work.", "", colorBlue}})
		return
	}

	if cfg.Token == "" {
		logf("warning: no GitHub token set; unauthenticated requests are limited to 60/hour")
	}
	logf("monitoring %d repo(s) every %ds", len(cfg.Repos), int(cfg.Interval/time.Second))
	m.state = loadState(m.statePath)

	for {
		started := time.Now()
		wait := m.runCycle(ctx)
		if flags["--once"] {
			return
		}
		sleep := cfg.Interval - time.Since(started)
		if wait > sleep {
			sleep = wait
		}
		if sleep < time.Second {
			sleep = time.Second
		}
		if !sleepCtx(ctx, sleep) {
			logf("stopped")
			return
		}
	}
}
