// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Mode is what kind of release is being cut.
type Mode string

// The three modes.
const (
	ModeRelease    Mode = "release"
	ModePrerelease Mode = "prerelease"
	ModePromote    Mode = "promote"
)

// Semver shapes, defined once.
//
// Every pattern is built from the same component, because three of them used to
// differ: discovery bounded the digits, the promote input did not, and the final
// check on the computed tag did not either, so a bump could mint a ten-digit
// version that passed on the way out and was invisible to discovery on the way
// back in.
//
// Nine digits: the shell this replaces used bash arithmetic, which is signed
// 64-bit and wraps in silence. Go would not wrap, but the limit stays because
// the tags it produced have to remain discoverable by the same patterns.
//
// No leading zeros: v01.2.3 is not a version, and version sorts do not order it
// where a reader would expect.
const (
	semverComponent = `(0|[1-9][0-9]{0,8})`
	rcNumber        = `[1-9][0-9]{0,8}`
)

var (
	semverTag    = regexp.MustCompile(`^v` + semverComponent + `\.` + semverComponent + `\.` + semverComponent + `$`)
	semverRCTag  = regexp.MustCompile(`^v` + semverComponent + `\.` + semverComponent + `\.` + semverComponent + `-rc\.` + rcNumber + `$`)
	semverAnyTag = regexp.MustCompile(`^v` + semverComponent + `\.` + semverComponent + `\.` + semverComponent + `(-rc\.` + rcNumber + `)?$`)
	seriesShape  = regexp.MustCompile(`^` + semverComponent + `\.` + semverComponent + `$`)
	preShape     = regexp.MustCompile(`^rc\.` + rcNumber + `$`)
)

// zeroVersion is the floor used when a repository has no finals yet.
const zeroVersion = "v0.0.0"

// Repo is the git history a resolution is computed against.
//
// An interface because every question here is about tags and ancestry, and
// answering them from a fixture is what makes the resolver testable without a
// repository per case.
type Repo interface {
	// ReachableTags lists tags matching a glob that are ancestors of HEAD.
	ReachableTags(pattern string) ([]string, error)
	// AllTags lists tags matching a glob ANYWHERE, reachable or not.
	//
	// Separate from ReachableTags because they answer different questions, and
	// conflating them is a real bug in both directions. Discovery must be
	// reachability-scoped so a stray tag on someone's branch cannot drive the
	// numbering; the Latest decision must NOT be, because a release branch's
	// own tags are invisible from main and are exactly what could outrank it.
	AllTags(pattern string) ([]string, error)
	// TagExists reports whether a tag exists ANYWHERE, not only on this branch.
	TagExists(tag string) (bool, error)
	// Head returns the commit HEAD points at.
	Head() (string, error)
	// CommitOf resolves a tag to its commit.
	CommitOf(tag string) (string, error)
	// IsAncestor reports whether commit is an ancestor of HEAD.
	IsAncestor(commit string) (bool, error)
	// CountCommits counts commits in from..HEAD.
	CountCommits(from string) (int, error)
	// Subject returns a commit's subject line.
	Subject(commit string) (string, error)
}

// Request is what the caller is asking for.
type Request struct {
	Mode Mode
	// Bump only applies when STARTING a train; it is ignored when continuing one.
	Bump Bump
	// Series confines the result to X.Y, set when cutting from a release branch.
	Series string
	// Pre is an explicit prerelease suffix, e.g. rc.3.
	Pre string
	// Version is an explicit final for promote.
	Version string
	// AllowConcurrentTrains permits starting a second live train.
	AllowConcurrentTrains bool
}

// Result is the resolution, plus everything the operator needs to see.
type Result struct {
	// Tag is the tag to create.
	Tag string
	// Base is the commit to create it at.
	Base string
	// LatestFinal is the highest final reachable from HEAD, or v0.0.0.
	LatestFinal string
	// Live are cores still in flight.
	Live []string
	// Stale are cores whose candidates were abandoned.
	Stale []string
	// Report is the human-facing state report, in order.
	Report []string
	// Warnings are conditions worth surfacing that do not refuse.
	Warnings []string
}

// core is a parsed vX.Y.Z.
type core struct {
	major, minor, patch int
}

