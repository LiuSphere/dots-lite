//go:build !windows

package main

import (
	"context"
	"dots-lite/internal/executor"
	"errors"
	"io"
)

func runMaintenance(context.Context, []string, io.Writer, io.Writer) error {
	return errors.New("double-click maintenance controls require Windows")
}
func serveMaintenance(context.Context, string, serviceConfig, *executor.Service, context.CancelFunc) (func(), error) {
	return func() {}, nil
}
