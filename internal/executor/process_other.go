//go:build !windows

package executor

import (
	"errors"
	"os"
	"os/exec"
)

type processControl struct{}

func prepareProcess(cmd *exec.Cmd) (*processControl, error) { return &processControl{}, nil }
func (p *processControl) started(cmd *exec.Cmd) error       { return nil }
func (p *processControl) close() error                      { return nil }

func (p *processControl) kill(cmd *exec.Cmd) error {
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