func parseCore(tag string) (core, bool) {
	match := semverTag.FindStringSubmatch(tag)
	if match == nil {
		return core{}, false
	}

	// Cannot fail for a string the pattern admitted, which bounds each
	// component to nine digits. Checked anyway rather than discarded, so that
	// loosening the pattern later cannot turn an unparseable component into a
	// silent zero.
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return core{}, false
	}

	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return core{}, false
	}

	patch, err := strconv.Atoi(match[3])
	if err != nil {
		return core{}, false
	}

	return core{major, minor, patch}, true
}

func (c core) String() string {
	return fmt.Sprintf("v%d.%d.%d", c.major, c.minor, c.patch)
}

// compare orders two cores, -1, 0 or 1.
func (c core) compare(other core) int {
	if got := cmp.Compare(c.major, other.major); got != 0 {
		return got
	}

	if got := cmp.Compare(c.minor, other.minor); got != 0 {
		return got
	}

	return cmp.Compare(c.patch, other.patch)
}

// greaterFinal reports whether a is a higher FINAL than b.
//
// Finals only, and that is enforced rather than assumed. The shell this
// replaces used `sort -V`, which does not implement semver precedence for
// prereleases: it ranks v1.0.0 BELOW v1.0.0-rc.1, where clause 11.3 requires
// the opposite. Every caller here compares bare cores already, so the refusal
// never fires today. It exists so a future caller passing a prerelease fails
// loudly instead of silently inverting the comparison.
func greaterFinal(a, b string) (bool, error) {
	if strings.Contains(a, "-") || strings.Contains(b, "-") {
		return false, fmt.Errorf(
			"greaterFinal compares finals only, got: %s %s (prerelease precedence is not version-sort order)", a, b)
	}

	left, ok := parseCore(a)
	if !ok {
		return false, fmt.Errorf("not a final version: %s", a)
	}

	right, ok := parseCore(b)
	if !ok {
		return false, fmt.Errorf("not a final version: %s", b)
	}

	return left.compare(right) > 0, nil
}

// resolver holds the state one resolution works from.
type resolver struct {
	repo Repo
	req  Request

	latestFinal string
	live        []string
	stale       []string

	report   []string
	warnings []string
}

func (r *resolver) note(format string, args ...any) {
	r.report = append(r.report, fmt.Sprintf(format, args...))
}

func (r *resolver) warn(format string, args ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, args...))
}

// Resolve works out which tag a release should mint, and at which commit.
//
// Why Base exists: promote finalizes a candidate that was already built,
// deployed and smoke-tested, so tagging HEAD would ship a DIFFERENT tree under
// a version whose only claim to being trustworthy is that soak. promote
// resolves the candidate's commit; release and prerelease resolve HEAD.
func Resolve(repo Repo, req Request) (*Result, error) {
	r := &resolver{repo: repo, req: req}

	if err := r.discover(); err != nil {
		return nil, err
	}

	base, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD: %w", err)
	}

	var tag string

	switch req.Mode {
	case ModeRelease:
		tag, err = r.release()
	case ModePrerelease:
		tag, err = r.prerelease()
	case ModePromote:
		tag, base, err = r.promote()
	default:
		return nil, fmt.Errorf("unexpected mode: %s", req.Mode)
	}

	if err != nil {
		return nil, err
	}

	if err := r.validate(tag, base); err != nil {
		return nil, err
	}

	if err := r.describeBase(base); err != nil {
		return nil, err
	}

	r.note("Computed tag: %s", tag)

	return &Result{
		Tag:         tag,
		Base:        base,
		LatestFinal: r.latestFinal,
		Live:        r.live,
		Stale:       r.stale,
		Report:      r.report,
		Warnings:    r.warnings,
	}, nil
}

// discover reads the repository state every mode depends on.
func (r *resolver) discover() error {
	final, err := r.latest()
	if err != nil {
		return err
	}

	r.latestFinal = final

	cores, err := r.trainCores()
	if err != nil {
		return err
	}

	for _, c := range cores {
		// Repository-wide on purpose: the question is whether the name is
		// already taken, and it is taken wherever the tag exists.
		taken, err := r.repo.TagExists(c)
		if err != nil {
			return err
		}

		if taken {
			continue
		}

		newer, err := greaterFinal(c, final)
		if err != nil {
			return err
		}

		if newer {
			r.live = append(r.live, c)
		} else {
			r.stale = append(r.stale, c)
		}
	}

	r.note("Latest final: %s", final)

	if len(r.live) > 0 {
		r.note("Live trains:  %s", strings.Join(r.live, " "))
	} else {
		r.note("Live trains:  (none)")
	}

	if len(r.stale) > 0 {
		r.note("Stale trains: %s (superseded by %s; their tags can be deleted)",
			strings.Join(r.stale, " "), final)
	}

	return nil
}

