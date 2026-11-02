// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/Azure/unbounded/internal/racer/disks"
	"github.com/Azure/unbounded/internal/racer/workload"
)

var errAborted = errors.New("aborted")

// nodeDisks is one Node's view: its approved list and its dataplane report.
type nodeDisks struct {
	Node     string       `json:"node"`
	Pod      string       `json:"pod,omitempty"`
	Approved []string     `json:"approved"`
	Disks    []disks.Disk `json:"disks"`
	// Problem explains why Disks is empty, if it is.
	Problem string `json:"problem,omitempty"`

	report *disks.Report
	legacy bool
	// annotated reports whether the approval annotation is present, even if
	// it parses to no IDs.
	annotated bool
}

// dataplanePods returns dataplane Pods, optionally restricted to a Node.
func dataplanePods(ctx context.Context, c kubernetes.Interface, ns, node string) ([]corev1.Pod, error) {
	opts := metav1.ListOptions{LabelSelector: labels.SelectorFromSet(workload.Labels()).String()}
	if node != "" {
		opts.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", node).String()
	}

	l, err := c.CoreV1().Pods(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list racer dataplane pods: %w", err)
	}

	// Some fake/legacy servers ignore field selectors; filter again.
	out := l.Items[:0]
	for _, p := range l.Items {
		if node == "" || p.Spec.NodeName == node {
			out = append(out, p)
		}
	}

	return out, nil
}

// pickPod prefers a running, non-terminating Pod.
func pickPod(pods []corev1.Pod) *corev1.Pod {
	var best *corev1.Pod

	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}

		if best == nil || (p.Status.Phase == corev1.PodRunning && best.Status.Phase != corev1.PodRunning) {
			best = p
		}
	}

	if best == nil && len(pods) > 0 {
		best = &pods[0]
	}

	return best
}

// fillReport sets Pod, report, Disks, and Problem from a Pod.
func (nd *nodeDisks) fillReport(p *corev1.Pod) {
	if p == nil {
		nd.Problem = "no racer dataplane pod on this node"
		return
	}

	nd.Pod = p.Name

	v, ok := p.Annotations[disks.ReportAnnotation]
	if !ok {
		nd.Problem = fmt.Sprintf("pod %s has no %s annotation yet", p.Name, disks.ReportAnnotation)
		return
	}

	r, err := disks.ParseReport(v)
	if err != nil {
		nd.Problem = fmt.Sprintf("pod %s: %v", p.Name, err)
		return
	}

	if r.Disks == nil {
		nd.Problem = fmt.Sprintf("pod %s reports no disks; the dataplane may predate disk reporting", p.Name)
	}

	nd.report = r
	nd.Disks = slices.Clone(r.Disks)
	sort.Slice(nd.Disks, func(i, j int) bool { return nd.Disks[i].ID < nd.Disks[j].ID })
}

func fromNode(n *corev1.Node) *nodeDisks {
	_, legacy := n.Annotations[disks.LegacyBlockDevicesAnnotation]
	v, annotated := n.Annotations[disks.Annotation]

	return &nodeDisks{
		Node:      n.Name,
		Approved:  disks.ParseAnnotation(v),
		legacy:    legacy,
		annotated: annotated,
	}
}

func warnLegacy(w io.Writer, nd *nodeDisks) {
	if nd.legacy {
		printf(w, "Warning: node %s has the legacy %s annotation, which is no longer honored. Remove it with:\n  kubectl annotate node %s %s-\n",
			nd.Node, disks.LegacyBlockDevicesAnnotation, nd.Node, disks.LegacyBlockDevicesAnnotation)
	}
}

// loadNode reads one Node and its dataplane report.
func loadNode(ctx context.Context, c kubernetes.Interface, ns, node string) (*nodeDisks, error) {
	n, err := c.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", node, err)
	}

	nd := fromNode(n)

	pods, err := dataplanePods(ctx, c, ns, node)
	if err != nil {
		return nil, err
	}

	nd.fillReport(pickPod(pods))

	return nd, nil
}

// --- list ---

func listCommand(e *env) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "list [NODE...]",
		Short: "Show disks each Racer dataplane reports and which are approved",
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.list(cmd.Context(), args, output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: wide or json")

	return cmd
}

