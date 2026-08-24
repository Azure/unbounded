// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// release cuts a final directly from HEAD.
func (r *resolver) release() (string, error) {
	if r.req.Pre != "" {
		return "", fmt.Errorf("pre is only valid with mode=prerelease")
	}

	tag, err := bumpCore(r.latestFinal, r.req.Bump)
	if err != nil {
		return "", err
	}

	// Cutting the version a live train is heading for is not a fork, it is that
	// train being finalised the long way round, so only warn when they differ.
	if len(r.live) > 0 {
		if slices.Contains(r.live, tag) {
			r.warn("%s is the version its candidates were building toward, but mode=release cuts it from HEAD; use mode=promote to ship the tree that was soaked", tag)
		} else {
			r.warn("cutting %s while %s is still in flight; that train will be stranded",
				tag, strings.Join(r.live, " "))
		}
	}

	return tag, nil
}

// prerelease cuts the next candidate, starting or continuing a train.
func (r *resolver) prerelease() (string, error) {
	name, err := r.prereleaseCore()
	if err != nil {
		return "", err
	}

	// TagExists is repository-wide on purpose, and that has a consequence here:
	// a core whose final exists somewhere off this branch is excluded from the
	// live set, so nothing stopped a new train being started on it. Every run
	// then reported "Starting a new train" and minted another rc, and promote
	// could never finish any of them, because the tag it would create is taken.
	taken, err := r.repo.TagExists(name)
	if err != nil {
		return "", err
	}

	if taken {
		return "", fmt.Errorf(
			"%s already exists as a tag, though not on this branch; a train for it could never be promoted, so pick another bump or delete that tag", name)
	}

	have, err := r.maxRC(name)
	if err != nil {
		return "", err
	}

	pre := r.req.Pre

	if pre == "" {
		pre = "rc." + strconv.Itoa(have+1)
		r.note("Auto-selected %s for %s", pre, name)
	} else {
		// rc is the only suffix in use; see RELEASING.md. alpha and beta were
		// previously used interchangeably with no defined meaning.
		//
		// Leading zeros are rejected rather than normalised, because the shell
		// this replaces read rc.08 as an octal literal and silently skipped it.
		if !preShape.MatchString(pre) {
			return "", fmt.Errorf(
				"pre must look like rc.N with no leading zeros and at most nine digits (got '%s'); rc is the only prerelease suffix", pre)
		}

		// Cannot fail for a string preShape admitted, which bounds the suffix to
		// nine digits with no leading zero. Checked anyway rather than
		// discarded, for the same reason as parseCore: loosening the pattern
		// later must not turn an unparseable suffix into a silent zero, which
		// here would hand out a candidate number that already exists.
		want, err := strconv.Atoi(strings.TrimPrefix(pre, "rc."))
		if err != nil {
			return "", fmt.Errorf("pre is not a number: %s", pre)
		}

		if want <= have {
			return "", fmt.Errorf(
				"pre=%s is not ahead of %s-rc.%d; leave pre blank to take the next one automatically", pre, name, have)
		}
	}

	return name + "-" + pre, nil
}

// prereleaseCore decides which core the next candidate belongs to.
func (r *resolver) prereleaseCore() (string, error) {
	if r.req.AllowConcurrentTrains {
		// Explicit intent wins: bump decides the target even if that means a
		// second train.
		name, err := bumpCore(r.latestFinal, r.req.Bump)
		if err != nil {
			return "", err
		}

		if len(r.live) > 0 && !slices.Contains(r.live, name) {
			r.warn("starting a SECOND live train %s alongside %s; promote will now require an explicit version",
				name, strings.Join(r.live, " "))
		}

		return name, nil
	}

	switch {
	case len(r.live) > 1:
		return "", fmt.Errorf(
			"multiple live trains (%s); promote or delete one, or set allow_concurrent_trains to add another",
			strings.Join(r.live, " "))

	case len(r.live) == 1:
		// Continue the train in flight. bump is deliberately ignored rather
		// than validated: it is a required input with a default, so there is no
		// way to tell "the user chose patch" from "the user left the default",
		// and erroring would fire on the most ordinary invocation there is.
		name := r.live[0]
		r.note("Continuing live train %s (bump=%s ignored; it only applies when starting a train)", name, r.req.Bump)

		return name, nil

	default:
		name, err := bumpCore(r.latestFinal, r.req.Bump)
		if err != nil {
			return "", err
		}

		r.note("Starting a new train at %s (bump=%s from %s)", name, r.req.Bump, r.latestFinal)

		return name, nil
	}
}

