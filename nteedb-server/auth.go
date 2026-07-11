package main

import (
	"bufio"
	"crypto/subtle"
	"fmt"
	"os"
	"strings"
)

// role is a connection's permission level, resolved once at auth time.
type role int

const (
	roleUser  role = iota // data commands + read-only stats
	roleAdmin             // everything (compact/reindex)
)

// authStore holds the server's credentials, configured at startup. Three
// modes, mirroring memcached/redis practice:
//   - none:     every connection is authenticated (admin) from the start;
//     protected mode restricts the listener to loopback.
//   - password: a single shared secret (redis requirepass) — grants admin.
//   - file:     user:password[:role] lines (memcached --auth-file).
type authStore struct {
	mode     string // "none" | "password" | "file"
	password string
	users    map[string]fileUser
}

type fileUser struct {
	password string
	role     role
}

func authNone() *authStore { return &authStore{mode: "none"} }

func authPassword(password string) *authStore {
	return &authStore{mode: "password", password: password}
}

// authFileStore parses user:password[:role] lines. Blank lines and #-comments
// are skipped. role is "admin" or "user" (default "user").
func authFileStore(path string) (*authStore, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("auth file: %w", err)
	}
	defer f.Close()

	users := make(map[string]fileUser)
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("auth file %s:%d: expected user:password[:role]", path, lineNo)
		}
		u := fileUser{password: parts[1], role: roleUser}
		if len(parts) == 3 {
			switch parts[2] {
			case "admin":
				u.role = roleAdmin
			case "user", "":
				u.role = roleUser
			default:
				return nil, fmt.Errorf("auth file %s:%d: unknown role %q (expected admin or user)", path, lineNo, parts[2])
			}
		}
		if _, dup := users[parts[0]]; dup {
			return nil, fmt.Errorf("auth file %s:%d: duplicate user %q", path, lineNo, parts[0])
		}
		users[parts[0]] = u
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("auth file: %w", err)
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("auth file %s: no users defined", path)
	}
	return &authStore{mode: "file", users: users}, nil
}

func (a *authStore) required() bool { return a.mode != "none" }

// check validates an `auth` command's arguments and returns the granted role.
// password mode: auth <password>. file mode: auth <user> <password>.
func (a *authStore) check(args []string) (role, error) {
	switch a.mode {
	case "none":
		return roleAdmin, nil // auth is a no-op when not configured
	case "password":
		if len(args) != 1 {
			return 0, fmt.Errorf("usage: auth <password>")
		}
		if !equalConstantTime(args[0], a.password) {
			return 0, fmt.Errorf("invalid password")
		}
		return roleAdmin, nil
	case "file":
		if len(args) != 2 {
			return 0, fmt.Errorf("usage: auth <user> <password>")
		}
		u, ok := a.users[args[0]]
		// Compare even for unknown users so a missing name costs the same as a
		// wrong password.
		pw := u.password
		if !ok {
			pw = ""
		}
		if !equalConstantTime(args[1], pw) || !ok {
			return 0, fmt.Errorf("invalid user or password")
		}
		return u.role, nil
	}
	return 0, fmt.Errorf("auth: unknown mode %q", a.mode)
}

func equalConstantTime(got, want string) bool {
	// ConstantTimeCompare is length-leaking by contract; hash-free equalization
	// is overkill for a first version — length of a password is low-value.
	return len(got) == len(want) &&
		subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
