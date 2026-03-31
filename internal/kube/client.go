package kube

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func ClientAndConfigFromFile(filePath string) (*kubernetes.Clientset, *rest.Config, error) {
	cliCfg, err := clientcmd.LoadFromFile(filePath)
	if err != nil {
		return nil, nil, fmt.Errorf("load kubernetes client config from file: %w", err)
	}

	restCfg, err := clientcmd.NewDefaultClientConfig(*cliCfg, nil).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("new kubernetes rest client config: %w", err)
	}

	cli, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("new kubernetes client: %w", err)
	}

	return cli, restCfg, nil
}
