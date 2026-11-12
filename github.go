package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// Repo is the slice of the GitHub repository object gh-compact needs.
type Repo struct {
	FullName    string `json:"full_name"`
	Private     bool   `json:"private"`
	Fork        bool   `json:"fork"`
	Archived    bool   `json:"archived"`
	Default     string `json:"default_branch"`
	Permissions struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
	} `json:"permissions"`
}

// CanWrite reports whether the signed-in user may edit and delete releases.
func (r Repo) CanWrite() bool { return !r.Archived && (r.Permissions.Push || r.Permissions.Admin) }

type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type Release struct {
	ID              int64     `json:"id"`
	TagName         string    `json:"tag_name"`
	Name            string    `json:"name"`
	Body            string    `json:"body"`
	Draft           bool      `json:"draft"`
	Prerelease      bool      `json:"prerelease"`
	TargetCommitish string    `json:"target_commitish"`
	CreatedAt       time.Time `json:"created_at"`
	PublishedAt     time.Time `json:"published_at"`
	HTMLURL         string    `json:"html_url"`
	Assets          []Asset   `json:"assets"`
	// TagSHA is only filled in backups, when the tag itself is deleted too.
	TagSHA string `json:"tag_sha,omitempty"`
}

// When returns the moment the release went out (drafts fall back to creation).
func (r Release) When() time.Time {
	if r.PublishedAt.IsZero() {
		return r.CreatedAt
	}
	return r.PublishedAt
}

// Title is what people see as the release's name.
func (r Release) Title() string {
	if strings.TrimSpace(r.Name) != "" {
		return r.Name
	}
	return r.TagName
}

type Client struct {
	rest *api.RESTClient
}

func NewClient(opts api.ClientOptions) (*Client, error) {
	if opts.Headers == nil {
		opts.Headers = map[string]string{}
	}
	opts.Headers["X-GitHub-Api-Version"] = "2022-11-28"
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	rest, err := api.NewRESTClient(opts)
	if err != nil {
		return nil, err
	}
	return &Client{rest: rest}, nil
}

func (c *Client) Login() (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	err := c.rest.Get("user", &u)
	return u.Login, err
}

// paged walks a list endpoint 100 items at a time.
func paged[T any](c *Client, path string) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var all []T
	for page := 1; ; page++ {
		var batch []T
		if err := c.rest.Get(fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page), &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}

// OwnedRepos lists every repository the user owns, private ones included.
func (c *Client) OwnedRepos() ([]Repo, error) {
	return paged[Repo](c, "user/repos?affiliation=owner&sort=pushed")
}

func (c *Client) OrgRepos(org string) ([]Repo, error) {
	return paged[Repo](c, "orgs/"+url.PathEscape(org)+"/repos?type=all&sort=pushed")
}

func (c *Client) Repo(fullName string) (Repo, error) {
	var r Repo
	err := c.rest.Get("repos/"+fullName, &r)
	return r, err
}

func (c *Client) Releases(fullName string) ([]Release, error) {
	return paged[Release](c, "repos/"+fullName+"/releases")
}

func (c *Client) UpdateBody(fullName string, id int64, body string) error {
	return c.send("PATCH", fmt.Sprintf("repos/%s/releases/%d", fullName, id), map[string]any{"body": body}, nil)
}

func (c *Client) DeleteRelease(fullName string, id int64) error {
	return c.rest.Delete(fmt.Sprintf("repos/%s/releases/%d", fullName, id), nil)
}

func (c *Client) TagSHA(fullName, tag string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := c.rest.Get("repos/"+fullName+"/git/ref/tags/"+escapeRef(tag), &ref)
	return ref.Object.SHA, err
}

func (c *Client) DeleteTag(fullName, tag string) error {
	return c.rest.Delete("repos/"+fullName+"/git/refs/tags/"+escapeRef(tag), nil)
}

func (c *Client) CreateTag(fullName, tag, sha string) error {
	return c.send("POST", "repos/"+fullName+"/git/refs", map[string]any{"ref": "refs/tags/" + tag, "sha": sha}, nil)
}

func (c *Client) CreateRelease(fullName string, r Release) (Release, error) {
	payload := map[string]any{
		"tag_name":    r.TagName,
		"name":        r.Name,
		"body":        r.Body,
		"prerelease":  r.Prerelease,
		"make_latest": "false",
	}
	if r.TargetCommitish != "" {
		payload["target_commitish"] = r.TargetCommitish
	}
	var out Release
	err := c.send("POST", "repos/"+fullName+"/releases", payload, &out)
	return out, err
}

