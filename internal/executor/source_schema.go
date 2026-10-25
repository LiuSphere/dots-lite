package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// The installed generator is authoritative. Omitting sourceKinds would silently
// restrict thread/list to interactive sessions, and a fixed enum can miss new kinds.
func DiscoverThreadSources(ctx context.Context, codex string) ([]string, error) {
	parent, err := filepath.Abs(os.TempDir())
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(parent, "dots-lite-codex-schema-")
	if err != nil {
		return nil, err
	}
	if !pathWithin(parent, directory) || samePath(parent, directory) {
		return nil, fmt.Errorf("schema temporary directory is outside its parent")
	}
	defer os.RemoveAll(directory)
	command := exec.CommandContext(ctx, codex, "app-server", "generate-json-schema", "--experimental", "--out", directory)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	control, err := prepareProcess(command)
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, errors.Join(err, control.close())
	}
	if err := control.started(command); err != nil {
		_ = control.kill(command)
		_ = command.Wait()
		return nil, errors.Join(err, control.close())
	}
	if err := errors.Join(command.Wait(), control.close()); err != nil {
		return nil, fmt.Errorf("installed Codex schema generation failed: %w: %.1000s", err, output.String())
	}
	data, err := os.ReadFile(filepath.Join(directory, "v2", "ThreadListParams.json"))
	if err != nil {
		return nil, err
	}
	var schema struct {
		Definitions struct {
			Source struct {
				Enum []string `json:"enum"`
			} `json:"ThreadSourceKind"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, err
	}
	if len(schema.Definitions.Source.Enum) == 0 {
		return nil, fmt.Errorf("installed thread source enumeration is unavailable")
	}
	return schema.Definitions.Source.Enum, nil
}

func (s *Service) LoadThreadSources(ctx context.Context, codex string) error {
	s.writeGate.RLock()
	defer s.writeGate.RUnlock()
	s.mu.Lock()
	allowed := s.maintenance.Phase == "running" || s.maintenance.Phase == "resuming"
	s.mu.Unlock()
	if !allowed {
		return errors.New("native inventory schema discovery is disabled during maintenance")
	}
	sources, err := DiscoverThreadSources(ctx, codex)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.ThreadSourceKinds = sources
	s.config.SourceSchemaError = ""
	if err != nil {
		s.config.SourceSchemaError = err.Error()
	}
	return err
}
