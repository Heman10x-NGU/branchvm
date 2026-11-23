//go:build linux

package storage

import (
	"os"
	"os/exec"
	"syscall"
)

func applyRepoOwnerCred(cmd *exec.Cmd, repoDir string) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(repoDir)
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: st.Uid,
			Gid: st.Gid,
		},
	}
}