func (c *Client) send(method, path string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if out == nil {
		// Some endpoints answer 201 with an empty body; don't try to decode it.
		resp, err := c.rest.Request(method, path, bytes.NewReader(b))
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
	return c.rest.Do(method, path, bytes.NewReader(b), out)
}

// escapeRef keeps the slashes of a tag like "app/v1.2" but escapes the rest.
func escapeRef(tag string) string {
	parts := strings.Split(tag, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func isNotFound(err error) bool {
	var he *api.HTTPError
	return errors.As(err, &he) && he.StatusCode == 404
}

// ---- Actions ----

type Run struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	WorkflowID int64     `json:"workflow_id"`
	Path       string    `json:"path"`
	HeadBranch string    `json:"head_branch"`
	HeadSHA    string    `json:"head_sha"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	HTMLURL    string    `json:"html_url"`
	RunAttempt int       `json:"run_attempt"`
	CreatedAt  time.Time `json:"created_at"`
}

func (r Run) Failed() bool {
	switch r.Conclusion {
	case "failure", "timed_out", "startup_failure", "cancelled":
		return true
	}
	return false
}

type Step struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
}

type Job struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	Steps      []Step `json:"steps"`
}

func (c *Client) Runs(fullName, query string) ([]Run, error) {
	var out struct {
		Runs []Run `json:"workflow_runs"`
	}
	err := c.rest.Get("repos/"+fullName+"/actions/runs?"+query, &out)
	return out.Runs, err
}

func (c *Client) Run(fullName string, id int64) (Run, error) {
	var r Run
	err := c.rest.Get(fmt.Sprintf("repos/%s/actions/runs/%d", fullName, id), &r)
	return r, err
}

func (c *Client) Jobs(fullName string, runID int64) ([]Job, error) {
	var out struct {
		Jobs []Job `json:"jobs"`
	}
	err := c.rest.Get(fmt.Sprintf("repos/%s/actions/runs/%d/jobs?per_page=100", fullName, runID), &out)
	return out.Jobs, err
}

// JobLog returns a job's plain-text log (at most the last 8 MiB).
func (c *Client) JobLog(fullName string, jobID int64) (string, error) {
	resp, err := c.rest.Request("GET", fmt.Sprintf("repos/%s/actions/jobs/%d/logs", fullName, jobID), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if len(data) > 8<<20 {
		data = data[len(data)-8<<20:]
	}
	return string(data), err
}

// Rerun re-runs only the failed jobs, or the whole run when GitHub won't.
func (c *Client) Rerun(fullName string, runID int64) error {
	err := c.send("POST", fmt.Sprintf("repos/%s/actions/runs/%d/rerun-failed-jobs", fullName, runID), map[string]any{}, nil)
	if err != nil {
		err = c.send("POST", fmt.Sprintf("repos/%s/actions/runs/%d/rerun", fullName, runID), map[string]any{}, nil)
	}
	return err
}

func (c *Client) Dispatch(fullName string, workflowID int64, ref string) error {
	return c.send("POST", fmt.Sprintf("repos/%s/actions/workflows/%d/dispatches", fullName, workflowID), map[string]any{"ref": ref}, nil)
}

// FileText reads a file from a branch; "" with no error when it doesn't exist.
func (c *Client) FileText(fullName, path, ref string) (text, sha string, err error) {
	var f struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	err = c.rest.Get("repos/"+fullName+"/contents/"+escapeRef(path)+"?ref="+url.QueryEscape(ref), &f)
	if isNotFound(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	return string(data), f.SHA, err
}

// PutFile commits one file to a branch and returns the new commit's SHA.
func (c *Client) PutFile(fullName, path, branch, fileSHA, message, text string, author map[string]string) (string, error) {
	payload := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(text)),
		"sha":     fileSHA,
		"branch":  branch,
	}
	if author != nil {
		payload["author"], payload["committer"] = author, author
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err := c.send("PUT", "repos/"+fullName+"/contents/"+escapeRef(path), payload, &out)
	return out.Commit.SHA, err
}

func (c *Client) BranchSHA(fullName, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := c.rest.Get("repos/"+fullName+"/git/ref/heads/"+escapeRef(branch), &ref)
	return ref.Object.SHA, err
}

func (c *Client) CreateBranch(fullName, branch, sha string) error {
	return c.send("POST", "repos/"+fullName+"/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": sha}, nil)
}

func (c *Client) LatestRelease(fullName string) (Release, error) {
	var r Release
	err := c.rest.Get("repos/"+fullName+"/releases/latest", &r)
	return r, err
}

func (c *Client) CreatePR(fullName, head, base, title, body string) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	err := c.send("POST", "repos/"+fullName+"/pulls", map[string]any{"head": head, "base": base, "title": title, "body": body}, &out)
	return out.HTMLURL, err
}
