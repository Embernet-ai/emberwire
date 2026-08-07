// Package shell decides what the exec node is permitted to run.
//
// This exists because of what the exec node is. Node-RED's threat model is that
// anyone who can deploy a flow already owns the box, and the exec node is the
// proof: it runs an arbitrary command line through a shell. That model held when
// Node-RED was a thing you ran on your own Pi. It does not hold for an App Store
// app on a customer's plant floor, where the editor is reachable by anyone with
// a dashboard login, and it is the mechanism behind CVE-2025-41656 —
// unauthenticated remote code execution against a default Node-RED, achieved by
// deploying a flow with an exec node in it.
//
// So the node ships disabled, an operator has to name the commands it may run,
// and there is no shell. Those three together mean a flow author cannot turn
// "can edit a flow" into "can run anything", which is the property that makes
// the node shippable at all.
package shell

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// Policy is the allowlist the exec node is checked against.
type Policy struct {
	enabled bool

	// resolved maps the absolute path of each allowed command to the spelling
	// the operator configured, which is what an error message should quote back
	// at them.
	mu       sync.RWMutex
	resolved map[string]string
	// unresolved holds allowlist entries that were not on the PATH at startup.
	// They are kept rather than rejected: a command may legitimately appear
	// later, mounted from a ConfigMap or installed into a sidecar volume.
	unresolved []string
}

// NewPolicy builds a policy from the configuration.
//
// An enabled policy with no commands is an error rather than "anything". The
// permissive reading of an empty list is how a deliberately narrow capability
// becomes a general-purpose shell, and it would be the single worst default in
// this codebase.
func NewPolicy(enabled bool, commands []string) (*Policy, error) {
	p := &Policy{enabled: enabled, resolved: map[string]string{}}
	if !enabled {
		return p, nil
	}
	if len(commands) == 0 {
		return nil, fmt.Errorf("the exec node is enabled but no commands are allowed; " +
			"list the commands a flow may run, or disable it")
	}

	for _, c := range commands {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		abs, err := resolve(c)
		if err != nil {
			// Not fatal. A command that is not there yet is a deployment
			// ordering problem, not a configuration error, and refusing to boot
			// over it would take the whole flow down for one unused entry.
			p.unresolved = append(p.unresolved, c)
			continue
		}
		p.resolved[abs] = c
	}

	if len(p.resolved) == 0 && len(p.unresolved) == 0 {
		return nil, fmt.Errorf("the exec node is enabled but every allowed command was blank")
	}
	return p, nil
}

// Enabled reports whether the exec node may run anything at all.
func (p *Policy) Enabled() bool { return p != nil && p.enabled }

// Unresolved lists allowed commands that were not found on the PATH at startup,
// so the operator can be told at boot rather than at the first message.
func (p *Policy) Unresolved() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.unresolved...)
}

// Allowed lists the configured commands, for the error message when one is
// refused. Sorted, so the message is the same every time.
func (p *Policy) Allowed() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]string, 0, len(p.resolved)+len(p.unresolved))
	for _, spelling := range p.resolved {
		out = append(out, spelling)
	}
	out = append(out, p.unresolved...)
	sort.Strings(out)
	return out
}

// ErrDisabled is returned when the exec node is switched off.
var ErrDisabled = fmt.Errorf("the exec node is disabled; enable it and list the " +
	"commands a flow may run")

// ErrNotAllowed is returned for a command outside the allowlist.
type ErrNotAllowed struct {
	Command string
	Allowed []string
}

func (e *ErrNotAllowed) Error() string {
	if len(e.Allowed) == 0 {
		return fmt.Sprintf("%q is not an allowed command", e.Command)
	}
	return fmt.Sprintf("%q is not an allowed command; the allowed commands are %s",
		e.Command, strings.Join(e.Allowed, ", "))
}

