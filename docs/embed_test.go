package docs

import (
	"strings"
	"testing"
)

func TestList(t *testing.T) {
	list := List()
	if len(list) < 10 || list[0].Name != "how-it-works" || list[0].Title != "What each action does, and why" {
		t.Fatalf("list starts with %+v", list[:1])
	}
	var notes []string
	for _, d := range list {
		if _, err := Read(d.Name); err != nil {
			t.Errorf("%s: %v", d.Name, err)
		}
		if d.Group == "Release notes" {
			notes = append(notes, d.Name)
		}
	}
	// By number: 0.10 would come before 0.9, which a sort by text gets wrong.
	if !newer("v0.10.0", "v0.9.1") || newer("v0.5.6", "v0.5.7") {
		t.Error("versions are not compared by number")
	}
	for i := 1; i < len(notes); i++ {
		if !newer(strings.TrimPrefix(notes[i-1], "release-notes/"), strings.TrimPrefix(notes[i], "release-notes/")) {
			t.Errorf("%s is listed before %s", notes[i-1], notes[i])
		}
	}
	if _, err := Read("../go"); err == nil {
		t.Error("read a file outside the documents")
	}
}
