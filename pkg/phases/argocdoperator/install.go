package argocdoperator

import (
	"github.com/flanksource/karina/pkg/platform"
)

const (
	Namespace = "argocd"
)

func Deploy(platform *platform.Platform) error {
	if platform.ArgocdOperator.IsDisabled() {
		return platform.DeleteSpecs("", "argocd-operator.yaml")
	}

	if err := platform.CreateOrUpdateNamespace(Namespace, nil, nil); err != nil {
		return err
	}

	return platform.ApplySpecs(Namespace, "argocd-operator.yaml")
}
