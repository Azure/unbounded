package app

import "os"

func getKubeconfigPath(p string) string {
	if !isEmpty(p) {
		return p
	}

	return os.Getenv("KUBECONFIG")
}
