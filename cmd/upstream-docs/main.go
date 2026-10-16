// Command upstream-docs puts the documentation of the projects the platform
// is built on into the hub's image, so that it can be read where the image
// runs, without the internet.
//
//	upstream-docs fetch  -projects docs/upstream/projects.json -out DIR
//	upstream-docs latest -projects docs/upstream/projects.json
//
// fetch downloads each project's repository at its pinned ref from GitHub,
// keeps the Markdown pages under its roots and the images they show, turns
// what only the project's own site generator understands into plain
// Markdown, points links and images into the hub, and writes index.json.
//
// latest moves every project to its newest release, or, for a repository
// without releases, to the newest commit of its default branch and the
// newest version its versions file names. The weekly workflow proposes the
// change in a pull request.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: upstream-docs fetch|latest -projects FILE [-out DIR]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	file := fs.String("projects", "docs/upstream/projects.json", "the projects and their pinned refs")
	out := fs.String("out", "upstream-docs", "where fetch writes the documents")
	fs.Parse(os.Args[2:])

	ps, err := load(*file)
	if err != nil {
		fail(err)
	}
	gh := newGitHub(os.Getenv("GITHUB_TOKEN"))
	switch os.Args[1] {
	case "fetch":
		idx := Index{Generated: time.Now().UTC().Format("2006-01-02")}
		for _, p := range ps.Projects {
			files, err := gh.archive(p.Repo, p.Ref)
			if err != nil {
				fail(fmt.Errorf("%s: %w", p.Name, err))
			}
			entry, err := write(p, files, filepath.Join(*out, p.ID))
			if err != nil {
				fail(fmt.Errorf("%s: %w", p.Name, err))
			}
			fmt.Printf("%s %s: %d pages, %d images, %.1f MB\n", p.Name, p.Version, len(entry.Pages), entry.Images, float64(entry.Bytes)/1e6)
			idx.Projects = append(idx.Projects, entry)
		}
		data, _ := json.Marshal(idx)
		if err := os.WriteFile(filepath.Join(*out, "index.json"), data, 0o644); err != nil {
			fail(err)
		}
	case "latest":
		for i, p := range ps.Projects {
			next, err := gh.latest(p)
			if err != nil {
				fail(fmt.Errorf("%s: %w", p.Name, err))
			}
			if next.Ref != p.Ref || next.Version != p.Version {
				fmt.Printf("%s: %s -> %s\n", p.Name, p.Version, next.Version)
			}
			ps.Projects[i] = next
		}
		if err := save(*file, ps); err != nil {
			fail(err)
		}
	default:
		fail(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "upstream-docs:", err)
	os.Exit(1)
}