func (e *env) list(ctx context.Context, nodes []string, output string) error {
	if output != "" && output != "wide" && output != "json" {
		return fmt.Errorf("unsupported output %q: use wide or json", output)
	}

	c, err := e.kubeClient()
	if err != nil {
		return err
	}

	ns, err := e.ns()
	if err != nil {
		return err
	}

	var result []*nodeDisks

	if len(nodes) > 0 {
		for _, n := range nodes {
			nd, err := loadNode(ctx, c, ns, n)
			if err != nil {
				return err
			}

			result = append(result, nd)
		}
	} else {
		pods, err := dataplanePods(ctx, c, ns, "")
		if err != nil {
			return err
		}

		byNode := map[string][]corev1.Pod{}

		for _, p := range pods {
			if p.Spec.NodeName != "" {
				byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], p)
			}
		}

		for name, ps := range byNode {
			n, err := c.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("get node %s: %w", name, err)
			}

			nd := fromNode(n)
			nd.fillReport(pickPod(ps))
			result = append(result, nd)
		}
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Node < result[j].Node })

	for _, nd := range result {
		warnLegacy(e.errOut, nd)

		if nd.Problem != "" {
			printf(e.errOut, "Warning: node %s: %s\n", nd.Node, nd.Problem)
		}
	}

	if output == "json" {
		for _, nd := range result {
			if nd.Approved == nil {
				nd.Approved = []string{}
			}

			if nd.Disks == nil {
				nd.Disks = []disks.Disk{}
			}
		}

		if result == nil {
			result = []*nodeDisks{}
		}

		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")

		return enc.Encode(result)
	}

	wide := output == "wide"
	tw := tabwriter.NewWriter(e.out, 0, 4, 2, ' ', 0)

	header := "NODE\tID\tDEV\tSIZE\tSTATE\tREASON"
	if wide {
		header += "\tMODEL\tSERIAL\tALIASES"
	}

	printLine(tw, header)

	for _, nd := range result {
		for _, d := range nd.Disks {
			row := strings.Join([]string{nd.Node, d.ID, dash(d.Dev), humanSize(d.SizeBytes), d.State, dash(d.Reason)}, "\t")
			if wide {
				row += "\t" + strings.Join([]string{dash(d.Model), dash(d.Serial), dash(strings.Join(d.Aliases, ","))}, "\t")
			}

			printLine(tw, row)
		}
	}

	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// humanSize renders bytes with binary units, e.g. 931.5Gi.
func humanSize(b uint64) string {
	if b == 0 {
		return "-"
	}

	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}

	v := float64(b)
	units := []string{"Ki", "Mi", "Gi", "Ti", "Pi", "Ei"}

	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}

	return fmt.Sprintf("%.1f%s", v, units[i])
}

// --- shared mutation helpers ---

func (e *env) confirm(yes bool) error {
	if yes {
		return nil
	}

	if !e.isTerminal() {
		return errors.New("refusing to continue without confirmation: stdin is not a terminal; pass --yes")
	}

	printf(e.out, "Type 'yes' to continue: ")

	line, err := bufio.NewReader(e.in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	if strings.TrimSpace(line) != "yes" {
		return errAborted
	}

	return nil
}

// updateApproved applies fn to the Node's approved list and writes it with
// Update, whose resourceVersion check fails with a Conflict if the Node
// changed since the read; conflicts are retried. Every change also sets
// disks.RevisionAnnotation to a fresh token, which it returns. When the
// approved list is unchanged it returns the Node's existing token, writing a
// fresh one only if that token is absent or invalid; a token-only change does
// not restart the dataplane. It returns "" only when nothing is approved
// before or after. If the existing annotation is invalid, the
// dataplane approves no disks, so any result other than clearing it is
// refused rather than turning its entries into effective approval.
func updateApproved(ctx context.Context, c kubernetes.Interface, node string, fn func([]string) []string) (string, error) {
	changed := ""

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		changed = ""

		n, err := c.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil {
			return err
		}

		old, had := n.Annotations[disks.Annotation]
		newIDs := fn(disks.ParseAnnotation(old))

		if had && len(newIDs) > 0 {
			if err := disks.ValidateAnnotation(old); err != nil {
				return fmt.Errorf("existing annotation on Node %s is invalid and approves no disks; clear it with 'revoke --all' first: %w", node, err)
			}
		}

		if len(newIDs) == 0 {
			if !had {
				return nil
			}

			delete(n.Annotations, disks.Annotation)
		} else {
			v, err := disks.FormatAnnotation(newIDs)
			if err != nil {
				return err
			}

			if had && v == old {
				if cur := n.Annotations[disks.RevisionAnnotation]; disks.ValidRevision(cur) {
					changed = cur

					return nil
				}
			}

			if n.Annotations == nil {
				n.Annotations = map[string]string{}
			}

			n.Annotations[disks.Annotation] = v
		}

		rev, err := disks.NewRevision()
		if err != nil {
			return err
		}

		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}

		n.Annotations[disks.RevisionAnnotation] = rev

		if _, err := c.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
			return err
		}

		changed = rev

		return nil
	})

	return changed, err
}

