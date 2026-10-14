// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// firstBootOwner authorizes the installed OS after handoff, even when the ISO
// bytes are released or the operation is terminal. Final reports end this claim.
func (p *MediaProvider) firstBootOwner(ctx context.Context, ip string) (*mediaOwner, bool, error) {
	if p.Client == nil {
		return nil, true, fmt.Errorf("media client unavailable")
	}

	var ops v1alpha3.MachineOperationList
	if err := p.Client.List(ctx, &ops); err != nil {
		return nil, true, err
	}

	var found *mediaOwner

	for i := range ops.Items {
		op := &ops.Items[i]
		if op.Spec.OperationKind != v1alpha3.OperationHostReplace {
			continue
		}

		for j := range op.Status.Targets {
			target := &op.Status.Targets[j]

			b := target.BootMedia
			if !mediaInSite(b, p.Site) || !mediaFirstBootPending(*target) || b.Netboot.DHCPLeases[0].IPv4 != ip {
				continue
			}

			if found != nil {
				return nil, true, fmt.Errorf("ambiguous media first-boot lease")
			}

			if b.MachineUID == "" || validateMediaLease(b.Netboot) != nil {
				return nil, true, fmt.Errorf("invalid frozen media identity or lease")
			}

			var machine v1alpha3.Machine
			if err := p.Client.Get(ctx, client.ObjectKey{Name: target.MachineRef}, &machine); err != nil {
				return nil, true, err
			}

			if machine.UID != b.MachineUID || !machine.DeletionTimestamp.IsZero() || machine.Labels["unbounded-cloud.io/site"] != p.Site {
				return nil, true, fmt.Errorf("media machine identity or site ownership changed")
			}

			if machine.Spec.Host == nil {
				machine.Spec.Host = &v1alpha3.HostSpec{}
			}

			machine.Spec.Host.Netboot = b.Netboot.DeepCopy()
			found = &mediaOwner{machine: &machine, op: op, index: j}
		}
	}

	return found, found != nil, nil
}

// mediaFirstBootPending keeps frozen templates available until a final report
// or abandonment ends first-boot authorization, even after artifact release.
func mediaFirstBootPending(target v1alpha3.MachineOperationTargetStatus) bool {
	b := target.BootMedia
	if b == nil || b.Netboot == nil || len(b.Netboot.DHCPLeases) == 0 || !b.ImageWritten || b.RebootRequestedAt == nil || b.Abandoned {
		return false
	}

	cond := apimeta.FindStatusCondition(target.Conditions, v1alpha3.MachineOperationConditionCloudInitDone)

	return cond == nil || (cond.Status != metav1.ConditionTrue && (cond.Status != metav1.ConditionFalse || cond.Reason != "Failed"))
}

// Retained media claims block live-IP fallback even after the Machine's IP or
// protocol changes. Only firstBootOwner can authorize these requests.
func (p *MediaProvider) rejectFirstBootFallback(ctx context.Context, machine *v1alpha3.Machine, ip string) error {
	var ops v1alpha3.MachineOperationList
	if err := p.Client.List(ctx, &ops); err != nil {
		return err
	}

	for _, op := range ops.Items {
		if op.Spec.OperationKind != v1alpha3.OperationHostReplace {
			continue
		}

		for _, target := range op.Status.Targets {
			b := target.BootMedia
			if !mediaInSite(b, p.Site) {
				continue
			}

			cond := apimeta.FindStatusCondition(target.Conditions, v1alpha3.MachineOperationConditionCloudInitDone)
			reported := cond != nil && (cond.Status == metav1.ConditionTrue || (cond.Status == metav1.ConditionFalse && cond.Reason == "Failed"))

			unconfirmed := !b.Abandoned && !reported && (b.ImageWritten || b.LoaderStartedAt != nil ||
				(!b.Cleaned && (b.Phase == "PoweringOn" || b.Phase == "Installing" || b.Phase == "Installed")))
			if b.Released && !unconfirmed {
				continue
			}

			if machine != nil && target.MachineRef == machine.Name {
				return errMediaFirstBootUnauthorized
			}

			if b.Netboot != nil && len(b.Netboot.DHCPLeases) > 0 && b.Netboot.DHCPLeases[0].IPv4 == ip {
				return errMediaFirstBootUnauthorized
			}
		}
	}

	return nil
}

