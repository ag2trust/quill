package proc

// baseEnv lists the variables every tool process inherits when set.
var baseEnv = []string{"PATH", "HOME", "USER", "LANG"}

// BuildEnv returns a clean tool environment: PATH, HOME, USER and LANG when
// set, TMPDIR=tmpdir, then each variable in names that is set. Duplicates are
// dropped, and ANTHROPIC_API_KEY is never included, even when named. passed
// lists, once each and never nil, the names in names whose value was copied:
// unset or empty variables, ANTHROPIC_API_KEY and TMPDIR are left out.
func BuildEnv(getenv func(string) string, names []string, tmpdir string) (env, passed []string) {
	seen := map[string]bool{"ANTHROPIC_API_KEY": true, "TMPDIR": true}
	added := map[string]bool{}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if v := getenv(name); v != "" {
			env = append(env, name+"="+v)
			added[name] = true
		}
	}
	for _, name := range baseEnv {
		add(name)
	}
	env = append(env, "TMPDIR="+tmpdir)
	passed = []string{}
	for _, name := range names {
		add(name)
		if added[name] {
			passed = append(passed, name)
			delete(added, name) // list each name once
		}
	}
	return env, passed
}
