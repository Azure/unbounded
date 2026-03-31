package helpers

import (
	"sort"
	"strings"
)

func EnvSliceToMap(env []string) map[string]string {
	m := map[string]string{}

	for _, ev := range env {
		if parts := strings.SplitN(ev, "=", 2); len(parts) == 2 {
			m[parts[0]] = parts[1]
		} else {
			m[parts[0]] = ""
		}
	}

	return m
}

func EnvMapToSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))

	for k, v := range env {
		out = append(out, k+"="+v)
	}

	sort.Strings(out)

	return out
}
