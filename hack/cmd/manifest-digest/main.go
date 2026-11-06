// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func main() {
	var (
		filePath string
		kind     string
		name     string
		dataKey  string
		field    string
	)

	flag.StringVar(&filePath, "file", "", "YAML manifest file")
	flag.StringVar(&kind, "kind", "", "Kubernetes object kind")
	flag.StringVar(&name, "name", "", "Kubernetes object metadata.name")
	flag.StringVar(&dataKey, "data-key", "", "ConfigMap data key to digest")
	flag.StringVar(&field, "field", "", "Dot-separated object field to digest as canonical JSON")
	flag.Parse()

	if filePath == "" || kind == "" || name == "" {
		exitError(errors.New("file, kind, and name are required"))
	}

	if (dataKey == "") == (field == "") {
		exitError(errors.New("exactly one of data-key or field is required"))
	}

	digest, err := manifestDigest(filePath, kind, name, dataKey, field)
	if err != nil {
		exitError(err)
	}

	fmt.Println(digest)
}

func manifestDigest(filePath, kind, name, dataKey, field string) (digest string, err error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close manifest: %w", closeErr)
		}
	}()

	decoder := utilyaml.NewYAMLOrJSONDecoder(file, 64*1024)

	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return "", fmt.Errorf("decode manifest: %w", err)
		}

		if len(object.Object) == 0 || object.GetKind() != kind || object.GetName() != name {
			continue
		}

		var content []byte

		switch {
		case dataKey != "":
			value, found, nestedErr := unstructured.NestedString(object.Object, "data", dataKey)
			if nestedErr != nil {
				return "", nestedErr
			}

			if !found {
				return "", fmt.Errorf("%s/%s data key %q not found", kind, name, dataKey)
			}

			content = []byte(value)
		case field != "":
			value, found, nestedErr := unstructured.NestedFieldNoCopy(
				object.Object,
				strings.Split(field, ".")...,
			)
			if nestedErr != nil {
				return "", nestedErr
			}

			if !found {
				return "", fmt.Errorf("%s/%s field %q not found", kind, name, field)
			}

			content, err = json.Marshal(value)
			if err != nil {
				return "", fmt.Errorf("encode %s/%s field %q: %w", kind, name, field, err)
			}
		}

		sum := sha256.Sum256(content)

		return fmt.Sprintf("sha256:%x", sum), nil
	}

	return "", fmt.Errorf("%s/%s not found in %s", kind, name, filePath)
}

func exitError(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