// --- allow ---

type allowOpts struct {
	yes, wait, force bool
	timeout          time.Duration
}

func allowCommand(e *env) *cobra.Command {
	var o allowOpts

	cmd := &cobra.Command{
		Use:   "allow NODE ID...",
		Short: "Approve disks for Racer; Racer will erase their first and last 1 MiB",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.allow(cmd.Context(), args[0], args[1:], o)
		},
	}
	cmd.Flags().BoolVar(&o.yes, "yes", false, "Skip the confirmation prompt")
	cmd.Flags().BoolVar(&o.wait, "wait", false, "Wait until each approved disk is claimed or rejected")
	cmd.Flags().BoolVar(&o.force, "force", false, "Approve disks that are unavailable or not in the report")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 5*time.Minute, "How long --wait waits")

	return cmd
}

func (e *env) allow(ctx context.Context, node string, ids []string, o allowOpts) error {
	for _, id := range ids {
		if err := disks.ValidateID(id); err != nil {
			return err
		}
	}

	c, err := e.kubeClient()
	if err != nil {
		return err
	}

	ns, err := e.ns()
	if err != nil {
		return err
	}

	nd, err := loadNode(ctx, c, ns, node)
	if err != nil {
		return err
	}

	warnLegacy(e.errOut, nd)

	if nd.report == nil || nd.report.Disks == nil {
		printf(e.errOut, "Warning: node %s: %s\n", node, nd.Problem)
	}

	var (
		chosen []disks.Disk
		errs   []string
	)

	for _, id := range ids {
		d, ok := nd.report.Find(id)
		switch {
		case !ok && !o.force:
			errs = append(errs, fmt.Sprintf("%s: not reported by the Racer pod on %s (use --force to approve anyway)", id, node))
		case !ok:
			d = disks.Disk{ID: id, State: "unknown"}
		case d.State == disks.StateUnavailable && !o.force:
			errs = append(errs, fmt.Sprintf("%s: unavailable (%s) (use --force to approve anyway)", d.ID, dash(d.Reason)))
		}

		if (ok || o.force) && !slices.ContainsFunc(chosen, func(x disks.Disk) bool { return x.ID == d.ID }) {
			chosen = append(chosen, d)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("refusing to approve:\n  %s", strings.Join(errs, "\n  "))
	}

	var fresh []disks.Disk

	for _, d := range chosen {
		if slices.Contains(nd.Approved, d.ID) {
			printf(e.out, "%s is already approved on %s\n", d.ID, node)
			continue
		}

		fresh = append(fresh, d)
	}

	if len(fresh) == 0 {
		printLine(e.out, "Nothing to do.")
		return nil
	}

	printf(e.out, "Disks to approve on node %s:\n", node)

	for _, d := range fresh {
		printf(e.out, "  %s  dev=%s size=%s model=%s serial=%s state=%s\n",
			d.ID, dash(d.Dev), humanSize(d.SizeBytes), dash(d.Model), dash(d.Serial), d.State)
	}

	printf(e.out, `
WARNING: Racer will ERASE the first and last 1 MiB of each disk above,
destroying its partition table and any filesystem, LVM, or RAID signatures.
The Racer pod on %s will restart and drop its cache.

`, node)

	if err := e.confirm(o.yes); err != nil {
		return err
	}

	rev, err := updateApproved(ctx, c, node, func(cur []string) []string {
		for _, d := range fresh {
			cur = append(cur, d.ID)
		}

		return cur
	})
	if err != nil {
		return fmt.Errorf("update node %s: %w", node, err)
	}

	printf(e.out, "Approved %d disk(s) on %s.\n", len(fresh), node)

	if !o.wait {
		return nil
	}

	want := make([]string, 0, len(fresh))
	for _, d := range fresh {
		want = append(want, d.ID)
	}

	return e.waitClaimed(ctx, c, ns, node, rev, want, o.timeout)
}

// waitClaimed polls until every id is claimed or rejected in a report whose
// DisksRevision is rev. A report with another revision may predate the
// approval change, for example from a process that is still restarting, so
// its states are ignored. An empty rev disables the check.
func (e *env) waitClaimed(ctx context.Context, c kubernetes.Interface, ns, node, rev string, ids []string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	t := time.NewTicker(e.pollInterval)
	defer t.Stop()

	for {
		pods, err := dataplanePods(ctx, c, ns, node)
		if err == nil {
			nd := &nodeDisks{Node: node}
			nd.fillReport(pickPod(pods))

			if rev != "" && (nd.report == nil || nd.report.DisksRevision != rev) {
				nd.report = nil
			}

			done, rejected := 0, 0

			for _, id := range ids {
				d, ok := nd.report.Find(id)
				if !ok {
					continue
				}

				switch d.State {
				case disks.StateClaimed:
					done++
				case disks.StateRejected:
					done++
					rejected++
				}
			}

			if done == len(ids) {
				for _, id := range ids {
					d, _ := nd.report.Find(id)
					if d.State == disks.StateRejected {
						printf(e.out, "%s: rejected (%s)\n", id, dash(d.Reason))
					} else {
						printf(e.out, "%s: claimed\n", id)
					}
				}

				if rejected > 0 {
					return fmt.Errorf("%d disk(s) rejected on %s", rejected, node)
				}

				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for disks on %s to be claimed (the Racer pod reports the approval revision it applied; older dataplanes do not)", node)
		case <-t.C:
		}
	}
}

// --- revoke ---

func revokeCommand(e *env) *cobra.Command {
	var yes, all bool

	cmd := &cobra.Command{
		Use:   "revoke NODE (ID...|--all)",
		Short: "Withdraw approval for disks; nothing is erased",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.revoke(cmd.Context(), args[0], args[1:], all, yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	cmd.Flags().BoolVar(&all, "all", false, "Revoke every approved disk")

	return cmd
}

func (e *env) revoke(ctx context.Context, node string, ids []string, all, yes bool) error {
	if all == (len(ids) > 0) {
		return errors.New("specify disk IDs or --all, not both")
	}

	c, err := e.kubeClient()
	if err != nil {
		return err
	}

	ns, err := e.ns()
	if err != nil {
		return err
	}

	nd, err := loadNode(ctx, c, ns, node)
	if err != nil {
		return err
	}

	warnLegacy(e.errOut, nd)

	var remove []string

	if all {
		remove = slices.Clone(nd.Approved)
	} else {
		for _, id := range ids {
			// A disk may be approved under its preferred ID or any alias.
			// Remove every approved name for it so consent is fully withdrawn.
			names := []string{id}
			if d, ok := nd.report.Find(id); ok {
				names = append([]string{d.ID}, d.Aliases...)
			}

			found := false

			for _, n := range names {
				if slices.Contains(nd.Approved, n) {
					remove = append(remove, n)
					found = true
				}
			}

			if !found {
				return fmt.Errorf("%s is not approved on %s", id, node)
			}
		}
	}

	// --all also clears an annotation that is present but approves no IDs,
	// such as an empty or invalid value, so it is always a recovery path.
	clearOnly := all && len(remove) == 0 && nd.annotated

	if len(remove) == 0 && !clearOnly {
		printLine(e.out, "Nothing to do.")
		return nil
	}

	slices.Sort(remove)
	remove = slices.Compact(remove)

	if clearOnly {
		printf(e.out, "The %s annotation on node %s approves no disks and will be removed.\n\n", disks.Annotation, node)
	} else {
		printf(e.out, "Disks to revoke on node %s:\n  %s\n\n", node, strings.Join(remove, "\n  "))
	}
	printf(e.out, "Racer stops using these disks. Nothing is erased; existing data is left on disk.\nThe Racer pod on %s will restart and drop its cache.\n\n", node)

	if err := e.confirm(yes); err != nil {
		return err
	}

	_, err = updateApproved(ctx, c, node, func(cur []string) []string {
		if all {
			return nil
		}

		return slices.DeleteFunc(cur, func(s string) bool { return slices.Contains(remove, s) })
	})
	if err != nil {
		return fmt.Errorf("update node %s: %w", node, err)
	}

	printf(e.out, "Revoked %d disk(s) on %s.\n", len(remove), node)

	return nil
}

// printf writes best-effort terminal output. A failed write to the user's
// terminal is not actionable, so the error is dropped after the check.
func printf(w io.Writer, format string, a ...any) {
	if _, err := fmt.Fprintf(w, format, a...); err != nil {
		return
	}
}

func printLine(w io.Writer, a ...any) {
	if _, err := fmt.Fprintln(w, a...); err != nil {
		return
	}
}