// Resolve checks a command and returns the absolute path to execute.
//
// The comparison is on the resolved absolute path, not on the string the flow
// typed. Comparing strings would let a flow name "curl" and get whichever curl
// happens to be first on the PATH of the process — and PATH is something a
// Function node can read and a sidecar can influence. Resolving first means the
// allowlist names a file on disk, which is the thing an operator thought they
// were allowing.
func (p *Policy) Resolve(command string) (string, error) {
	if !p.Enabled() {
		return "", ErrDisabled
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("no command given")
	}

	abs, err := resolve(command)
	if err != nil {
		// Report it as a refusal rather than as "not found". Which of the two it
		// is depends on the filesystem, and a flow author probing the difference
		// learns what is installed on the box.
		return "", &ErrNotAllowed{Command: command, Allowed: p.Allowed()}
	}

	p.mu.RLock()
	_, ok := p.resolved[abs]
	unresolved := p.unresolved
	p.mu.RUnlock()
	if ok {
		return abs, nil
	}

	// An entry that was missing at startup may exist now. Resolve it once and
	// promote it, so a command mounted after boot works without a restart.
	for _, entry := range unresolved {
		entryAbs, err := resolve(entry)
		if err != nil || entryAbs != abs {
			continue
		}
		p.mu.Lock()
		p.resolved[abs] = entry
		p.unresolved = removeString(p.unresolved, entry)
		p.mu.Unlock()
		return abs, nil
	}

	return "", &ErrNotAllowed{Command: command, Allowed: p.Allowed()}
}

func removeString(list []string, s string) []string {
	out := list[:0]
	for _, e := range list {
		if e != s {
			out = append(out, e)
		}
	}
	return out
}

// resolve turns a command name or path into an absolute path, following the
// PATH for a bare name and evaluating symlinks so that two spellings of the same
// binary compare equal.
func resolve(command string) (string, error) {
	path := command
	if !strings.ContainsRune(command, filepath.Separator) &&
		!(runtime.GOOS == "windows" && strings.ContainsRune(command, '/')) {
		found, err := exec.LookPath(command)
		if err != nil {
			return "", err
		}
		path = found
	} else if err := executable(path); err != nil {
		// LookPath does this check for a bare name. Doing it here too means a
		// path that is not on disk yet lands in the unresolved list rather than
		// being admitted to the allowlist and failing at the first message.
		return "", err
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// EvalSymlinks so that /usr/bin/python3 and /usr/bin/python3.12 resolve to
	// the same file: allowing one and getting the other would be a surprise in
	// whichever direction it went.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if runtime.GOOS == "windows" {
		// Windows paths are case-insensitive, so compare them that way. This
		// matters only for development on a workstation; the shipped image is
		// Linux.
		abs = strings.ToLower(abs)
	}
	return abs, nil
}

// executable reports whether a path names a file that could be run.
//
// The permission check is skipped on Windows, where executability is decided by
// the extension rather than by a mode bit. The shipped image is Linux; this is
// only for developing on a workstation.
func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}

// SplitArgs splits a command line into arguments the way a shell would, honouring
// single quotes, double quotes and backslash escapes — and nothing else.
//
// Deliberately not a shell. There is no expansion, no substitution, no globbing,
// no pipes and no operators, because every one of those is a way to turn one
// allowed command into an arbitrary one. A command line containing an unquoted
// shell metacharacter is refused rather than being run with the character taken
// literally, since a flow author who typed it plainly expected it to do
// something.
func SplitArgs(line string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		started bool
	)

	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			flush()

		case '\\':
			if i+1 >= len(runes) {
				return nil, fmt.Errorf("the command line ends in a trailing backslash")
			}
			i++
			cur.WriteRune(runes[i])
			started = true

		case '\'':
			// Single quotes are literal all the way to the closing quote, which
			// is the only place a metacharacter is allowed through.
			end := -1
			for j := i + 1; j < len(runes); j++ {
				if runes[j] == '\'' {
					end = j
					break
				}
			}
			if end < 0 {
				return nil, fmt.Errorf("unclosed single quote")
			}
			cur.WriteString(string(runes[i+1 : end]))
			started = true
			i = end

		case '"':
			end := -1
			for j := i + 1; j < len(runes); j++ {
				if runes[j] == '\\' {
					j++
					continue
				}
				if runes[j] == '"' {
					end = j
					break
				}
			}
			if end < 0 {
				return nil, fmt.Errorf("unclosed double quote")
			}
			seg := runes[i+1 : end]
			for j := 0; j < len(seg); j++ {
				if seg[j] == '\\' && j+1 < len(seg) {
					j++
				}
				cur.WriteRune(seg[j])
			}
			started = true
			i = end

		default:
			if strings.ContainsRune("|&;<>()$`", c) {
				return nil, fmt.Errorf("%q is a shell metacharacter and there is no shell here; "+
					"quote it if it is meant literally", string(c))
			}
			cur.WriteRune(c)
			started = true
		}
	}
	flush()

	if len(args) == 0 {
		return nil, fmt.Errorf("the command line is empty")
	}
	return args, nil
}