// Record against the authorized target, not a later operation for this Machine.
func (p *MediaProvider) recordFirstBootCondition(ctx context.Context, o *mediaOwner, ev *cloudInitEvent) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, claimed, err := p.firstBootOwner(ctx, o.status().Netboot.DHCPLeases[0].IPv4)
		if err != nil {
			return err
		}

		if !claimed || current.op.UID != o.op.UID || current.status().ID != o.status().ID {
			return fmt.Errorf("media first-boot ownership changed")
		}

		cond := buildCloudInitCondition(ev, current.op.Generation)
		if cond == nil {
			return nil
		}

		base := current.op.DeepCopy()
		apimeta.SetStatusCondition(&current.op.Status.Targets[current.index].Conditions, *cond)

		return p.Client.Status().Patch(ctx, current.op, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// artifactOwner claims the original lease before a mutable Machine IP lookup.
// A revoked claim stays blocked rather than falling through to PXE serving.
func (p *MediaProvider) artifactOwner(ctx context.Context, ip string) (*mediaOwner, bool, error) {
	var ops v1alpha3.MachineOperationList

	if p.Client == nil {
		return nil, true, fmt.Errorf("media client unavailable")
	}

	if err := p.Client.List(ctx, &ops); err != nil {
		return nil, true, err
	}

	var found *mediaOwner

	claimed := false
	outstanding := false

	for _, op := range ops.Items {
		for _, target := range op.Status.Targets {
			if !mediaInSite(target.BootMedia, p.Site) || target.BootMedia.Netboot == nil || len(target.BootMedia.Netboot.DHCPLeases) == 0 || target.BootMedia.Netboot.DHCPLeases[0].IPv4 != ip {
				continue
			}

			claimed = true

			if mediaArtifactsReleased(&op, target) {
				continue
			}

			outstanding = true

			o, err := p.owner(ctx, target.BootMedia.ID)
			if err == nil && validateMediaLease(o.status().Netboot) == nil {
				if found != nil {
					return nil, true, fmt.Errorf("ambiguous media lease")
				}

				found = o
			}
		}
	}

	if found != nil {
		return found, true, nil
	}

	if claimed && !outstanding && p.nonMediaSuccessor(ctx, ip, ops.Items) {
		return nil, false, nil
	}

	return nil, claimed, fmt.Errorf("no active media artifact authorization")
}

func mediaArtifactsReleased(op *v1alpha3.MachineOperation, target v1alpha3.MachineOperationTargetStatus) bool {
	return target.BootMedia != nil && target.BootMedia.Released && target.BootMedia.Cleaned &&
		(op.Status.IsTerminal() || target.Phase == v1alpha3.OperationPhaseComplete || target.Phase == v1alpha3.OperationPhaseFailed)
}

// Released history yields only to an explicit live PXE/HTTP install. Changing
// the Machine protocol alone must not revive a stale install request.
func (p *MediaProvider) nonMediaSuccessor(ctx context.Context, ip string, ops []v1alpha3.MachineOperation) bool {
	var machines v1alpha3.MachineList
	if err := p.Client.List(ctx, &machines); err != nil {
		return false
	}

	var machine *v1alpha3.Machine

	for i := range machines.Items {
		m := &machines.Items[i]
		if m.Labels["unbounded-cloud.io/site"] != p.Site || !m.DeletionTimestamp.IsZero() || m.Spec.Netboot() == nil {
			continue
		}

		for _, lease := range m.Spec.Netboot().DHCPLeases {
			if lease.IPv4 != ip {
				continue
			}

			if machine != nil && machine.Name != m.Name {
				return false
			}

			machine = m
		}
	}

	if machine == nil {
		return false
	}

	protocol := machine.Spec.Netboot().TargetBootProtocol()
	if protocol != v1alpha3.PXEBootProtocolPXE && protocol != v1alpha3.PXEBootProtocolHTTP {
		return false
	}

	for i := range ops {
		op := &ops[i]
		if op.Spec.OperationKind != v1alpha3.OperationHostReplace || op.Status.IsTerminal() || !op.DeletionTimestamp.IsZero() || !operationRequestsInstall(op, machine.Name) {
			continue
		}

		if len(op.Status.Targets) == 0 {
			return true
		}

		for _, target := range op.Status.Targets {
			if target.MachineRef == machine.Name && target.BootMedia == nil {
				return true
			}
		}
	}

	return false
}

// ServeHTTP serves only operation-scoped control requests. Ordinary artifact
// downloads stay on the HTTP root, where frozen media lease claims take
// precedence over the host source-IP lookup, including revoked claims.
func (p *MediaProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/bootmedia/"), "/")
	if len(parts) != 2 || r.URL.RawQuery != "" || r.URL.RawPath != "" {
		http.NotFound(w, r)
		return
	}

	id, action := parts[0], parts[1]
	log := slog.With("handler", "bootmedia", "action", action, "remote", r.RemoteAddr)

	if (action == "complete" && r.Method != http.MethodPost) || (action != "complete" && r.Method != http.MethodGet) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	o, err := p.callbackOwner(r.Context(), id)
	if err != nil {
		log.Warn("rejecting boot media request without an owner", "err", err)
		http.NotFound(w, r)

		return
	}

	log = log.With("operation", o.op.Name, "machine", o.status().MachineUID)

	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || validateMediaLease(o.status().Netboot) != nil || ip != o.status().Netboot.DHCPLeases[0].IPv4 {
		log.Warn("rejecting boot media request from an unexpected source")
		http.NotFound(w, r)

		return
	}

	switch action {
	case "grub.cfg":
		o, err = p.owner(r.Context(), id)
		if err != nil {
			log.Info("serving halt config because install is no longer authorized", "err", err)
			// Explicit halt prevents an attached ISO from chainloading itself.
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("halt\n")) //nolint:errcheck // Best-effort HTTP response.

			return
		}

		if phase := o.status().Phase; phase != "PoweringOn" && phase != "Installing" {
			// The controller may be power cycling the host for a boot retry.
			log.Info("serving halt config while the host is not expected to boot", "phase", phase)
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("halt\n")) //nolint:errcheck // Best-effort HTTP response.

			return
		}

		data, err := p.installerConfig(o)
		if err != nil {
			log.Error("rendering boot media installer config", "err", err)
			http.Error(w, "media config unavailable", http.StatusServiceUnavailable)

			return
		}

		if o.status().LoaderStartedAt == nil {
			// Record the boot before any installer can run, so the controller
			// never power cycles a host that is installing.
			base := o.op.DeepCopy()
			status := *o.status()
			now := metav1.Now()
			status.LoaderStartedAt = &now

			o.op.Status.Targets[o.index].BootMedia = &status
			if err := p.Client.Status().Patch(r.Context(), o.op, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				log.Warn("saving loader start", "err", err)
				http.Error(w, "cannot save loader start", http.StatusServiceUnavailable)

				return
			}
		}

		log.Info("serving boot media installer config")
		w.Header().Set("Content-Type", "text/plain")
		w.Write(data) //nolint:errcheck // Best-effort HTTP response.
	case "complete":
		if !o.status().ImageWritten {
			base := o.op.DeepCopy()
			status := *o.status()
			status.ImageWritten = true
			o.op.Status.Targets[o.index].BootMedia = &status
			// A resource-version precondition avoids overwriting a concurrent
			// controller cleanup or another target's status. The installer retries.
			if err := p.Client.Status().Patch(r.Context(), o.op, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				log.Warn("saving image-written status", "err", err)
				http.Error(w, "cannot save image-written status", http.StatusServiceUnavailable)

				return
			}

			log.Info("installer reported image written")
		}

		w.WriteHeader(http.StatusOK)
	case "ready":
		if !o.status().Cleaned || !o.status().ImageWritten {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "media cleanup pending", http.StatusServiceUnavailable)

			return
		}

		if o.status().RebootRequestedAt == nil {
			base := o.op.DeepCopy()
			status := *o.status()
			now := metav1.Now()
			status.RebootRequestedAt = &now

			o.op.Status.Targets[o.index].BootMedia = &status
			if err := p.Client.Status().Patch(r.Context(), o.op, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				log.Warn("saving reboot acknowledgment", "err", err)
				http.Error(w, "cannot save reboot acknowledgment", http.StatusServiceUnavailable)

				return
			}

			log.Info("installer allowed to reboot")
		}

		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (p *MediaProvider) installerConfig(o *mediaOwner) ([]byte, error) {
	if p.Resolver == nil || p.Resolver.Cache == nil {
		return nil, fmt.Errorf("media resolver unavailable")
	}

	u, err := mediaServeURL(p.Resolver.ServeURL)
	if err != nil {
		return nil, err
	}

	spec := o.status().Netboot
	if !pinnedImagePattern.MatchString(spec.Image) || !pinnedImagePattern.MatchString(spec.NetbootImage) {
		return nil, fmt.Errorf("media requires digest-pinned images")
	}

	meta, err := p.Resolver.Cache.MetadataForRefArchitecture(spec.NetbootImage, spec.TargetArchitecture())
	if err != nil {
		return nil, err
	}

	if meta.MediaFormat != mediaFormat {
		return nil, fmt.Errorf("incompatible media installer")
	}

	path, tmpl, err := p.Resolver.Cache.ResolvePathForArchitecture(spec.NetbootImage, spec.TargetArchitecture(), "grub/grub.cfg")
	if err != nil {
		return nil, err
	}

	if !tmpl {
		return nil, fmt.Errorf("media installer config must be a template")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	ci := ClusterInfo{}
	if p.Resolver.Cluster != nil {
		ci = p.Resolver.Cluster.ClusterInfo()
	}

	for _, value := range []string{o.machine.Name, o.machine.Namespace, ci.ApiserverURL, spec.TargetDisk} {
		if !mediaTokenPattern.MatchString(value) {
			return nil, fmt.Errorf("unsafe media installer value")
		}
	}

	data := newTemplateData(o.machine, ci, strings.TrimRight(p.Resolver.ServeURL, "/"), "", spec.DHCPLeases[0].IPv4, true)
	data.VirtualMedia = true
	data.ArtifactRoot = "(http," + u.Host + ")"
	data.MediaCallbackURL = strings.TrimRight(p.Resolver.ServeURL, "/") + "/bootmedia/" + o.status().ID

	return renderTemplate(string(content), data)
}
