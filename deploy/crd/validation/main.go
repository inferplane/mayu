// Command validation checks CRDs with the Kubernetes API server's native schema
// and CEL cost validator. Its dependencies stay outside the main Go module.
package main

import (
	"context"
	"fmt"
	"os"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"sigs.k8s.io/yaml"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: validation <crd.yaml> [<crd.yaml> ...]")
		os.Exit(2)
	}
	failed := false
	for _, path := range os.Args[1:] {
		if err := validate(path); err != nil {
			fmt.Fprintf(os.Stderr, "%s: FAIL: %v\n", path, err)
			failed = true
		} else {
			fmt.Printf("%s: PASS (0 native schema/CEL validation errors)\n", path)
		}
	}
	if failed {
		os.Exit(1)
	}
}

func validate(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	var wire v1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(raw, &wire); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	// The API server defaults the wire object and populates the storage version
	// on creation before running its internal CRD validation.
	v1.SetDefaults_CustomResourceDefinition(&wire)
	wire.Status.StoredVersions = nil
	for _, version := range wire.Spec.Versions {
		if version.Storage {
			wire.Status.StoredVersions = append(wire.Status.StoredVersions, version.Name)
		}
	}
	var internal apiextensions.CustomResourceDefinition
	if err := v1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&wire, &internal, nil); err != nil {
		return fmt.Errorf("convert: %w", err)
	}
	if errs := validation.ValidateCustomResourceDefinition(context.Background(), &internal); len(errs) != 0 {
		return fmt.Errorf("%d validation errors: %w", len(errs), errs.ToAggregate())
	}
	return nil
}
