// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"strings"
	"testing"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
)

func TestTemplateGitReadsOnePinnedFile(t *testing.T) {
	repo := gitfetchtest.New(t)
	const file = "nested/compose.yaml"
	initial := repo.Commit(map[string]string{file: "services: {web: {image: nginx:1.27}}\n"})
	h := &Handler{gitClient: gitfetchtest.Client()}
	req := &v1.ComposeTemplateGitSource{Repository: repo.URL, Path: file}
	draft, err := h.readTemplateGit(t.Context(), req)
	if err != nil || draft.GetGit().GetCommit() != initial || !strings.Contains(draft.GetComposeYaml(), "nginx:1.27") {
		t.Fatalf("first checkout: %v %v", draft, err)
	}
	if err := compose.ValidateTemplate(t.Context(), draft, nil); err != nil {
		t.Fatal(err)
	}
	next := repo.Commit(map[string]string{file: "services: {web: {image: nginx:1.28}}\n"})
	req.Ref = initial
	previous, err := h.readTemplateGit(t.Context(), req)
	if err != nil || previous.GetGit().GetCommit() != initial || !strings.Contains(previous.GetComposeYaml(), "nginx:1.27") {
		t.Fatalf("older checkout: %v %v", previous, err)
	}
	req.Ref = ""
	newer, err := h.readTemplateGit(t.Context(), req)
	if err != nil || newer.GetGit().GetCommit() != next {
		t.Fatalf("new ref: %v %v", newer, err)
	}
	req.Path = "../compose.yaml"
	if _, err := h.readTemplateGit(t.Context(), req); err == nil {
		t.Fatal("path traversal was accepted")
	}
	req.Path = file
	req.Repository = "https://127.0.0.1/repository.git"
	if _, err := h.readTemplateGit(t.Context(), req); err == nil {
		t.Fatal("a private address was accepted")
	}
}

func TestTemplateGitRefusesLinksAndOversizedFiles(t *testing.T) {
	repo := gitfetchtest.New(t)
	initial := repo.Commit(map[string]string{
		"compose.yaml": "services: {web: {image: nginx:1.27}}",
		"large.yaml":   strings.Repeat("a", compose.MaxFile+1),
	})
	repo.Link("linked.yaml", "compose.yaml")
	repo.Commit(nil)
	repo.Submodule("module.yaml", initial)
	h := &Handler{gitClient: gitfetchtest.Client()}
	for _, path := range []string{"linked.yaml", "module.yaml", "large.yaml"} {
		t.Run(path, func(t *testing.T) {
			got, err := h.readTemplateGit(t.Context(), &v1.ComposeTemplateGitSource{Repository: repo.URL, Path: path})
			if err == nil || got != nil {
				t.Fatal("Git prepared a file outside the template's file rules")
			}
		})
	}
}
