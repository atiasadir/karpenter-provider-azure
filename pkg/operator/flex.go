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

package operator

import (
	"context"
	"fmt"
	"strings"

	"github.com/patrickmn/go-cache"

	"github.com/Azure/karpenter-provider-azure/pkg/auth"
	azurecache "github.com/Azure/karpenter-provider-azure/pkg/cache"
	"github.com/Azure/karpenter-provider-azure/pkg/cloudprovider"
	instancetypecontroller "github.com/Azure/karpenter-provider-azure/pkg/controllers/instancetype"
	quotacontroller "github.com/Azure/karpenter-provider-azure/pkg/controllers/quota"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/allocationstrategy"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/capacityrecommendation"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instancetype"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/launchtemplate"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/networksecuritygroup"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/pricing"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/quota"
)

func (o *Operator) configureFlexLocations(ctx context.Context, cfg *auth.Config, env *auth.Environment, nsg *networksecuritygroup.Provider) error {
	o.FlexLocations = map[string]cloudprovider.RegionalProviders{}
	for _, location := range strings.Split(options.FromContext(ctx).FlexLocations, ",") {
		if location == "" || location == cfg.Location {
			continue
		}
		if _, exists := o.FlexLocations[location]; exists {
			continue
		}
		if err := o.addFlexLocation(ctx, cfg, env, nsg, location); err != nil {
			return fmt.Errorf("configuring Flex location %s: %w", location, err)
		}
	}
	return nil
}

func (o *Operator) addFlexLocation(ctx context.Context, cfg *auth.Config, env *auth.Environment, nsg *networksecuritygroup.Provider, location string) error {
	offerings := azurecache.NewUnavailableOfferings()
	prices := pricing.NewProvider(ctx, env, pricing.NewAPI(env.Cloud), location, o.Elected())
	quotas := quota.NewProvider(o.AZClient.UsageClient, o.AZClient.QuotaCategoryVMFamilyMappingClient, location, o.Clock)
	instanceTypes := instancetype.NewDefaultProvider(location,
		cache.New(instancetype.InstanceTypesCacheTTL, azurecache.DefaultCleanupInterval),
		o.AZClient.SKUClient, prices, offerings, quotas)
	if err := instanceTypes.UpdateInstanceTypes(ctx); err != nil {
		return err
	}
	if err := quotas.Update(ctx); err != nil {
		return err
	}
	images := imagefamily.NewProvider(o.AZClient.ImageVersionsClient, location, cfg.SubscriptionID,
		o.AZClient.NodeImageVersionsClient,
		cache.New(imagefamily.ImageExpirationInterval, imagefamily.ImageCacheCleaningInterval))
	resolver := imagefamily.NewDefaultResolver(o.GetClient(), images, instanceTypes, o.AZClient.NodeBootstrappingClient)
	caBundle, err := getCABundle(o.GetConfig())
	if err != nil {
		return err
	}
	opts := options.FromContext(ctx)
	templates := launchtemplate.NewProvider(ctx, resolver, images, nsg, caBundle,
		opts.ClusterEndpoint, cfg.TenantID, cfg.SubscriptionID, cfg.ResourceGroup,
		opts.KubeletIdentityClientID, opts.NodeResourceGroup, location, opts.ProvisionMode)
	recommendations := capacityrecommendation.NewProvider(o.AZClient.SKUMixPlacementClient,
		cache.New(cache.NoExpiration, azurecache.DefaultCleanupInterval), location)
	allocation := allocationstrategy.NewProvider(recommendations, opts.ComputeRecommendationMode)
	instances := instance.NewDefaultVMProvider(o.AZClient, instanceTypes, allocation, templates,
		o.LoadBalancerProvider, nsg, offerings, location, opts.NodeResourceGroup,
		cfg.SubscriptionID, opts.ProvisionMode, opts.DiskEncryptionSetID, env, quotas)
	o.FlexLocations[location] = cloudprovider.RegionalProviders{InstanceTypes: instanceTypes, Instances: instances}
	o.FlexControllers = append(o.FlexControllers,
		instancetypecontroller.NewController(instanceTypes).WithName("instancetype.flex."+location),
		quotacontroller.NewController(quotas, o.Clock).WithName("quota.flex."+location),
	)
	return nil
}