// latest returns the highest final reachable from HEAD, or v0.0.0.
//
// Tags anywhere else in the repository must not influence the numbering: a
// v9.0.0 cut on someone's feature branch would otherwise become the latest
// final and make the next release from main v9.0.1. Reachability is also what
// scopes a release branch to its own series without any explicit filtering,
// since v0.4.0 cut on main is simply not an ancestor of release-0.3.
func (r *resolver) latest() (string, error) {
	tags, err := r.repo.ReachableTags("v[0-9]*")
	if err != nil {
		return "", err
	}

	best := zeroVersion
	bestCore, _ := parseCore(best)

	for _, tag := range tags {
		c, ok := parseCore(tag)
		if !ok {
			continue
		}

		if c.compare(bestCore) > 0 {
			best, bestCore = tag, c
		}
	}

	return best, nil
}

// trainCores lists every core that has canonical rc tags, in version order.
func (r *resolver) trainCores() ([]string, error) {
	tags, err := r.repo.ReachableTags("v[0-9]*-rc.*")
	if err != nil {
		return nil, err
	}

	seen := map[string]core{}

	for _, tag := range tags {
		if !semverRCTag.MatchString(tag) {
			continue
		}

		name := tag[:strings.LastIndex(tag, "-rc.")]
		if c, ok := parseCore(name); ok {
			seen[name] = c
		}
	}

	cores := make([]string, 0, len(seen))
	for name := range seen {
		cores = append(cores, name)
	}

	slices.SortFunc(cores, func(a, b string) int { return seen[a].compare(seen[b]) })

	return cores, nil
}

// rcTags lists the canonical candidates for one core.
//
// Discovery, numbering and promotion must agree on this definition: a stray
// alpha, malformed rc or bare suffix is repository metadata, not a live train.
func (r *resolver) rcTags(name string) ([]string, error) {
	tags, err := r.repo.ReachableTags(name + "-rc.*")
	if err != nil {
		return nil, err
	}

	prefix := name + "-rc."
	canonical := make([]string, 0, len(tags))

	for _, tag := range tags {
		if semverRCTag.MatchString(tag) && strings.HasPrefix(tag, prefix) {
			canonical = append(canonical, tag)
		}
	}

	return canonical, nil
}

// maxRC returns the highest rc number cut for a core, or 0.
//
// Numeric on purpose: v0.1.24 ran to rc.18, and a lexical maximum reports rc.9,
// which would hand out rc.10 a second time.
func (r *resolver) maxRC(name string) (int, error) {
	tags, err := r.rcTags(name)
	if err != nil {
		return 0, err
	}

	highest := 0
	prefix := name + "-rc."

	for _, tag := range tags {
		n, err := strconv.Atoi(strings.TrimPrefix(tag, prefix))
		if err != nil {
			continue
		}

		highest = max(highest, n)
	}

	return highest, nil
}

// bumpCore advances a version by one level.
func bumpCore(base string, level Bump) (string, error) {
	c, ok := parseCore(base)
	if !ok {
		return "", fmt.Errorf("cannot bump %s: not a final version", base)
	}

	switch level {
	case BumpMajor:
		c.major++
		c.minor, c.patch = 0, 0
	case BumpMinor:
		c.minor++
		c.patch = 0
	case BumpPatch:
		c.patch++
	default:
		return "", fmt.Errorf("unexpected bump level: %s", level)
	}

	// Said here rather than left to the pattern check at the end, which would
	// report "not vX.Y.Z" for a version that is plainly vX.Y.Z and merely too
	// big for the arithmetic that produced it.
	if digits(c.major) > 9 || digits(c.minor) > 9 || digits(c.patch) > 9 {
		return "", fmt.Errorf(
			"bumping %s by %s overflows a version component; nine digits is the limit", base, level)
	}

	return c.String(), nil
}

func digits(n int) int {
	return len(strconv.Itoa(n))
}

// CheckSeries reports whether a series is well formed, as X.Y with no leading
// zeros and no leading v.
//
// Exported so the commands can reject a malformed series before spending a
// workflow dispatch to learn the same thing, while the regex and the message
// stay defined once. The resolver applies it too, where getting it wrong mints
// a number another branch owns.
func CheckSeries(series string) error {
	if !seriesShape.MatchString(series) {
		return fmt.Errorf("series must be X.Y with no leading zeros, got: %s", series)
	}

	return nil
}
