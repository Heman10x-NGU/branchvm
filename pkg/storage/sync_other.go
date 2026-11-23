//go:build !linux

package storage

import "os/exec"

func applyRepoOwnerCred(cmd *exec.Cmd, repoDir string) {}
