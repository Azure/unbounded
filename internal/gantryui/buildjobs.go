// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	godigest "github.com/opencontainers/go-digest"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

var buildName = regexp.MustCompile(`^gantry-build-[a-f0-9]{20}$`)

type ImageBuildConfig struct {
	Image          string
	ServiceAccount string
	Base           string
	Repository     string
	MaxConcurrent  int
}

func (cfg ImageBuildConfig) Validate(host string) error {
	if !validBuildBase(host, cfg.Base) || !validBuildBase(host, cfg.Image) {
		return errors.New("builder image and runtime base must be digest-pinned images in the configured ACR")
	}

	if _, err := reference.WithName(host + "/" + cfg.Repository); err != nil {
		return errors.New("invalid generated-image repository")
	}

	if len(validation.IsDNS1123Subdomain(cfg.ServiceAccount)) > 0 || cfg.MaxConcurrent < 1 {
		return errors.New("builder service account and positive concurrency are required")
	}

	return nil
}

type ImageBuild struct {
	Name       string        `json:"name"`
	Request    BuildRequest  `json:"request"`
	Created    time.Time     `json:"created"`
	Progress   BuildProgress `json:"progress"`
	Deployment *Deployment   `json:"deployment,omitempty"`
}

func (s *Server) createBuild(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Builder == nil {
		s.fail(w, http.StatusServiceUnavailable, errors.New("image generation is not configured"))
		return
	}

	if r.Header.Get("Content-Type") != "application/json" {
		s.fail(w, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
		return
	}

	var request BuildRequest

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&request); err != nil {
		s.fail(w, http.StatusBadRequest, errors.New("invalid image generation request"))
		return
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		s.fail(w, http.StatusBadRequest, errors.New("expected one image generation request"))
		return
	}

	if err := request.Validate(); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}

	s.buildMu.Lock()
	defer s.buildMu.Unlock()

	if active, err := s.reclaimActive(r.Context()); err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	} else if active {
		s.fail(w, http.StatusConflict, errors.New("wait for disk reclamation before generating images"))
		return
	}

	jobs, err := s.cfg.Client.BatchV1().Jobs(s.cfg.Namespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: managerLabel + "=gantry-ui," + buildLabel,
	})
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	active := 0

	for _, job := range jobs.Items {
		if !buildFinished(&job) {
			active++
		}
	}

	if active >= s.cfg.Builder.MaxConcurrent {
		s.fail(w, http.StatusConflict, errors.New("an image build is already active; wait for it to finish"))
		return
	}

	var identifier [10]byte
	if _, err := rand.Read(identifier[:]); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}

	name := "gantry-build-" + hex.EncodeToString(identifier[:])
	spec := BuildSpec{
		BuildRequest: request, Name: name, Registry: s.cfg.Registry,
		Repository: s.cfg.Builder.Repository, Base: s.cfg.Builder.Base,
		Namespace: s.cfg.Namespace, NodePool: s.cfg.NodePool, Architecture: s.cfg.Architecture,
	}

	job, err := imageBuildJob(spec, *s.cfg.Builder)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}

	job, err = s.cfg.Client.BatchV1().Jobs(s.cfg.Namespace).Create(r.Context(), job, metav1.CreateOptions{})
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	s.write(w, http.StatusCreated, ImageBuild{
		Name: name, Request: request, Created: job.CreationTimestamp.Time,
		Progress: BuildProgress{Stage: "queued", Layers: request.Layers, Message: "Waiting for the build Job"},
	})
}

