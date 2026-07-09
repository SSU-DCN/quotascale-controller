package nodescaling

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/SSU-DCN/quotascale-controller/pkg/logging"
	"gopkg.in/yaml.v2"
	yamlv3 "gopkg.in/yaml.v3"
)

const (
	defaultNodeScalingRepoDir = "quotascale-controller-node-scaling"
	defaultNodeScalingBranch  = "main"
	defaultNodeScalingFile    = "feature/node-scaling/md.yaml"
)

type NodeScalingConfig struct {
	Enabled      bool
	RepoURL      string
	RepoBranch   string
	RepoFilePath string
	Username     string
	Password     string
}

type NodeScalingRuntime struct {
	Config       NodeScalingConfig
	RepoDir      string
	justCloned   bool
	skipNextPull bool
}

type MachineDeploymentManifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Spec       struct {
		Replicas *int32 `yaml:"replicas"`
	} `yaml:"spec"`
}

var nodeScalingRuntime *NodeScalingRuntime

func InitializeNodeScaling(enabled bool, overrides NodeScalingConfig) (*NodeScalingRuntime, error) {
	if !enabled {
		nodeScalingRuntime = nil
		return nil, nil
	}

	cfg, err := LoadNodeScalingConfig(overrides)
	if err != nil {
		return nil, err
	}

	runtime := &NodeScalingRuntime{
		Config:  cfg,
		RepoDir: filepath.Join(os.TempDir(), defaultNodeScalingRepoDir),
	}
	if err := runtime.SyncRepo(); err != nil {
		return nil, err
	}

	nodeScalingRuntime = runtime
	return runtime, nil
}

func LoadNodeScalingConfig(overrides NodeScalingConfig) (NodeScalingConfig, error) {
	cfg := NodeScalingConfig{
		Enabled:      true,
		RepoURL:      firstNonEmpty(overrides.RepoURL, os.Getenv("GITEA_REPO_URL")),
		RepoBranch:   firstNonEmpty(overrides.RepoBranch, os.Getenv("GITEA_REPO_BRANCH")),
		RepoFilePath: firstNonEmpty(overrides.RepoFilePath, os.Getenv("GITEA_REPO_FILE_PATH")),
		Username:     firstNonEmpty(overrides.Username, os.Getenv("GITEA_USERNAME")),
		Password:     firstNonEmpty(overrides.Password, os.Getenv("GITEA_PASSWORD")),
	}
	if cfg.RepoBranch == "" {
		cfg.RepoBranch = defaultNodeScalingBranch
	}
	if cfg.RepoFilePath == "" {
		cfg.RepoFilePath = defaultNodeScalingFile
	}

	switch {
	case cfg.RepoURL == "":
		return cfg, errors.New("node scaling enabled but GITEA_REPO_URL is not set")
	case cfg.Username == "":
		return cfg, errors.New("node scaling enabled but GITEA_USERNAME is not set")
	case cfg.Password == "":
		return cfg, errors.New("node scaling enabled but GITEA_PASSWORD is not set")
	}

	return cfg, nil
}

