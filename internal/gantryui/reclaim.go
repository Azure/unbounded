// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

const (
	reclaimJobName    = "gantry-reclaim"
	reclaimLabel      = "gantry.unbounded-cloud.io/reclaim"
	reclaimAnnotation = "gantry.unbounded-cloud.io/reclaim-spec"
	ReclaimTimeout    = 45 * time.Minute
)

type ReclaimConfig struct {
	Image           string
	ServiceAccount  string
	ImagePullSecret string
}

func (cfg ReclaimConfig) Validate(registry string) error {
	if !validHelperImage(cfg.Image) || len(validation.IsDNS1123Subdomain(cfg.ServiceAccount)) != 0 {
		return errors.New("reclaimer requires a digest-pinned image and a service account")
	}

	if cfg.ImagePullSecret != "" && len(validation.IsDNS1123Subdomain(cfg.ImagePullSecret)) != 0 {
		return errors.New("invalid helper image pull secret name")
	}

	return nil
}

type ReclaimSpec struct {
	Namespace       string `json:"namespace"`
	NodePool        string `json:"nodePool"`
	Architecture    string `json:"architecture"`
	Image           string `json:"image"`
	ServiceAccount  string `json:"serviceAccount"`
	ImagePullSecret string `json:"imagePullSecret,omitempty"`
}

type ReclaimProgress struct {
	Stage           string `json:"stage"`
	Message         string `json:"message"`
	Total           int    `json:"total"`
	Completed       int    `json:"completed"`
	Failed          int    `json:"failed"`
	Deleted         int    `json:"deleted"`
	Reclaimed       uint64 `json:"reclaimed"`
	ProtectedLeases int    `json:"protectedLeases"`
}

type ReclaimState struct {
	Name     string               `json:"name"`
	UID      string               `json:"uid"`
	Progress ReclaimProgress      `json:"progress"`
	Workers  []ReclaimWorkerState `json:"workers"`
}

type ReclaimWorkerState struct {
	Node  string `json:"node"`
	Stage string `json:"stage"`
}

func (spec ReclaimSpec) Validate() error {
	for _, value := range []string{spec.Namespace, spec.NodePool, spec.ServiceAccount} {
		if len(validation.IsDNS1123Label(value)) != 0 {
			return errors.New("invalid reclaimer namespace, pool, or service account")
		}
	}

	if spec.Architecture != "amd64" && spec.Architecture != "arm64" {
		return errors.New("unsupported reclaimer architecture")
	}

	if spec.ImagePullSecret != "" && len(validation.IsDNS1123Subdomain(spec.ImagePullSecret)) != 0 {
		return errors.New("invalid helper image pull secret name")
	}
	// Unlike the browser, this command is also invoked directly by Kubernetes.
	if !validBuildBase(registryHost(spec.Image), spec.Image) {
		return errors.New("reclaimer image must be digest pinned")
	}

	return nil
}

func registryHost(image string) string {
	for i, character := range image {
		if character == '/' {
			return image[:i]
		}
	}

	return ""
}

func ownedReclaim(job *batchv1.Job) (ReclaimSpec, error) {
	var spec ReclaimSpec
	if job.Name != reclaimJobName || job.Labels[managerLabel] != "gantry-ui" || job.Labels[reclaimLabel] != "coordinator" {
		return spec, errors.New("reclaim Job is not owned by this dashboard")
	}

	if err := json.Unmarshal([]byte(job.Annotations[reclaimAnnotation]), &spec); err != nil {
		return spec, err
	}

	if err := spec.Validate(); err != nil {
		return spec, err
	}

	containers := job.Spec.Template.Spec.Containers
	if spec.Namespace != job.Namespace || len(containers) != 1 || containers[0].Name != "reclaimer" ||
		containers[0].Image != spec.Image || job.Spec.Template.Spec.ServiceAccountName != spec.ServiceAccount {
		return spec, errors.New("reclaim Job does not match its saved configuration")
	}

	return spec, nil
}

