package app

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// AgentKeyFile is the agent key's name inside `$UFFICI_HOME`.
const AgentKeyFile = "agent-key"

// InstanceIDFile is the companion's device id inside `$UFFICI_HOME`.
const InstanceIDFile = "instance-id"

// uuidPattern is the shape the gateway requires of an instance id.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// LoadAgentKey resolves the agent key: the file in `$UFFICI_HOME` first, then
// the `UFFICI_AGENT_KEY` value the caller read from the environment.
//
// 🔴 THE FILE IS PREFERRED, and the docs say why: an environment variable is
// visible to anyone who can run `docker inspect` on the container or read
// `/proc/<pid>/environ`, and it is inherited by every child the process
// starts. The environment exists for one deployment only — a Kubernetes pod
// that mounts the key from a Secret, where no file is written by anyone.
//
// 🔴 The key is SECRET-CLASS: never logged, never quoted in an error, and
// written by this program in exactly one place — WriteAgentKey, at the end of
// `uffici pair` (#1966). A world- or group-readable key file is
// refused rather than repaired, for the same reason as the wire token: the
// window in which it was readable is the question, and the operator decides.
func LoadAgentKey(home, fromEnv string) (string, error) {
	path := filepath.Join(home, AgentKeyFile)
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return "", fmt.Errorf(
				"the agent key at %s is mode %04o, which lets other users read it — anyone who can read it "+
					"can answer as this agent. Fix it with `chmod 600 %s`, and ask an administrator to rotate "+
					"the key if the machine is shared", path, perm, path)
		}
		raw, readErr := os.ReadFile(path) //nolint:gosec // the path is the companion's own home
		if readErr != nil {
			return "", fmt.Errorf("reading the agent key: %w", readErr)
		}
		key := strings.TrimSpace(string(raw))
		if key == "" {
			return "", fmt.Errorf("the agent key at %s is empty", path)
		}
		return key, nil
	case os.IsNotExist(err):
		if key := strings.TrimSpace(fromEnv); key != "" {
			return key, nil
		}
		return "", fmt.Errorf(
			"no agent key: put it in %s (mode 600), or — in a Kubernetes pod mounting it from a Secret — "+
				"set UFFICI_AGENT_KEY. An administrator reveals the key once, when the agent is created in "+
				"the portal or its key is rotated", path)
	default:
		return "", fmt.Errorf("checking the agent key: %w", err)
	}
}

// EnsureInstanceID returns this companion's device id, minting it on first
// start and reusing it on every start after.
//
// 🔴 PERSISTENT, and that is the point. The gateway's one-key-one-harness gate
// compares it: the same instance reconnecting replaces its own connection,
// while a DIFFERENT instance is refused `instance_conflict`. A companion that
// minted a new id per start would be refused by its own ghost for three
// heartbeats after every restart — which is why a pod or an LXC keeps
// `$UFFICI_HOME` on a volume.
//
// A malformed file is refused, never replaced: silently minting a new id would
// turn "I copied a broken file" into a conflict with the old id that nobody
// can explain.
func EnsureInstanceID(home string) (string, error) {
	path := filepath.Join(home, InstanceIDFile)
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the companion's own home
	switch {
	case err == nil:
		id := strings.TrimSpace(string(raw))
		if !uuidPattern.MatchString(id) {
			return "", fmt.Errorf("the instance id at %s is not a lowercase uuid; delete it to mint a new one", path)
		}
		return id, nil
	case os.IsNotExist(err):
		if mkErr := os.MkdirAll(home, 0o700); mkErr != nil {
			return "", fmt.Errorf("creating %s: %w", home, mkErr)
		}
		id, mintErr := newInstanceID()
		if mintErr != nil {
			return "", mintErr
		}
		if writeErr := os.WriteFile(path, []byte(id+"\n"), TokenMode); writeErr != nil {
			return "", fmt.Errorf("writing the instance id: %w", writeErr)
		}
		return id, nil
	default:
		return "", fmt.Errorf("reading the instance id: %w", err)
	}
}

func newInstanceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("minting the instance id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// WriteAgentKey puts a key `uffici pair` received into `$UFFICI_HOME/agent-key`,
// mode 600 in a mode-700 home (#1966).
//
// 🔴 The ONE place this program writes the key. It goes to a temporary file
// that is born mode 600 and is then renamed over the old key, so no reader ever
// sees a half-written key or a moment in which the file was readable by others.
// Pairing again replaces the key: the portal's approval is a rotation, and the
// key it replaced keeps working through the overlap.
func WriteAgentKey(home, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("refusing to write an empty agent key")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("creating the companion home: %w", err)
	}
	temp, err := os.CreateTemp(home, ".agent-key-*")
	if err != nil {
		return fmt.Errorf("writing the agent key: %w", err)
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("writing the agent key: %w", err)
	}
	if _, err := temp.WriteString(key + "\n"); err != nil {
		_ = temp.Close()
		return fmt.Errorf("writing the agent key: %w", err)
	}
	// On disk before the rename: a crash must not leave an empty key in place of the old one.
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("writing the agent key: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("writing the agent key: %w", err)
	}
	if err := os.Rename(name, filepath.Join(home, AgentKeyFile)); err != nil {
		return fmt.Errorf("writing the agent key: %w", err)
	}
	return nil
}
