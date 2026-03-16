package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

func ApplySecret(ctx context.Context, kubeCli kubernetes.Interface, s *v1.SecretApplyConfiguration) error {
	applyOpts := metav1.ApplyOptions{
		FieldManager: "forge",
	}

	if retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := kubeCli.CoreV1().Secrets(*s.Namespace).Apply(ctx, s, applyOpts)
		return err
	}); retryErr != nil {
		return fmt.Errorf("applying secret %q in namespace %q: %w", *s.Name, *s.Namespace, retryErr)
	}

	return nil
}
