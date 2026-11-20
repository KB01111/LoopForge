//go:build windows

package runtime

import "agentflow/internal/sysx"

// killProcessGroups closes the terminal and kills its process tree.
func (s *workspaceTerminal) killProcessGroups() {
	s.file.Close()
	_ = sysx.SignalGroup(s.cmd.Process.Pid, true)
}