func LoadNodeScalingConfigFromEnv() (NodeScalingConfig, error) {
	return LoadNodeScalingConfig(NodeScalingConfig{})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func GetNodeScalingRuntime() *NodeScalingRuntime {
	return nodeScalingRuntime
}

func (runtime *NodeScalingRuntime) SyncRepo() error {
	if err := os.MkdirAll(filepath.Dir(runtime.RepoDir), 0o755); err != nil {
		return err
	}

	if _, err := os.Stat(filepath.Join(runtime.RepoDir, ".git")); err == nil {
		if runtime.skipNextPull {
			runtime.skipNextPull = false
			logging.LogInfo(
				"Skipping immediate node scaling repo pull for %s in %s because this process just cloned it",
				runtime.Config.RepoURL,
				runtime.RepoDir,
			)
			return nil
		}
		if err := runtime.pullRepo(); err != nil {
			if fallbackErr := runtime.ensureLocalMachineDeploymentReadable(); fallbackErr == nil {
				logging.LogError(
					"Node scaling repo pull failed for %s in %s, but using existing local repo because %s is readable: %s",
					runtime.Config.RepoURL,
					runtime.RepoDir,
					runtime.MachineDeploymentFilePath(),
					err.Error(),
				)
				return nil
			}
			return err
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	if _, err := os.Stat(runtime.RepoDir); err == nil {
		if err := os.RemoveAll(runtime.RepoDir); err != nil {
			return err
		}
	}

	return runtime.cloneRepo()
}

func (runtime *NodeScalingRuntime) cloneRepo() error {
	logging.LogInfo("Cloning node scaling repo %s into %s", runtime.Config.RepoURL, runtime.RepoDir)
	_, err := git.PlainClone(runtime.RepoDir, false, &git.CloneOptions{
		URL:           runtime.Config.RepoURL,
		Auth:          runtime.auth(),
		ReferenceName: plumbing.NewBranchReferenceName(runtime.Config.RepoBranch),
		SingleBranch:  true,
		Depth:         1,
		Progress:      os.Stdout,
	})
	if err != nil {
		return err
	}
	runtime.justCloned = true
	runtime.skipNextPull = true
	return nil
}

func (runtime *NodeScalingRuntime) pullRepo() error {
	logging.LogInfo("Pulling node scaling repo %s in %s", runtime.Config.RepoURL, runtime.RepoDir)
	repo, err := git.PlainOpen(runtime.RepoDir)
	if err != nil {
		return err
	}

	if err := runtime.ensureRemoteURL(repo); err != nil {
		return err
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return err
	}

	err = worktree.Pull(&git.PullOptions{
		RemoteName:    "origin",
		Auth:          runtime.auth(),
		ReferenceName: plumbing.NewBranchReferenceName(runtime.Config.RepoBranch),
		SingleBranch:  true,
		Force:         true,
	})
	if err == git.NoErrAlreadyUpToDate {
		return nil
	}
	return err
}

func (runtime *NodeScalingRuntime) ensureLocalMachineDeploymentReadable() error {
	_, err := runtime.ReadMachineDeploymentReplicas()
	return err
}

func (runtime *NodeScalingRuntime) ensureRemoteURL(repo *git.Repository) error {
	remote, err := repo.Remote("origin")
	if err != nil {
		return err
	}

	if len(remote.Config().URLs) > 0 && remote.Config().URLs[0] == runtime.Config.RepoURL {
		return nil
	}

	if err := repo.DeleteRemote("origin"); err != nil {
		return err
	}

	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{runtime.Config.RepoURL},
	})
	return err
}

func (runtime *NodeScalingRuntime) auth() transport.AuthMethod {
	if runtime.Config.Username == "" && runtime.Config.Password == "" {
		return nil
	}

	return &http.BasicAuth{
		Username: runtime.Config.Username,
		Password: runtime.Config.Password,
	}
}

func (runtime *NodeScalingRuntime) CommitAndPush(message string) error {
	repo, err := git.PlainOpen(runtime.RepoDir)
	if err != nil {
		return err
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return err
	}

	if _, err := worktree.Add(runtime.MachineDeploymentFilePath()); err != nil {
		return err
	}

	commitHash, err := worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  runtime.Config.Username,
			Email: fmt.Sprintf("%s@gitea.local", runtime.Config.Username),
		},
	})
	if err != nil {
		return err
	}
	logging.LogInfo(
		"Committed node scaling manifest change for %s in %s with message %q at %s",
		runtime.MachineDeploymentFilePath(),
		runtime.Config.RepoURL,
		message,
		commitHash.String(),
	)

	if err := repo.Push(&git.PushOptions{
		Auth: runtime.auth(),
	}); err != nil {
		return err
	}
	logging.LogInfo(
		"Pushed node scaling manifest change for %s to %s on branch %s",
		runtime.MachineDeploymentFilePath(),
		runtime.Config.RepoURL,
		runtime.Config.RepoBranch,
	)
	return nil
}

func (runtime *NodeScalingRuntime) MachineDeploymentFilePath() string {
	return runtime.Config.RepoFilePath
}

func (runtime *NodeScalingRuntime) MachineDeploymentAbsolutePath() string {
	return filepath.Join(runtime.RepoDir, runtime.MachineDeploymentFilePath())
}

func (runtime *NodeScalingRuntime) ReadMachineDeployment() (*MachineDeploymentManifest, error) {
	content, err := os.ReadFile(runtime.MachineDeploymentAbsolutePath())
	if err != nil {
		return nil, err
	}

	var manifest MachineDeploymentManifest
	if err := yaml.Unmarshal(content, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (runtime *NodeScalingRuntime) ReadMachineDeploymentReplicas() (int32, error) {
	manifest, err := runtime.ReadMachineDeployment()
	if err != nil {
		return 0, err
	}
	if manifest.Spec.Replicas == nil {
		return 0, errors.New("machine deployment spec.replicas is missing")
	}
	return *manifest.Spec.Replicas, nil
}

func (runtime *NodeScalingRuntime) WriteMachineDeploymentReplicas(replicas int32) error {
	content, err := os.ReadFile(runtime.MachineDeploymentAbsolutePath())
	if err != nil {
		return err
	}

	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(content, &doc); err != nil {
		return err
	}

	if err := setSpecReplicas(&doc, replicas); err != nil {
		return err
	}

	var buffer bytes.Buffer
	encoder := yamlv3.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}

	return os.WriteFile(runtime.MachineDeploymentAbsolutePath(), buffer.Bytes(), 0o644)
}

func setSpecReplicas(doc *yamlv3.Node, replicas int32) error {
	if doc == nil || len(doc.Content) == 0 {
		return errors.New("machine deployment manifest is empty")
	}

	root := doc.Content[0]
	if root.Kind != yamlv3.MappingNode {
		return errors.New("machine deployment manifest root must be a mapping")
	}

	specNode := findMappingValue(root, "spec")
	if specNode == nil {
		return errors.New("machine deployment spec is missing")
	}
	if specNode.Kind != yamlv3.MappingNode {
		return errors.New("machine deployment spec must be a mapping")
	}

	replicasNode := findMappingValue(specNode, "replicas")
	if replicasNode == nil {
		specNode.Content = append(specNode.Content,
			&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: "replicas"},
			&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(int64(replicas), 10)},
		)
		return nil
	}

	replicasNode.Kind = yamlv3.ScalarNode
	replicasNode.Tag = "!!int"
	replicasNode.Value = strconv.FormatInt(int64(replicas), 10)
	replicasNode.Style = 0
	return nil
}

func findMappingValue(node *yamlv3.Node, key string) *yamlv3.Node {
	if node == nil || node.Kind != yamlv3.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
