package webhook

import "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

// restorePointWarnings turns a deprecation note into an admission warning, which
// kubectl apply shows. An empty note gives none.
func restorePointWarnings(warning string) admission.Warnings {
	if warning == "" {
		return nil
	}
	return admission.Warnings{warning}
}
