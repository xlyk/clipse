package spawn

import "strings"

// LinearAPIKeyEnvVar is the kernel-only Linear credential a worker must
// never receive (design doc threat model, B3): a shell-enabled DAC agent
// runs against untrusted Linear issue content, so it must not hold the
// credential that lets it write back to the board. AllowlistedEnv strips it
// unconditionally, even if a caller's allowlist names it by mistake —
// config.Load's validation already rejects that at config-load time, but
// this is the last line of defense in the code that actually builds a
// worker's environment.
const LinearAPIKeyEnvVar = "LINEAR_API_KEY"

// AllowlistedEnv builds a worker's process environment from environ (in
// exec.Cmd.Env "KEY=VALUE" form — typically the dispatcher's own
// os.Environ()) by keeping only the entries whose key appears in allowlist.
// Anything not named in allowlist — and LinearAPIKeyEnvVar unconditionally —
// is dropped, regardless of what environ carries. An allow-listed key absent
// from environ is simply omitted, never forwarded as an empty value. Entry
// order follows environ's order.
//
// This is the mechanism dispatcher.New's default WithEnvFor uses to
// guarantee a spawned worker never inherits the dispatcher's full
// environment (see the design doc's threat model, B3).
func AllowlistedEnv(environ []string, allowlist []string) []string {
	allowed := make(map[string]bool, len(allowlist))
	for _, key := range allowlist {
		if key == LinearAPIKeyEnvVar {
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
