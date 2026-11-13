/*
Portions Copyright (c) Microsoft Corporation.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package status

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/flex"
)

type FlexReconciler struct {
	provider func() (*flex.Provider, error)
}

func NewFlexReconciler(provider func() (*flex.Provider, error)) *FlexReconciler {
	return &FlexReconciler{provider: provider}
}

func (r *FlexReconciler) Reconcile(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) (reconcile.Result, error) {
	if nodeClass.Spec.Flex == nil {
		return reconcile.Result{}, nil
	}
	if err := flex.ValidateConfiguration(ctx, nodeClass); err != nil {
		return r.fail(nodeClass, err)
	}
	if r.provider == nil {
		return r.fail(nodeClass, fmt.Errorf("flex provider is not configured"))
	}
	provider, err := r.provider()
	if err != nil {
		return r.fail(nodeClass, err)
	}
	if provider == nil {
		return r.fail(nodeClass, fmt.Errorf("flex provider is not configured"))
	}
	image, err := provider.Resolve(ctx, nodeClass)
	if err != nil {
		return r.fail(nodeClass, err)
	}
	nodeClass.Status.Images = []v1beta1.NodeImage{image}
	nodeClass.StatusConditions().SetTrue(v1beta1.ConditionTypeImagesReady)
	nodeClass.StatusConditions().SetTrue(v1beta1.ConditionTypeFlexReady)
	return reconcile.Result{RequeueAfter: healthyRequeueInterval}, nil
}

func (r *FlexReconciler) fail(nodeClass *v1beta1.AKSNodeClass, err error) (reconcile.Result, error) {
	reason := "FlexPrerequisitesUnavailable"
	var configError *flex.ConfigurationError
	isConfigurationError := errors.As(err, &configError)
	if isConfigurationError {
		reason = configError.Reason
	}
	nodeClass.StatusConditions().SetFalse(v1beta1.ConditionTypeFlexReady, reason, err.Error())
	nodeClass.StatusConditions().SetFalse(v1beta1.ConditionTypeImagesReady, reason, err.Error())
	if isConfigurationError {
		return reconcile.Result{RequeueAfter: time.Minute}, nil
	}
	return reconcile.Result{}, err
}