// promote finalises a candidate, at the commit that was actually soaked.
func (r *resolver) promote() (tag, base string, err error) {
	if r.req.Pre != "" {
		return "", "", fmt.Errorf("pre is only valid with mode=prerelease")
	}

	switch {
	case r.req.Version != "":
		tag, err = r.promoteExplicit()
	case len(r.live) == 0:
		return "", "", fmt.Errorf(
			"no live prerelease train to promote; use mode=release to cut a final version directly")
	case len(r.live) > 1:
		// The v0.1.24 orphan came from guessing here. It no longer guesses.
		return "", "", fmt.Errorf(
			"multiple live trains (%s); pass version to say which one to promote", strings.Join(r.live, " "))
	default:
		tag = r.live[0]
		r.note("Promoting the only live train: %s", tag)
	}

	if err != nil {
		return "", "", err
	}

	base, err = r.candidateCommit(tag)
	if err != nil {
		return "", "", err
	}

	return tag, base, nil
}

// promoteExplicit validates a caller-supplied final version.
func (r *resolver) promoteExplicit() (string, error) {
	norm := "v" + strings.TrimPrefix(r.req.Version, "v")

	if !semverTag.MatchString(norm) {
		return "", fmt.Errorf(
			"version must be a final vX.Y.Z with no suffix, no leading zeros and at most nine digits per component, got: %s", r.req.Version)
	}

	tags, err := r.rcTags(norm)
	if err != nil {
		return "", err
	}

	if len(tags) == 0 {
		return "", fmt.Errorf("no prerelease train found for %s; use mode=release to cut it directly", norm)
	}

	// An explicit version must not be a way back to a train the resolver has
	// just reported as stale. Promoting v0.1.24 today would mint a final
	// release below v0.2.4 out of a candidate abandoned months ago.
	newer, err := greaterFinal(norm, r.latestFinal)
	if err != nil {
		return "", err
	}

	if !newer {
		return "", fmt.Errorf(
			"%s is older than the latest final %s; its candidates were abandoned, delete them rather than promoting them", norm, r.latestFinal)
	}

	return norm, nil
}

// candidateCommit resolves the commit of a core's highest rc: the tree that was
// actually built, deployed and smoke-tested.
func (r *resolver) candidateCommit(name string) (string, error) {
	highest, err := r.maxRC(name)
	if err != nil {
		return "", err
	}

	if highest == 0 {
		// The tag listing is the useful half of this during an incident: it
		// says what IS there, which is usually a malformed or legacy suffix
		// that discovery declined to treat as a candidate.
		existing, err := r.repo.AllTags(name + "-*")
		if err != nil {
			return "", err
		}

		return "", fmt.Errorf(
			"no rc tag found for %s (tags: %s); nothing to promote, use mode=release to cut it directly",
			name, strings.Join(existing, " "))
	}

	commit, err := r.repo.CommitOf(fmt.Sprintf("%s-rc.%d", name, highest))
	if err != nil {
		return "", fmt.Errorf("could not resolve the commit of %s-rc.%d: %w", name, highest, err)
	}

	return commit, nil
}

// validate applies the checks that guard every mode's answer.
func (r *resolver) validate(tag, base string) error {
	if !semverAnyTag.MatchString(tag) {
		return fmt.Errorf("computed tag is not vX.Y.Z[-suffix]: %s", tag)
	}

	// When cutting from a release-X.Y branch, the computed tag must stay inside
	// that series. Discovery is already scoped by reachability, so this should
	// be unreachable, and that is the point: it catches the cases where
	// reachability stops being true, such as a force-pushed release branch or
	// main merged into one. Getting this wrong mints a number the other branch
	// owns, which the tag-exists check below would only catch once that number
	// had already been taken.
	if r.req.Series != "" {
		if !seriesShape.MatchString(r.req.Series) {
			return fmt.Errorf("series must be X.Y with no leading zeros, got: %s", r.req.Series)
		}

		if !strings.HasPrefix(tag, "v"+r.req.Series+".") {
			return fmt.Errorf(
				"computed tag %s is outside series %s; a release-%s branch may only cut v%s.Z",
				tag, r.req.Series, r.req.Series, r.req.Series)
		}
	}

	exists, err := r.repo.TagExists(tag)
	if err != nil {
		return err
	}

	if exists {
		return fmt.Errorf("tag %s already exists", tag)
	}

	// A candidate cut from an unmerged branch, or orphaned by a force-push,
	// would ship a tree nobody reviewed on the default branch.
	ancestor, err := r.repo.IsAncestor(base)
	if err != nil {
		return err
	}

	if !ancestor {
		return fmt.Errorf(
			"%s is not an ancestor of HEAD; refusing to tag a commit that is not on the branch being released from", base)
	}

	return nil
}

// describeBase reports which tree is being tagged, and what it excludes.
func (r *resolver) describeBase(base string) error {
	head, err := r.repo.Head()
	if err != nil {
		return err
	}

	if base == head {
		r.note("Base commit: %s (HEAD)", base)

		return nil
	}

	subject, err := r.repo.Subject(base)
	if err != nil {
		return err
	}

	r.note("Base commit: %s (%s)", base, subject)

	count, err := r.repo.CountCommits(base)
	if err != nil {
		return err
	}

	r.note("Excluding %d commit(s) merged since that candidate was cut", count)

	return nil
}