func imageBuildJob(spec BuildSpec, cfg ImageBuildConfig) (*batchv1.Job, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}

	labels := map[string]string{managerLabel: "gantry-ui", buildLabel: spec.Name}
	// Only one layer is stored at a time. Include packaging overhead and
	// headroom for container logs as well as the payload itself.
	layerBytes := int64(spec.LayerMB) * payloadMB
	storage := *resource.NewQuantity(layerBytes+layerBytes/100+128*1024*1024, resource.BinarySI)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace, Labels: labels, Annotations: map[string]string{buildAnnotation: string(encoded)}},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(0)), ActiveDeadlineSeconds: ptr.To(int64(ImageBuildTimeout / time.Second)), TTLSecondsAfterFinished: ptr.To(int32(86400)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{managerLabel: "gantry-ui", buildLabel: spec.Name, "azure.workload.identity/use": "true"}},
				Spec: corev1.PodSpec{
					ServiceAccountName: cfg.ServiceAccount, RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:    map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": spec.Architecture, "kubernetes.azure.com/mode": "system"},
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)), FSGroup: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name: "builder", Image: cfg.Image, Command: []string{"/unbounded/bin/gantry-ui", "build-image"},
						Env:             []corev1.EnvVar{{Name: "GANTRY_IMAGE_BUILD", Value: string(encoded)}},
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
						Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"), corev1.ResourceEphemeralStorage: storage}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("512Mi"), corev1.ResourceEphemeralStorage: storage}},
						VolumeMounts:    []corev1.VolumeMount{{Name: "work", MountPath: "/work"}},
					}},
					Volumes: []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &storage}}}},
				},
			},
		},
	}, nil
}

func buildFinished(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) {
			return true
		}
	}

	return false
}

func (s *Server) builds(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.cfg.Client.BatchV1().Jobs(s.cfg.Namespace).List(r.Context(), metav1.ListOptions{LabelSelector: managerLabel + "=gantry-ui," + buildLabel})
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	sort.Slice(jobs.Items, func(i, j int) bool {
		return jobs.Items[i].CreationTimestamp.After(jobs.Items[j].CreationTimestamp.Time)
	})

	if len(jobs.Items) > 20 {
		jobs.Items = jobs.Items[:20]
	}

	result := make([]ImageBuild, 0, len(jobs.Items))
	for _, job := range jobs.Items {
		state, err := s.describeBuild(r.Context(), &job, false)
		if err != nil {
			s.fail(w, http.StatusBadGateway, err)
			return
		}

		result = append(result, state)
	}

	s.write(w, http.StatusOK, result)
}

func (s *Server) build(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !buildName.MatchString(name) {
		s.fail(w, http.StatusBadRequest, errors.New("invalid build name"))
		return
	}

	job, err := s.cfg.Client.BatchV1().Jobs(s.cfg.Namespace).Get(r.Context(), name, metav1.GetOptions{})
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	state, err := s.describeBuild(r.Context(), job, true)
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	s.write(w, http.StatusOK, state)
}

