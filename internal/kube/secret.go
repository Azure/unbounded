package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

func ApplySecret(ctx context.Context,
	kubeCli kubernetes.Interface,
	s *v1.SecretApplyConfiguration, opts metav1.ApplyOptions,
) error {
	if retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := kubeCli.CoreV1().Secrets(*s.Namespace).Apply(ctx, s, opts)
		return err
	}); retryErr != nil {
		return fmt.Errorf("applying secret %q: %w", fmt.Sprintf("%s/%s", *s.Name, *s.Namespace), retryErr)
	}

	return nil
}
