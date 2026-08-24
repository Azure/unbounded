// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

import (
	"fmt"
	"regexp"

	"golang.org/x/mod/semver"
)

// Classification is what happens to a release once it has been built.
type Classification struct {
	// FromMain says whether this release soaks on unbounded-stable.
	FromMain bool
	// Latest says whether the GitHub release is marked Latest.
	Latest bool
	// Report is the reasoning, for the operator.
	Report []string
}

// classifyTag is the shape a release tag may take.
//
// Deliberately broader than the resolver's: this classifies whatever was
// actually tagged, including the legacy alpha and beta suffixes that predate
// rc being the only one. Tightening it to the resolver's pattern would make a
// historical tag unclassifiable rather than merely unfashionable.
var classifyTag = regexp.MustCompile(
	`^v` + semverComponent + `\.` + semverComponent + `\.` + semverComponent + `(-[0-9A-Za-z.-]+)?$`)

// seriesOf extracts the X.Y a tag belongs to.
var seriesOf = regexp.MustCompile(`^v([0-9]+\.[0-9]+)\.`)

// Classify decides whether a release soaks, and whether it is Latest.
//
// There are two questions here and they are NOT the same question. An earlier
// design answered both with one version comparison and got the first one wrong.
//
//	FromMain  Should this soak on unbounded-stable?
//
//	          That cluster soaks main, and only main. A release cut from a
//	          release-X.Y branch has its own soak story, and deploying it to
//	          stable would replace whatever main last put there.
//
//	          Deliberately a question about PROVENANCE, not version ordering.
//	          Ordering cannot answer it: stable can be running a candidate newer
//	          than the newest final, so "is this version the highest" says
//	          deploy when the honest answer is that this release has no business
//	          on that cluster at all.
//
//	          Assumes release branches are never merged back into main. The flow
//	          is one-way: fixes land on main and are cherry-picked down, so a
//	          branch's commits stay off main and its tags stay unreachable.
//
//	Latest    Should the GitHub release be marked Latest?
//
//	          This one IS about version ordering, because Latest is what
//	          releases/latest/download resolves to, which is the install command
//	          in README.md and every guide. Publishing v0.3.1 after v0.5.0 must
//	          not repoint those at v0.3.1.
//
//	          GitHub defaults make_latest to true on any newly published
//	          release, so the answer must be passed explicitly on every publish.
//
// Expects repo to be a checkout of the DEFAULT BRANCH with full history and
// tags, so HEAD is main.
func Classify(repo Repo, tag string) (*Classification, error) {
	if tag == "" {
		return nil, fmt.Errorf("no tag given")
	}

	// Checked here even though release-upgrade validates it too: this is the
	// thing deciding whether a cluster gets touched, and it should not depend
	// on a caller two workflows away.
	if !classifyTag.MatchString(tag) {
		return nil, fmt.Errorf("not a release tag: %s", tag)
	}

	commit, err := repo.CommitOf(tag)
	if err != nil {
		return nil, fmt.Errorf("tag %s does not exist here; the checkout needs full history and tags", tag)
	}

	c := &Classification{}

	c.FromMain, err = repo.IsAncestor(commit)
	if err != nil {
		return nil, err
	}

	if c.FromMain {
		c.note("%s is reachable from HEAD: cut from the default branch, so it soaks.", tag)
	} else {
		c.note("%s is NOT reachable from HEAD: cut from a release branch, so it does not soak.", tag)
	}

	if err := c.decideLatest(repo, tag); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Classification) note(format string, args ...any) {
	c.Report = append(c.Report, fmt.Sprintf(format, args...))
}

// decideLatest answers whether anything outranks this tag.
func (c *Classification) decideLatest(repo Repo, tag string) error {
	if semver.Prerelease(tag) != "" {
		// GitHub refuses to mark a prerelease as Latest, so asking for it is at
		// best ignored. Answer honestly rather than sending a flag that cannot
		// apply.
		c.Latest = false
		c.note("%s is a prerelease; a prerelease can never be Latest.", tag)

		return nil
	}

	// Everything on the trunk, plus everything in this tag's own series. That
	// is exactly the set of releases that could legitimately outrank it:
	//
	//   - Tags reachable from HEAD are main's releases. Scoping to them keeps a
	//     stray final cut on someone's feature branch from suppressing Latest
	//     forever, the same reason tag discovery is reachability-scoped.
	//   - Tags in the same series are the branch's own, which reachability
	//     cannot see from main. Without them, republishing v0.3.1 while v0.3.2
	//     exists would mark the older one Latest, flipping the marker
	//     backwards, which is the bug this check exists to prevent.
	reachable, err := repo.ReachableTags("v*")
	if err != nil {
		return err
	}

	candidates := append([]string(nil), reachable...)

	if match := seriesOf.FindStringSubmatch(tag); match != nil {
		sameSeries, err := repo.AllTags("v" + match[1] + ".*")
		if err != nil {
			return err
		}

		candidates = append(candidates, sameSeries...)
	}

	highest := highestFinal(candidates)
	if highest == "" {
		// Tags exist but none is a final release. Nothing has shipped, so
		// nothing can outrank this.
		c.Latest = true
		c.note("no final release tags found; treating as not a maintenance release")

		return nil
	}

	// True semver precedence, NOT greaterFinal. That function refuses
	// prereleases on purpose, because version-sort order ranks v1.0.0 below
	// v1.0.0-rc.1 where clause 11.3 requires the opposite. This comparison may
	// legitimately involve a prerelease, so it uses the semver package. Do not
	// "simplify" the two into one.
	superseded := semver.Compare(tag, highest) < 0

	c.Latest = !superseded

	if superseded {
		c.note("highest release: %s; %s is a maintenance release", highest, tag)
	} else {
		c.note("highest release: %s; %s is not a maintenance release", highest, tag)
	}

	return nil
}

// highestFinal returns the highest canonical final among tags, or empty.
func highestFinal(tags []string) string {
	highest := ""

	for _, tag := range tags {
		// Canonical only: v01.2.3 and v1.2 are repository metadata, not
		// releases, and must not be able to outrank anything.
		if !semver.IsValid(tag) || semver.Canonical(tag) != tag {
			continue
		}

		if semver.Prerelease(tag) != "" {
			continue
		}

		if highest == "" || semver.Compare(tag, highest) > 0 {
			highest = tag
		}
	}

	return highest
}