func (s *Server) describeBuild(ctx context.Context, job *batchv1.Job, details bool) (ImageBuild, error) {
	var spec BuildSpec

	if job.Labels[managerLabel] != "gantry-ui" || job.Labels[buildLabel] != job.Name || !buildName.MatchString(job.Name) {
		return ImageBuild{}, errors.New("build Job is not owned by the Gantry UI")
	}

	if err := json.Unmarshal([]byte(job.Annotations[buildAnnotation]), &spec); err != nil {
		return ImageBuild{}, errors.New("build Job metadata is invalid")
	}

	if spec.Name != job.Name || spec.Namespace != s.cfg.Namespace || spec.Registry != s.cfg.Registry {
		return ImageBuild{}, errors.New("build Job does not match this dashboard")
	}

	state := ImageBuild{Name: job.Name, Request: spec.BuildRequest, Created: job.CreationTimestamp.Time, Progress: BuildProgress{Stage: "queued", Layers: spec.Layers, Message: "Waiting for the build Job"}}
	if job.Status.Active > 0 {
		state.Progress.Stage = "building"
		state.Progress.Message = "Generating the image"
	}

	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && condition.Type == batchv1.JobFailed {
			state.Progress.Stage = "failed"
			state.Progress.Message = condition.Reason + ": " + condition.Message
		}

		if condition.Status == corev1.ConditionTrue && condition.Type == batchv1.JobComplete {
			state.Progress.Stage = "complete"
			state.Progress.Message = "Image build completed"
		}
	}

	if !details {
		return state, nil
	}

	workloads, err := s.cfg.Client.AppsV1().DaemonSets(s.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managerLabel + "=gantry-ui," + buildLabel + "=" + job.Name})
	if err != nil {
		return state, err
	}

	if len(workloads.Items) > 1 {
		return state, errors.New("multiple generated workloads claim this build")
	}

	if len(workloads.Items) == 1 {
		workload := workloads.Items[0]

		deployment, err := describeDeployment(&workload)
		if err != nil {
			return state, err
		}

		if deployment.Image.Repository != spec.Repository || deployment.Image.Tag != job.Name || deployment.Image.Streaming != spec.Streaming || !strings.HasPrefix(deployment.Image.Reference, spec.Registry+"/"+spec.Repository+"@") {
			return state, errors.New("generated workload does not match its build")
		}

		state.Deployment = &deployment
		if state.Progress.Stage == "failed" {
			state.Progress.Message += "; the published image workload exists and its pull started"
		} else {
			state.Progress.Stage = "complete"
			state.Progress.Message = "Image published; Flex-worker pull started"
		}

		return state, nil
	}

	pods, err := s.cfg.Client.CoreV1().Pods(s.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job.Name})
	if err != nil {
		return state, err
	}

	for _, pod := range pods.Items {
		owned := false

		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "Job" && owner.UID == job.UID && owner.Controller != nil && *owner.Controller {
				owned = true
			}
		}

		if !owned {
			continue
		}

		readable := false

		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != "builder" {
				continue
			}

			if status.State.Waiting != nil {
				state.Progress.Message = status.State.Waiting.Reason + ": " + status.State.Waiting.Message
			}

			readable = status.State.Running != nil || status.State.Terminated != nil
		}

		if !readable {
			continue
		}

		logs, err := s.cfg.Client.CoreV1().Pods(s.cfg.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "builder", TailLines: ptr.To(int64(100)), LimitBytes: ptr.To(int64(64 << 10))}).Stream(ctx)
		if err != nil {
			return state, err
		}

		scanner := bufio.NewScanner(io.LimitReader(logs, 64<<10))
		for scanner.Scan() {
			progress, ok := parseBuildLog(scanner.Bytes())
			if !ok {
				continue
			}

			if progress.Stage == "failed" || state.Progress.Stage != "failed" {
				state.Progress = progress
			}
		}

		scanErr := scanner.Err()
		if err := errors.Join(scanErr, logs.Close()); err != nil {
			return state, err
		}

		break
	}

	if state.Progress.Stage == "complete" && state.Deployment == nil {
		state.Progress.Stage = "deploying"

		state.Progress.Message = "Waiting for the generated workload"
		if job.Status.CompletionTime != nil && time.Since(job.Status.CompletionTime.Time) > 30*time.Second {
			state.Progress.Stage = "failed"
			state.Progress.Message = "Build completed, but its generated DaemonSet was not found"
		}
	}

	if state.Progress.Stage == "failed" {
		state.Progress.Message += "; inspect logs for Job " + job.Name
	}

	return state, nil
}

func parseBuildLog(line []byte) (BuildProgress, bool) {
	var progress BuildProgress
	if json.Unmarshal(line, &progress) == nil && progress.Stage != "" {
		progress.Message = registryRequestError{err: errors.New(progress.Message)}.Error()
		return progress, true
	}

	// Older builders and startup failures use main's default text logger.
	_, raw, found := strings.Cut(string(line), " ERROR Gantry UI stopped error=")
	if !found {
		return BuildProgress{}, false
	}

	message, err := strconv.Unquote(raw)
	if err != nil {
		return BuildProgress{}, false
	}

	return BuildProgress{Stage: "failed", Message: registryRequestError{err: errors.New(message)}.Error()}, true
}

func validBuildBase(host, base string) bool {
	parts := strings.Split(base, "@")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], host+"/") {
		return false
	}

	_, err := reference.WithName(parts[0])
	d, digestErr := godigest.Parse(parts[1])

	return err == nil && digestErr == nil && d.Algorithm() == godigest.SHA256
}
