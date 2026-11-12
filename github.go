package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