func (s *Server) reclaimActive(ctx context.Context) (bool, error) {
	if s.cfg.Reclaimer == nil {
		return false, nil
	}

	job, err := s.cfg.Client.BatchV1().Jobs(s.cfg.Namespace).Get(ctx, reclaimJobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	if _, err := ownedReclaim(job); err != nil {
		return false, err
	}

	if !buildFinished(job) {
		return true, nil
	}

	workers, err := s.cfg.Client.BatchV1().Jobs(s.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: reclaimLabel + "=worker"})
	if err != nil {
		return false, err
	}

	for _, worker := range workers.Items {
		for _, owner := range worker.OwnerReferences {
			if owner.Kind == "Job" && owner.UID == job.UID && !buildFinished(&worker) {
				return true, nil
			}
		}
	}

	return false, nil
}

func (s *Server) createReclaim(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Reclaimer == nil {
		s.fail(w, http.StatusServiceUnavailable, errors.New("disk reclamation is not configured"))
		return
	}

	if r.Header.Get("Content-Type") != "application/json" {
		s.fail(w, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
		return
	}

	var request struct {
		Confirm string `json:"confirm"`
	}

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&request); err != nil {
		s.fail(w, http.StatusBadRequest, errors.New("invalid reclaim confirmation"))
		return
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) || request.Confirm != "DELETE ALL DEMOS" {
		s.fail(w, http.StatusBadRequest, errors.New("confirm deletion of all dashboard demos with DELETE ALL DEMOS"))
		return
	}

	s.buildMu.Lock()
	defer s.buildMu.Unlock()

	if active, err := s.reclaimActive(r.Context()); err != nil {
		s.fail(w, http.StatusConflict, err)
		return
	} else if active {
		s.fail(w, http.StatusConflict, errors.New("disk reclamation is already in progress"))
		return
	}

	jobs, err := s.cfg.Client.BatchV1().Jobs(s.cfg.Namespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: managerLabel + "=gantry-ui," + buildLabel,
	})
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	for _, job := range jobs.Items {
		if !buildFinished(&job) {
			s.fail(w, http.StatusConflict, errors.New("wait for the active image build before reclaiming disk space"))
			return
		}
	}

	job, err := s.cfg.Client.BatchV1().Jobs(s.cfg.Namespace).Get(r.Context(), reclaimJobName, metav1.GetOptions{})
	if err == nil {
		if _, err := ownedReclaim(job); err != nil {
			s.fail(w, http.StatusConflict, err)
			return
		}

		if !buildFinished(job) {
			s.fail(w, http.StatusConflict, errors.New("disk reclamation is already in progress"))
			return
		}

		if job.DeletionTimestamp == nil {
			err = s.cfg.Client.BatchV1().Jobs(s.cfg.Namespace).Delete(r.Context(), job.Name, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &job.UID}, PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
			})
			if err != nil && !apierrors.IsNotFound(err) {
				s.fail(w, http.StatusBadGateway, err)
				return
			}
		}

		w.Header().Set("Retry-After", "2")
		s.fail(w, http.StatusConflict, errors.New("previous reclaim is being removed; retry shortly"))

		return
	} else if !apierrors.IsNotFound(err) {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	spec := ReclaimSpec{
		Namespace: s.cfg.Namespace, NodePool: s.cfg.NodePool, Architecture: s.cfg.Architecture,
		Image: s.cfg.Reclaimer.Image, ServiceAccount: s.cfg.Reclaimer.ServiceAccount, ImagePullSecret: s.cfg.Reclaimer.ImagePullSecret,
	}

	desired, err := reclaimCoordinatorJob(spec)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}

	job, err = s.cfg.Client.BatchV1().Jobs(s.cfg.Namespace).Create(r.Context(), desired, metav1.CreateOptions{})
	if err != nil {
		status := http.StatusBadGateway
		if apierrors.IsAlreadyExists(err) {
			status = http.StatusConflict
		}

		s.fail(w, status, err)

		return
	}

	s.write(w, http.StatusCreated, ReclaimState{
		Name: job.Name, UID: string(job.UID),
		Progress: ReclaimProgress{Stage: "queued", Message: "Waiting for the disk reclamation Job"},
	})
}

