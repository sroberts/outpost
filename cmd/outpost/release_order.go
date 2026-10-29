package main

import (
	"strconv"
	"strings"
)

// compareReleases orders two release tags as semantic versions, with or
// without a leading v: -1 if a is older than b, 0 if they are the same
// version, 1 if a is newer. ok is false when either tag isn't a version, and
// then nothing can be said about which is newer.
func compareReleases(a, b string) (cmp int, ok bool) {
	va, okA := parseVersion(a)
	vb, okB := parseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range va.core {
		if va.core[i] != vb.core[i] {
			return sign(va.core[i] - vb.core[i]), true
		}
	}
	return comparePrerelease(va.pre, vb.pre), true
}

type semver struct {
	core [3]int
	pre  []string
}

// parseVersion reads v1, v1.2 and v1.2.3, with an optional -prerelease and
// +build, which is ignored as semver says it should be.
func parseVersion(tag string) (semver, bool) {
	s := strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v semver
	if i := strings.IndexByte(s, '-'); i >= 0 {
		if s[i+1:] == "" {
			return v, false
		}
		v.pre = strings.Split(s[i+1:], ".")
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return v, false
		}
		v.core[i] = n
	}
	return v, true
}

// comparePrerelease follows semver: a release is newer than any prerelease of
// it, numeric identifiers sort numerically and below alphanumeric ones, and a
// shorter list sorts first when it is a prefix of the longer.
func comparePrerelease(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		na, errA := strconv.Atoi(a[i])
		nb, errB := strconv.Atoi(b[i])
		switch {
		case errA == nil && errB == nil:
			if na != nb {
				return sign(na - nb)
			}
		case errA == nil:
			return -1
		case errB == nil:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(a) - len(b))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
