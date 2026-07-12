package spawn

import "strings"

// LinearAPIKeyEnvVar is the kernel-only Linear credential a worker must
// never receive (design doc threat model, B3): a shell-enabled DAC agent
// runs against untrusted Linear issue content, so it must not hold the
// credential that lets it write back to the board. AllowlistedEnv strips it
// unconditionally, even if a caller's allowlist names it by mistake. Daytona
// controller variables receive the same treatment: config.Load rejects them
// from the general allowlist, and the Daytona dispatcher path adds them later
// through its dedicated overlay. These checks are the last line of defense in
// the code that actually builds a worker's environment.
const LinearAPIKeyEnvVar = "LINEAR_API_KEY"

var controllerOnlyEnvVars = map[string]struct{}{
	LinearAPIKeyEnvVar: {},
	"DAYTONA_API_KEY":  {},
	"DAYTONA_API_URL":  {},
	"DAYTONA_TARGET":   {},
}

// AllowlistedEnv builds a worker's process environment from environ (in
// exec.Cmd.Env "KEY=VALUE" form — typically the dispatcher's own
// os.Environ()) by keeping only the entries whose key appears in allowlist.
// Anything not named in allowlist — plus the kernel/controller-only variables
// above unconditionally — is dropped, regardless of what environ carries. An
// allow-listed key absent from environ is simply omitted, never forwarded as
// an empty value. Entry order follows environ's order.
//
// This is the mechanism dispatcher.New's default WithEnvFor uses to
// guarantee a spawned worker never inherits the dispatcher's full
// environment (see the design doc's threat model, B3).
func AllowlistedEnv(environ []string, allowlist []string) []string {
	allowed := make(map[string]bool, len(allowlist))
	for _, key := range allowlist {
		if _, controllerOnly := controllerOnlyEnvVars[key]; controllerOnly {
			continue
		}
		allowed[key] = true
	}

	env := make([]string, 0, len(allowlist))
	for _, kv := range environ {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || !allowed[key] {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// RemoveEnv removes every entry whose variable name appears in names while
// preserving all other entries byte-for-byte and in order.
func RemoveEnv(environ []string, names ...string) []string {
	removed := make(map[string]struct{}, len(names))
	for _, name := range names {
		removed[name] = struct{}{}
	}
	filtered := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			filtered = append(filtered, entry)
			continue
		}
		if _, drop := removed[key]; drop {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}
