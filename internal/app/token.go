package app

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TokenFile is the wire token's name inside `$UFFICI_HOME`.
const TokenFile = "wire-token"

// TokenMode is the token file's permission: owner only.
//
// 🔴 The token is the only thing stopping a DIFFERENT local process attaching
// to this companion and answering an agent's briefs. A world-readable token
// file makes it decoration.
const TokenMode os.FileMode = 0o600

// tokenBytes is the minted token's entropy. 32 bytes is well past any local
// brute force, and the wire's schema requires at least 32 CHARACTERS, which
// base64 of 32 bytes comfortably exceeds.
const tokenBytes = 32

// minTokenChars is the wire schema's floor for a token, in characters.
const minTokenChars = 32

// EnsureToken resolves the wire token: a supplied one, else the file in
// `$UFFICI_HOME`, minting one there on first start.
//
// 🔴 A SUPPLIED token is never written to disk. It belongs to whoever supplied
// it — in the pod shape, to the orchestrator that hands the same value to both
// containers, because the plugin's filesystem is not the companion's and the
// minted file is unreachable from it. Writing it would leave a copy in a volume
// that outlives the container, at the one path whose security argument is that
// nothing else has seen what is in it.
//
// 🔴 It refuses a token file the world can read rather than fixing it silently.
// A companion that quietly repaired the mode would hide the window in which the
// token was readable — and that window is the whole question. The operator is
// told, and decides.
func EnsureToken(home, supplied string) (string, error) {
	if supplied != "" {
		if len(supplied) < minTokenChars {
			// 🔴 The length, never the value: this string reaches a log.
			return "", fmt.Errorf(
				"UFFICI_WIRE_TOKEN is %d characters; the wire requires at least %d",
				len(supplied), minTokenChars)
		}
		return supplied, nil
	}

	path := filepath.Join(home, TokenFile)

	info, err := os.Stat(path)
	switch {
	case err == nil:
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return "", fmt.Errorf(
				"the wire token at %s is mode %04o, which lets other users read it — "+
					"the token is the only thing stopping another local process from answering "+
					"this agent's briefs. Fix it with `chmod 600 %s`, or delete it to mint a new one",
				path, perm, path)
		}
		raw, readErr := os.ReadFile(path) //nolint:gosec // the path is the companion's own home
		if readErr != nil {
			return "", fmt.Errorf("reading the wire token: %w", readErr)
		}
		token := strings.TrimSpace(string(raw))
		if len(token) < minTokenChars {
			return "", fmt.Errorf(
				"the wire token at %s is %d characters; the wire requires at least 32. "+
					"Delete it to mint a new one", path, len(token))
		}
		return token, nil

	case os.IsNotExist(err):
		return mintToken(home, path)

	default:
		return "", fmt.Errorf("checking the wire token: %w", err)
	}
}

func mintToken(home, path string) (string, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", home, err)
	}
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("minting the wire token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	// 🔴 Written with the restrictive mode from the start, never written open and
	// chmod'ed after: between those two calls the token is readable, and that is
	// precisely the window an attacker needs.
	if err := os.WriteFile(path, []byte(token+"\n"), TokenMode); err != nil {
		return "", fmt.Errorf("writing the wire token: %w", err)
	}
	return token, nil
}
