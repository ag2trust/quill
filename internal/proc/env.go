package proc

// baseEnv lists the variables every tool process inherits when set.
var baseEnv = []string{"PATH", "HOME", "USER", "LANG"}

// BuildEnv returns a clean tool environment: PATH, HOME, USER and LANG when
// set, TMPDIR=tmpdir, then each variable in names that is set. Duplicates are
// dropped, and ANTHROPIC_API_KEY is never included, even when named.
func BuildEnv(getenv func(string) string, names []string, tmpdir string) []string {
	var env []string
	seen := map[string]bool{"ANTHROPIC_API_KEY": true, "TMPDIR": true}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if v := getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	for _, name := range baseEnv {
		add(name)
	}
	env = append(env, "TMPDIR="+tmpdir)
	for _, name := range names {
		add(name)
	}
	return env
}
