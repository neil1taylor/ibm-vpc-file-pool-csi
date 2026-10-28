package poolbench

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"
)

// A run applied without "load" or "knee" must still get their defaults, so each
// nested object needs a default of its own.
func TestCRDNestedObjectsDefault(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/storage.ibmcloud.io_poolbenches.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties map[string]struct {
									Default any `json:"default"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties
	for _, name := range []string{"load", "knee", "linux", "windows", "control"} {
		if p, ok := props[name]; !ok || p.Default == nil {
			t.Errorf("spec.%s needs default: {}", name)
		}
	}
}
