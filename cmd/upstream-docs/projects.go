package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// Project is one project whose documentation goes into the image.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Repo is the GitHub repository that holds the documentation.
	Repo string `json:"repo"`
	// Site is the project's documentation site, where a link to one of its
	// pages that is not in the image goes.
	Site string `json:"site"`
	// Track is "release": Ref is the newest release's tag; or "branch": Ref
	// is a commit of the default branch, and Versions names the file that
	// lists the documented versions, newest first.
	Track    string `json:"track"`
	Versions string `json:"versions,omitempty"`
	// Roots are the folders that hold the pages, with {version} and
	// {minor} ("1.9" of "v1.9.2") filled in from Version.
	Roots []string `json:"roots"`
	// Static is the folder that a path such as "/img/a.png" is under.
	Static  string `json:"static,omitempty"`
	Ref     string `json:"ref"`
	Version string `json:"version"`
}

// Projects is the file that pins every project.
type Projects struct {
	Projects []Project `json:"projects"`
}

func load(file string) (Projects, error) {
	var ps Projects
	data, err := os.ReadFile(file)
	if err != nil {
		return ps, err
	}
	err = json.Unmarshal(data, &ps)
	return ps, err
}

func save(file string, ps Projects) error {
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append(data, '\n'), 0o644)
}

// roots returns the project's folders of pages for its version.
func (p Project) roots() []string {
	v := strings.TrimPrefix(p.Version, "v")
	minor := v
	if parts := strings.SplitN(v, ".", 3); len(parts) >= 2 {
		minor = parts[0] + "." + parts[1]
	}
	r := strings.NewReplacer("{version}", v, "{minor}", minor)
	out := make([]string, len(p.Roots))
	for i, root := range p.Roots {
		out[i] = strings.Trim(r.Replace(root), "/")
	}
	return out
}

// gitHub reads from GitHub: releases and commits through its API, files and
// archives from its download hosts.
type gitHub struct {
	token  string
	client *http.Client
	api    string
	raw    string
	load   string
}

func newGitHub(token string) *gitHub {
	return &gitHub{token: token, client: &http.Client{Timeout: 10 * time.Minute},
		api: "https://api.github.com", raw: "https://raw.githubusercontent.com", load: "https://codeload.github.com"}
}

func (g *gitHub) get(url string, api bool) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
		if g.token != "" {
			req.Header.Set("Authorization", "Bearer "+g.token)
		}
	}
	res, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, res.Status)
	}
	return body, nil
}

// latest returns the project moved to its newest release, or to the newest
// commit of its default branch and the newest version its versions file
// names.
func (g *gitHub) latest(p Project) (Project, error) {
	switch p.Track {
	case "release":
		body, err := g.get(g.api+"/repos/"+p.Repo+"/releases/latest", true)
		if err != nil {
			return p, err
		}
		var rel struct {
			Tag string `json:"tag_name"`
		}
		if err := json.Unmarshal(body, &rel); err != nil || rel.Tag == "" {
			return p, fmt.Errorf("no release tag in %s", p.Repo)
		}
		p.Ref, p.Version = rel.Tag, rel.Tag
	case "branch":
		body, err := g.get(g.api+"/repos/"+p.Repo+"/commits/HEAD", true)
		if err != nil {
			return p, err
		}
		var c struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal(body, &c); err != nil || c.SHA == "" {
			return p, fmt.Errorf("no commit in %s", p.Repo)
		}
		body, err = g.get(g.raw+"/"+p.Repo+"/"+c.SHA+"/"+p.Versions, false)
		if err != nil {
			return p, err
		}
		var versions []string
		if err := json.Unmarshal(body, &versions); err != nil || len(versions) == 0 {
			return p, fmt.Errorf("no versions in %s", p.Versions)
		}
		p.Ref, p.Version = c.SHA, versions[0]
	default:
		return p, fmt.Errorf("track %q is neither release nor branch", p.Track)
	}
	return p, nil
}

// archive returns the files of the repository at ref that may go into the
// image, by their path in the repository: Markdown pages, images, and the
// license and notice at the top.
func (g *gitHub) archive(repo, ref string) (map[string][]byte, error) {
	body, err := g.get(g.load+"/"+repo+"/tar.gz/"+ref, false)
	if err != nil {
		return nil, err
	}
	return readArchive(bytes.NewReader(body))
}

// maxImage leaves out an image larger than this: a page shows its caption.
const maxImage = 4 << 20

func readArchive(r io.Reader) (map[string][]byte, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		// The archive puts everything under "<repo>-<ref>/".
		_, name, ok := strings.Cut(h.Name, "/")
		if !ok || !keep(name) || (isImage(name) && h.Size > maxImage) {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[name] = data
	}
}

func keep(name string) bool {
	if isPage(name) || isImage(name) {
		return true
	}
	return !strings.Contains(name, "/") && (strings.HasPrefix(name, "LICENSE") || strings.HasPrefix(name, "NOTICE"))
}

func isPage(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".md" || ext == ".mdx"
}

func isImage(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp":
		return true
	}
	return false
}
