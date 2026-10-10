package buildkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR111_TheBuildKitSeccompProfileAddsOnlyWhatRootlessNeeds asserts the
// profile R-111's container runs under (issue #130) is Docker's default plus
// the calls scripts/buildkit-seccomp.py lists, and nothing that would make it
// unconfined by another name.
//
// Loosening it is meant to be a deliberate change to the script, this list and
// the reasons in docker-compose.yml together, never a quick edit to the JSON to
// get a build through.
func TestR111_TheBuildKitSeccompProfileAddsOnlyWhatRootlessNeeds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "buildkit-seccomp.json"))
	require.NoError(t, err)

	var profile struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names    []string `json:"names"`
			Action   string   `json:"action"`
			Args     []any    `json:"args"`
			Comment  string   `json:"comment"`
			Includes struct {
				Caps []string `json:"caps"`
			} `json:"includes"`
		} `json:"syscalls"`
	}
	require.NoError(t, json.Unmarshal(raw, &profile))
	require.Equal(t, "SCMP_ACT_ERRNO", profile.DefaultAction, "anything not listed must be refused")

	added := profile.Syscalls[len(profile.Syscalls)-1]
	require.Contains(t, added.Comment, "Rootless BuildKit")
	require.Equal(t, []string{
		"clone", "clone3", "fsconfig", "fsmount", "fsopen", "fspick", "keyctl", "mount",
		"mount_setattr", "move_mount", "open_tree", "pivot_root", "setdomainname", "sethostname",
		"setns", "umount2", "unshare",
	}, added.Names, "change scripts/buildkit-seccomp.py and this list together, with the reason")

	// Calls no build needs, which the default profile gives only to a holder of
	// the matching capability. Allowed unconditionally, any of them would undo
	// the point of having a profile. Not ptrace: the default profile itself
	// allows it on kernels from 4.8.
	never := []string{
		"bpf", "perf_event_open", "init_module", "finit_module", "delete_module",
		"kexec_load", "kexec_file_load", "reboot", "open_by_handle_at", "clock_settime",
		"settimeofday", "iopl", "ioperm", "add_key", "request_key", "acct", "swapon", "swapoff",
	}
	for _, rule := range profile.Syscalls {
		if rule.Action != "SCMP_ACT_ALLOW" || len(rule.Includes.Caps) > 0 {
			continue
		}
		for _, name := range never {
			require.False(t, slices.Contains(rule.Names, name),
				"%s is allowed without a capability; builds do not need it", name)
		}
	}
}
