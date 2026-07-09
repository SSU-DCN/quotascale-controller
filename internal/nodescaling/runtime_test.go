package nodescaling

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestLoadNodeScalingConfigFromEnvDefaultsBranch(t *testing.T) {
	t.Setenv("GITEA_REPO_URL", "https://gitea.example.local/repo.git")
	t.Setenv("GITEA_USERNAME", "user")
	t.Setenv("GITEA_PASSWORD", "pass")

	cfg, err := LoadNodeScalingConfigFromEnv()
	if err != nil {
		t.Fatalf("expected config to load, got error: %v", err)
	}
	if cfg.RepoBranch != defaultNodeScalingBranch {
		t.Fatalf("expected default branch %s, got %s", defaultNodeScalingBranch, cfg.RepoBranch)
	}
	if cfg.RepoFilePath != defaultNodeScalingFile {
		t.Fatalf("expected default file path %s, got %s", defaultNodeScalingFile, cfg.RepoFilePath)
	}
}

func TestLoadNodeScalingConfigPrefersOverridesOverEnv(t *testing.T) {
	t.Setenv("GITEA_REPO_URL", "https://gitea.example.local/env.git")
	t.Setenv("GITEA_REPO_BRANCH", "env-branch")
	t.Setenv("GITEA_REPO_FILE_PATH", "env/path.yaml")
	t.Setenv("GITEA_USERNAME", "env-user")
	t.Setenv("GITEA_PASSWORD", "env-pass")

	cfg, err := LoadNodeScalingConfig(NodeScalingConfig{
		RepoURL:      "https://gitea.example.local/flag.git",
		RepoBranch:   "flag-branch",
		RepoFilePath: "flag/path.yaml",
		Username:     "flag-user",
		Password:     "flag-pass",
	})
	if err != nil {
		t.Fatalf("expected config to load, got error: %v", err)
	}
	if cfg.RepoURL != "https://gitea.example.local/flag.git" {
		t.Fatalf("expected override repo URL, got %s", cfg.RepoURL)
	}
	if cfg.RepoBranch != "flag-branch" {
		t.Fatalf("expected override branch, got %s", cfg.RepoBranch)
	}
	if cfg.RepoFilePath != "flag/path.yaml" {
		t.Fatalf("expected override file path, got %s", cfg.RepoFilePath)
	}
	if cfg.Username != "flag-user" {
		t.Fatalf("expected override username, got %s", cfg.Username)
	}
	if cfg.Password != "flag-pass" {
		t.Fatalf("expected override password, got %s", cfg.Password)
	}
}

func TestNodeScalingRuntimeSyncRepoClonesRepository(t *testing.T) {
	sourceDir := t.TempDir()
	repo := initGitRepo(t, sourceDir)
	writeFile(t, filepath.Join(sourceDir, defaultNodeScalingFile), machineDeploymentYAML(3))
	commitFile(t, repo, defaultNodeScalingFile, "initial commit")
	checkoutBranch(t, repo, defaultNodeScalingBranch)

	targetDir := filepath.Join(t.TempDir(), "cloned-repo")
	runtime := &NodeScalingRuntime{
		Config: NodeScalingConfig{
			Enabled:    true,
			RepoURL:    sourceDir,
			RepoBranch: defaultNodeScalingBranch,
		},
		RepoDir: targetDir,
	}

	if err := runtime.SyncRepo(); err != nil {
		t.Fatalf("expected repo clone to succeed, got error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(targetDir, defaultNodeScalingFile))
	if err != nil {
		t.Fatalf("expected cloned file to exist, got error: %v", err)
	}
	if string(content) != machineDeploymentYAML(3) {
		t.Fatalf("expected cloned content to match source, got %q", string(content))
	}
	if !runtime.justCloned {
		t.Fatalf("expected runtime to record that it just cloned the repo")
	}
	if !runtime.skipNextPull {
		t.Fatalf("expected runtime to skip the next pull immediately after clone")
	}
}

func TestNodeScalingRuntimeSyncRepoSkipsImmediatePullAfterClone(t *testing.T) {
	sourceDir := t.TempDir()
	repo := initGitRepo(t, sourceDir)
	writeFile(t, filepath.Join(sourceDir, defaultNodeScalingFile), machineDeploymentYAML(3))
	commitFile(t, repo, defaultNodeScalingFile, "initial commit")
	checkoutBranch(t, repo, defaultNodeScalingBranch)

	targetDir := filepath.Join(t.TempDir(), "cloned-repo")
	runtime := &NodeScalingRuntime{
		Config: NodeScalingConfig{
			Enabled:    true,
			RepoURL:    sourceDir,
			RepoBranch: defaultNodeScalingBranch,
		},
		RepoDir: targetDir,
	}

	if err := runtime.SyncRepo(); err != nil {
		t.Fatalf("expected initial clone to succeed, got error: %v", err)
	}
	if err := runtime.SyncRepo(); err != nil {
		t.Fatalf("expected immediate post-clone sync to skip pull successfully, got error: %v", err)
	}
	if runtime.skipNextPull {
		t.Fatalf("expected skipNextPull to be cleared after the skipped sync")
	}
}

func TestNodeScalingRuntimeSyncRepoFallsBackToExistingLocalRepoWhenPullFails(t *testing.T) {
	repoDir := t.TempDir()
	repo := initGitRepo(t, repoDir)
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(3))
	commitFile(t, repo, defaultNodeScalingFile, "initial commit")
	checkoutBranch(t, repo, defaultNodeScalingBranch)

	runtime := &NodeScalingRuntime{
		Config: NodeScalingConfig{
			Enabled:      true,
			RepoURL:      "http://invalid.example.local/repo.git",
			RepoBranch:   defaultNodeScalingBranch,
			RepoFilePath: defaultNodeScalingFile,
			Username:     "user",
			Password:     "pass",
		},
		RepoDir: repoDir,
	}

	if err := runtime.SyncRepo(); err != nil {
		t.Fatalf("expected sync to fall back to the existing local repo, got error: %v", err)
	}

	replicas, err := runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		t.Fatalf("expected local manifest to stay readable, got error: %v", err)
	}
	if replicas != 3 {
		t.Fatalf("expected replicas to remain 3 from the local manifest, got %d", replicas)
	}
}