func reclaimCoordinatorJob(spec ReclaimSpec) (*batchv1.Job, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: reclaimJobName, Namespace: spec.Namespace,
			Labels:      map[string]string{managerLabel: "gantry-ui", reclaimLabel: "coordinator"},
			Annotations: map[string]string{reclaimAnnotation: string(raw)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(0)), ActiveDeadlineSeconds: ptr.To(int64(ReclaimTimeout / time.Second)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{reclaimLabel: "coordinator"}},
				Spec: corev1.PodSpec{
					ServiceAccountName: spec.ServiceAccount, RestartPolicy: corev1.RestartPolicyNever,
					ImagePullSecrets: helperPullSecrets(spec.ImagePullSecret),
					NodeSelector:     map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": spec.Architecture, "kubernetes.azure.com/mode": "system"},
					SecurityContext:  &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532))},
					Containers: []corev1.Container{{
						Name: "reclaimer", Image: spec.Image,
						Command:         []string{"/unbounded/bin/gantry-ui", "reclaim"},
						Env:             []corev1.EnvVar{{Name: "GANTRY_RECLAIM", Value: string(raw)}},
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
						Resources:       reclaimResources(),
					}},
				},
			},
		},
	}, nil
}

func (s *Server) reclaimStatus(w http.ResponseWriter, r *http.Request) {
	job, err := s.cfg.Client.BatchV1().Jobs(s.cfg.Namespace).Get(r.Context(), reclaimJobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		s.write(w, http.StatusOK, nil)
		return
	}

	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	if _, err := ownedReclaim(job); err != nil {
		s.fail(w, http.StatusConflict, err)
		return
	}

	state := ReclaimState{Name: job.Name, UID: string(job.UID), Progress: ReclaimProgress{Stage: "queued", Message: "Waiting for the disk reclamation Job"}}

	pods, err := s.cfg.Client.CoreV1().Pods(job.Namespace).List(r.Context(), metav1.ListOptions{LabelSelector: "job-name=" + job.Name})
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	for _, pod := range pods.Items {
		if !jobOwnsPod(job, &pod) {
			continue
		}

		for _, container := range pod.Status.ContainerStatuses {
			if container.Name != "reclaimer" {
				continue
			}

			if container.State.Waiting != nil {
				state.Progress.Message = container.State.Waiting.Reason + ": " + container.State.Waiting.Message
				continue
			}

			raw, err := s.cfg.Client.CoreV1().Pods(job.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "reclaimer", TailLines: ptr.To(int64(10)), LimitBytes: ptr.To(int64(16384))}).DoRaw(r.Context())
			if err != nil {
				s.fail(w, http.StatusBadGateway, err)
				return
			}

			for _, line := range strings.Split(string(raw), "\n") {
				var progress ReclaimProgress
				if json.Unmarshal([]byte(line), &progress) == nil && progress.Stage != "" {
					state.Progress = progress
				}
			}
		}
	}

	if buildFinished(job) {
		succeeded := false

		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
				succeeded = true
			}
		}

		if !succeeded {
			if state.Progress.Stage != "failed" {
				state.Progress.Message = "Disk reclamation Job failed; inspect its logs"
			}

			state.Progress.Stage = "failed"
		}

		if succeeded && state.Progress.Stage != "complete" {
			state.Progress.Stage = "failed"
			state.Progress.Message = "Reclaim Job ended without a final completion report"
		}
	}

	workers, err := s.cfg.Client.BatchV1().Jobs(job.Namespace).List(r.Context(), metav1.ListOptions{LabelSelector: reclaimLabel + "=worker"})
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	for _, worker := range workers.Items {
		owned := false

		for _, owner := range worker.OwnerReferences {
			if owner.Kind == "Job" && owner.UID == job.UID && owner.Controller != nil && *owner.Controller {
				owned = true
			}
		}

		if !owned {
			continue
		}

		stage := "reclaiming"

		for _, condition := range worker.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}

			if condition.Type == batchv1.JobFailed {
				stage = "failed"
			}

			if condition.Type == batchv1.JobComplete {
				stage = "complete"
			}
		}

		state.Workers = append(state.Workers, ReclaimWorkerState{Node: worker.Spec.Template.Spec.NodeName, Stage: stage})
	}

	s.write(w, http.StatusOK, state)
}

func jobOwnsPod(job *batchv1.Job, pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" && owner.Name == job.Name && owner.UID == job.UID && owner.Controller != nil && *owner.Controller {
			return true
		}
	}

	return false
}
