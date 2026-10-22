// Package consoleplugin turns the driver's OpenShift console plugin on and off
// in the console operator configuration. The Helm chart runs it as hook Jobs.
package consoleplugin

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// consoleName is the cluster-wide console operator configuration.
const consoleName = "cluster"

var consoleGVK = schema.GroupVersionKind{Group: "operator.openshift.io", Version: "v1", Kind: "Console"}

// Register adds plugin to the console's enabled plugins, keeping every other
// plugin. It does nothing if plugin is already there.
func Register(ctx context.Context, c client.Client, plugin string) error {
	return edit(ctx, c, func(plugins []string) ([]string, bool) {
		for _, p := range plugins {
			if p == plugin {
				return plugins, false
			}
		}
		return append(plugins, plugin), true
	}, false)
}

// Unregister removes plugin from the console's enabled plugins and keeps every
// other plugin. A cluster without a console configuration is not an error.
func Unregister(ctx context.Context, c client.Client, plugin string) error {
	return edit(ctx, c, func(plugins []string) ([]string, bool) {
		kept := make([]string, 0, len(plugins))
		for _, p := range plugins {
			if p != plugin {
				kept = append(kept, p)
			}
		}
		return kept, len(kept) != len(plugins)
	}, true)
}

// edit applies change to the plugin list. The update carries the read
// resourceVersion, so a concurrent edit by someone else is retried, not lost.
func edit(ctx context.Context, c client.Client, change func([]string) ([]string, bool), missingOK bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(consoleGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: consoleName}, u); err != nil {
			if missingOK && (apierrors.IsNotFound(err) || meta.IsNoMatchError(err)) {
				return nil
			}
			return fmt.Errorf("read console operator configuration: %w", err)
		}
		plugins, _, err := unstructured.NestedStringSlice(u.Object, "spec", "plugins")
		if err != nil {
			return fmt.Errorf("read console plugins: %w", err)
		}
		plugins, changed := change(plugins)
		if !changed {
			return nil
		}
		if len(plugins) == 0 {
			unstructured.RemoveNestedField(u.Object, "spec", "plugins")
		} else if err := unstructured.SetNestedStringSlice(u.Object, plugins, "spec", "plugins"); err != nil {
			return err
		}
		return c.Update(ctx, u)
	})
}