func TestWriteMachineDeploymentReplicasUpdatesSpecReplicas(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(3))

	runtime := &NodeScalingRuntime{
		Config: NodeScalingConfig{
			RepoFilePath: defaultNodeScalingFile,
		},
		RepoDir: repoDir,
	}

	if err := runtime.WriteMachineDeploymentReplicas(5); err != nil {
		t.Fatalf("expected replica update to succeed, got error: %v", err)
	}

	replicas, err := runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		t.Fatalf("expected replica read to succeed, got error: %v", err)
	}
	if replicas != 5 {
		t.Fatalf("expected replicas to be 5, got %d", replicas)
	}
}

func TestWriteMachineDeploymentReplicasPreservesOtherManifestFields(t *testing.T) {
	repoDir := t.TempDir()
	path := filepath.Join(repoDir, defaultNodeScalingFile)
	writeFile(t, path, `apiVersion: cluster.x-k8s.io/v1beta2
kind: MachineDeployment
metadata:
  name: worker-md
  namespace: default
spec:
  clusterName: demo
  replicas: 0
  selector:
    matchLabels:
      pool: workers
  template:
    spec:
      version: v1.30.0
`)

	runtime := &NodeScalingRuntime{
		Config: NodeScalingConfig{
			RepoFilePath: defaultNodeScalingFile,
		},
		RepoDir: repoDir,
	}

	if err := runtime.WriteMachineDeploymentReplicas(1); err != nil {
		t.Fatalf("expected replica update to succeed, got error: %v", err)
	}

	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected updated manifest to be readable, got error: %v", err)
	}

	content := string(updated)
	for _, expected := range []string{
		"apiVersion: cluster.x-k8s.io/v1beta2",
		"kind: MachineDeployment",
		"name: worker-md",
		"namespace: default",
		"clusterName: demo",
		"pool: workers",
		"version: v1.30.0",
		"replicas: 1",
	} {
		if !contains(content, expected) {
			t.Fatalf("expected updated manifest to contain %q, got:\n%s", expected, content)
		}
	}
	for _, expected := range []string{
		"metadata:\n  name: worker-md\n  namespace: default\n",
		"spec:\n  clusterName: demo\n  replicas: 1\n",
		"selector:\n    matchLabels:\n      pool: workers\n",
		"template:\n    spec:\n      version: v1.30.0\n",
	} {
		if !contains(content, expected) {
			t.Fatalf("expected updated manifest to preserve 2-space indentation pattern %q, got:\n%s", expected, content)
		}
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func initGitRepo(t *testing.T, dir string) *git.Repository {
	t.Helper()

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("expected repo init to succeed, got error: %v", err)
	}

	return repo
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err != nil {
		t.Fatalf("expected directory creation to succeed, got error: %v", err)
	}

	err = os.WriteFile(path, []byte(content), 0o644)
	if err != nil {
		t.Fatalf("expected file write to succeed, got error: %v", err)
	}
}

func commitFile(t *testing.T, repo *git.Repository, file, message string) {
	t.Helper()

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("expected worktree, got error: %v", err)
	}

	if _, err := worktree.Add(file); err != nil {
		t.Fatalf("expected add to succeed, got error: %v", err)
	}

	_, err = worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "tester",
			Email: "tester@example.local",
		},
	})
	if err != nil {
		t.Fatalf("expected commit to succeed, got error: %v", err)
	}
}

func checkoutBranch(t *testing.T, repo *git.Repository, branch string) {
	t.Helper()

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("expected worktree, got error: %v", err)
	}

	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch),
		Create: true,
	})
	if err != nil {
		t.Fatalf("expected branch checkout to succeed, got error: %v", err)
	}
}

func machineDeploymentYAML(replicas int32) string {
	return fmt.Sprintf("apiVersion: cluster.x-k8s.io/v1beta1\nkind: MachineDeployment\nspec:\n  replicas: %d\n", replicas)
}
