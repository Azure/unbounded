package unboundedcni

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const (
	controllerDeploymentName = "unbounded-cni-controller"
	controllerNamespace      = "unbounded-cni"
	localPort                = 9999
	remotePort               = 9999
	statusEndpoint           = "/status/json"
)

// ControllerStatus represents the JSON response from the controller status endpoint.
type ControllerStatus struct {
	Seq           int64           `json:"seq"`
	Timestamp     string          `json:"timestamp"`
	NodeCount     int             `json:"nodeCount"`
	SiteCount     int             `json:"siteCount"`
	AzureTenantID string          `json:"azureTenantId"`
	LeaderInfo    *LeaderInfo     `json:"leaderInfo"`
	BuildInfo     *BuildInfo      `json:"buildInfo"`
	Problems      []StatusProblem `json:"problems"`
}

// LeaderInfo contains information about the current controller leader.
type LeaderInfo struct {
	PodName  string `json:"podName,omitempty"`
	NodeName string `json:"nodeName,omitempty"`
}

// BuildInfo contains build metadata for the controller.
type BuildInfo struct {
	Version   string `json:"version,omitempty"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"buildTime,omitempty"`
}

type StatusProblem struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Errors []string `json:"errors"`
}

// ValidateConnectivityOptions contains options for the validateConnectivity function.
type ValidateConnectivityOptions struct {
	// Timeout is the maximum time to wait for problems to resolve.
	// If zero, defaults to 10 minutes.
	Timeout time.Duration
	// RetryInterval is the time to wait between retry attempts.
	// If zero, defaults to 10 seconds.
	RetryInterval time.Duration
}

func ValidateConnectivity(ctx context.Context, logger *slog.Logger, kubeCli kubernetes.Interface, restConfig *rest.Config, opts ValidateConnectivityOptions) error {
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}

	if opts.RetryInterval == 0 {
		opts.RetryInterval = 10 * time.Second
	}

	podName, err := findControllerPod(ctx, kubeCli)
	if err != nil {
		return fmt.Errorf("failed to find controller pod: %w", err)
	}

	// Set up port-forward
	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{})
	errChan := make(chan error, 1)

	// Start port-forward in a goroutine
	go func() {
		err := portForwardPod(ctx, restConfig, controllerNamespace, podName, localPort, remotePort, stopChan, readyChan)
		if err != nil {
			errChan <- err
		}
	}()

	// Wait for port-forward to be ready or error/timeout
	select {
	case <-readyChan:
		// Port-forward is ready
	case err := <-errChan:
		return fmt.Errorf("port-forward failed: %w", err)
	case <-ctx.Done():
		close(stopChan)
		return ctx.Err()
	case <-time.After(30 * time.Second):
		close(stopChan)
		return fmt.Errorf("timeout waiting for port-forward to be ready")
	}

	defer close(stopChan)

	timeoutCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// Retry loop for checking status and problems
	ticker := time.NewTicker(opts.RetryInterval)
	defer ticker.Stop()

	for {
		status, err := fetchControllerStatus(ctx)
		if err != nil {
			return fmt.Errorf("failed to fetch controller status: %w", err)
		}

		// Check if there are no problems
		if len(status.Problems) == 0 {
			logger.Info("Connectivity validated successfully",
				"nodeCount", status.NodeCount,
				"siteCount", status.SiteCount,
				"problemCount", len(status.Problems),
			)

			return nil
		}

		// Log the problems
		logger.Warn("Controller reported problems, will retry",
			"problemCount", len(status.Problems),
			"retryInterval", opts.RetryInterval,
		)

		for _, problem := range status.Problems {
			logger.Warn("Connectivity problem", "type", problem.Type, "name", problem.Name, "errors", problem.Errors)
		}

		// Wait for retry interval or context cancellation
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for problems to resolve: %d problems remain", len(status.Problems))
		case <-ticker.C:
			// Continue to next iteration
		}
	}
}

// findControllerPod finds a running pod from the unbounded-cni-controller deployment.
func findControllerPod(ctx context.Context, kubeCli kubernetes.Interface) (string, error) {
	// List pods with the deployment's label selector
	labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s", controllerDeploymentName)

	pods, err := kubeCli.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found for deployment %s", controllerDeploymentName)
	}

	// Find a running pod
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			return pod.Name, nil
		}
	}

	return "", fmt.Errorf("no running pods found for deployment %s", controllerDeploymentName)
}

// portForwardPod establishes a port-forward connection to the specified pod.
func portForwardPod(ctx context.Context, restConfig *rest.Config, namespace, podName string, localPort, remotePort int, stopChan <-chan struct{}, readyChan chan struct{}) error {
	// Build the port-forward URL
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", namespace, podName)

	hostURL, err := url.Parse(restConfig.Host)
	if err != nil {
		return fmt.Errorf("failed to parse host URL: %w", err)
	}

	hostURL.Path = path

	// Create SPDY transport
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create round tripper: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, hostURL)

	// Set up ports
	ports := []string{fmt.Sprintf("%d:%d", localPort, remotePort)}

	// Create port-forwarder
	pf, err := portforward.New(dialer, ports, stopChan, readyChan, io.Discard, io.Discard)
	if err != nil {
		return fmt.Errorf("failed to create port forwarder: %w", err)
	}

	// Run port-forward (blocks until stopChan is closed or error)
	return pf.ForwardPorts()
}

// fetchControllerStatus makes an HTTP request to the controller status endpoint.
func fetchControllerStatus(ctx context.Context) (*ControllerStatus, error) {
	statusURL := fmt.Sprintf("http://localhost:%d%s", localPort, statusEndpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close response body: %v\n", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("unexpected status code %d (failed to read body: %v)", resp.StatusCode, err)
		}

		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var status ControllerStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &status, nil
}
