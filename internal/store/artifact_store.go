package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gorchera/internal/domain"
	runtimeexec "gorchera/internal/runtime"
)

type ArtifactStore struct {
	root string
}

func NewArtifactStore(root string) *ArtifactStore {
	return &ArtifactStore{root: root}
}

func (s *ArtifactStore) MaterializeWorkerArtifacts(jobID string, stepIndex int, output domain.WorkerOutput) ([]string, error) {
	if err := validateID(jobID); err != nil {
		return nil, err
	}
	baseDir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}

	paths := make([]string, 0, len(output.Artifacts))
	for _, name := range output.Artifacts {
		safe := sanitizeArtifactName(name)
		path := filepath.Join(baseDir, fmt.Sprintf("step-%02d-%s", stepIndex, safe))
		var payload []byte
		var err error
		if content, ok := output.FileContents[name]; ok {
			payload = []byte(content)
		} else {
			payload, err = json.MarshalIndent(map[string]string{
				"summary":                 output.Summary,
				"status":                  output.Status,
				"next_recommended_action": output.NextRecommendedAction,
			}, "", "  ")
			if err != nil {
				return nil, err
			}
		}
		if err := writeAtomically(path, payload); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func (s *ArtifactStore) MaterializeTextArtifact(jobID, name, content string) (string, error) {
	if err := validateID(jobID); err != nil {
		return "", err
	}
	baseDir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(baseDir, sanitizeArtifactName(name))
	if err := writeAtomically(path, []byte(content)); err != nil {
		return "", err
	}
	return path, nil
}

func (s *ArtifactStore) MaterializeJSONArtifact(jobID, name string, value any) (string, error) {
	if err := validateID(jobID); err != nil {
		return "", err
	}
	baseDir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(baseDir, sanitizeArtifactName(name))
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeAtomically(path, payload); err != nil {
		return "", err
	}
	return path, nil
}

func (s *ArtifactStore) MaterializeSystemResult(jobID string, stepIndex int, result runtimeexec.Result) ([]string, error) {
	if err := validateID(jobID); err != nil {
		return nil, err
	}
	baseDir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}

	path := filepath.Join(baseDir, fmt.Sprintf("step-%02d-runtime_result.json", stepIndex))
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeAtomically(path, payload); err != nil {
		return nil, err
	}
	return []string{path}, nil
}

func sanitizeArtifactName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." {
		return "artifact.json"
	}
	replacer := strings.NewReplacer("\\", "-", "/", "-", " ", "-", ":", "-", "*", "-", "?", "-", "\"", "-", "<", "-", ">", "-", "|", "-")
	return replacer.Replace(name)
}
