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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

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
			if target.BootMedia == nil || target.BootMedia.Netboot == nil || len(target.BootMedia.Netboot.DHCPLeases) == 0 || target.BootMedia.Netboot.DHCPLeases[0].IPv4 != ip {
				continue
			}

			claimed = true

			if target.BootMedia.Released && target.BootMedia.Cleaned && (op.Status.IsTerminal() || target.Phase == v1alpha3.OperationPhaseComplete || target.Phase == v1alpha3.OperationPhaseFailed) {
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
		if !m.DeletionTimestamp.IsZero() || m.Spec.Netboot() == nil {
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
// downloads stay on the existing HTTP root and use the host source-IP lookup.
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

		data, err := p.installerConfig(o)
		if err != nil {
			log.Error("rendering boot media installer config", "err", err)
			http.Error(w, "media config unavailable", http.StatusServiceUnavailable)

			return
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
