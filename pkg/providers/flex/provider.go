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

package flex

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	sdkerrors "github.com/Azure/azure-sdk-for-go-extensions/pkg/errors"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/blang/semver/v4"
	"github.com/google/uuid"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/azclient/azapi"
	"github.com/Azure/karpenter-provider-azure/pkg/utils"
)

const (
	HostImagePublisher   = "Canonical"
	HostImageOffer       = "ubuntu-24_04-lts"
	HostImageSKU         = "server"
	PoolTag              = "karpenter.azure.com_flex_pool"
	PoolAnnotation       = "karpenter.azure.com/flex-pool-id"
	SpecHashTag          = "karpenter.azure.com_flex_nodeclass_hash"
	SpecHashAnnotation   = "karpenter.azure.com/flex-nodeclass-hash"
	ProviderIDAnnotation = "karpenter.azure.com/flex-provider-id"
	LocationAnnotation   = "karpenter.azure.com/flex-location"
	TerminationFinalizer = "karpenter.azure.com/flex-termination"
)

var imageVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

type VirtualNetworksAPI interface {
	Get(context.Context, string, string, *armnetwork.VirtualNetworksClientGetOptions) (armnetwork.VirtualNetworksClientGetResponse, error)
}

type ImagesAPI interface {
	Get(context.Context, string, string, string, string, string, *armcompute.VirtualMachineImagesClientGetOptions) (armcompute.VirtualMachineImagesClientGetResponse, error)
}

type IdentitiesAPI interface {
	Get(context.Context, string, string, *armmsi.UserAssignedIdentitiesClientGetOptions) (armmsi.UserAssignedIdentitiesClientGetResponse, error)
}

type Provider struct {
	subscriptionID string
	clusterID      string
	location       string
	networks       VirtualNetworksAPI
	images         ImagesAPI
	identities     IdentitiesAPI
	machines       azapi.AKSMachinesAPI
	pools          azapi.AKSAgentPoolsAPI
}

func NewProvider(subscriptionID, clusterResourceGroup, clusterName, location string,
	networks VirtualNetworksAPI, images ImagesAPI, identities IdentitiesAPI,
	machines azapi.AKSMachinesAPI, pools azapi.AKSAgentPoolsAPI,
) *Provider {
	return &Provider{
		subscriptionID: subscriptionID,
		clusterID: fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ContainerService/managedClusters/%s",
			subscriptionID, clusterResourceGroup, clusterName),
		location: location, networks: networks, images: images, identities: identities,
		machines: machines, pools: pools,
	}
}

type ConfigurationError struct {
	Reason  string
	Message string
}

func (e *ConfigurationError) Error() string { return e.Message }

func invalid(reason, message string) error {
	return &ConfigurationError{Reason: reason, Message: message}
}

// ValidateConfiguration is also checked on the create path, so stale NodeClass
// readiness cannot bypass the opt-in or select an unsupported provisioning mode.
func ValidateConfiguration(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) error {
	if nodeClass.Spec.Flex == nil {
		return nil
	}
	opts := options.FromContext(ctx)
	if !opts.EnableFlexNodes {
		return invalid("FlexNodesDisabled", "Flex workers require enable-flex-nodes=true")
	}
	if opts.ProvisionMode != consts.ProvisionModeAKSScriptless || opts.UseSIG {
		return invalid("FlexUnsupportedProvisionMode", "Flex workers require self-hosted aksscriptless provisioning; AKS Machine API and bootstrappingclient modes are not supported")
	}
	if opts.NetworkPlugin != consts.NetworkPluginNone {
		return invalid("FlexRequiresNoCNICluster", "Flex node pools require the AKS cluster networkPlugin=none; managed Azure CNI Overlay/Cilium clusters are not supported")
	}
	if location := lo.FromPtr(nodeClass.Spec.Flex.Location); location != "" && !lo.Contains(strings.Split(opts.FlexLocations, ","), location) {
		return invalid("FlexLocationNotEnabled", "Flex location must be explicitly enabled by the controller's flex-locations setting")
	}
	if err := validateHostConfiguration(ctx, nodeClass); err != nil {
		return err
	}
	return validateWorkerConfiguration(nodeClass)
}

func validateHostConfiguration(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) error {
	opts := options.FromContext(ctx)
	if lo.FromPtr(nodeClass.Spec.ImageFamily) != v1beta1.Ubuntu2404ImageFamily ||
		nodeClass.IsFIPSEnabled() || opts.EnableFIPS || opts.ShouldUseNodeHardening() {
		return invalid("FlexUnsupportedImageConfiguration", "Flex workers require Ubuntu2404 without FIPS or node hardening")
	}
	if lo.FromPtr(nodeClass.Spec.VNETSubnetID) == "" {
		return invalid("FlexSubnetRequired", "Flex workers require an explicit precreated vnetSubnetID")
	}
	if nodeClass.Spec.Versions != nil && lo.FromPtr(nodeClass.Spec.Versions.NodeImageVersion) != "" {
		return invalid("FlexUnsupportedImagePin", "Flex workers pin the host image with flex.hostImageVersion, not versions.nodeImageVersion")
	}
	if !imageVersionPattern.MatchString(nodeClass.Spec.Flex.HostImageVersion) {
		return invalid("FlexInvalidHostImageVersion", "Flex hostImageVersion must pin a numeric marketplace image version")
	}
	if err := uuid.Validate(nodeClass.Spec.Flex.IdentityClientID); err != nil {
		return invalid("FlexInvalidIdentity", "Flex identityClientID must be a UUID")
	}
	return nil
}

func validateWorkerConfiguration(nodeClass *v1beta1.AKSNodeClass) error {
	if nodeClass.Spec.LinuxOSConfig != nil || nodeClass.Spec.LocalDNS != nil ||
		nodeClass.IsKataEnabled() || nodeClass.IsArtifactStreamingExplicitlyEnabled() ||
		nodeClass.Spec.GPU != nil || nodeClass.IsNodePublicIPEnabled() || nodeClass.Spec.CapacityReservation != nil {
		return invalid("FlexUnsupportedConfiguration", "Flex workers do not support linuxOSConfig, localDNS, Kata, artifact streaming, GPU configuration, node public IPs, or capacity reservations")
	}
	if nodeClass.Spec.Kubelet != nil && !equality.Semantic.DeepEqual(nodeClass.Spec.Kubelet, &v1beta1.KubeletConfiguration{}) {
		return invalid("FlexUnsupportedKubeletConfiguration", "Flex workers do not support kubelet overrides; configure pod density with spec.maxPods")
	}
	return nil
}

func (p *Provider) AgentPoolID(poolName string) string {
	return p.clusterID + "/agentPools/" + poolName
}

func (p *Provider) ValidatePoolID(id string) error {
	_, err := p.parsePoolID(id)
	return err
}

func (p *Provider) parsePoolID(id string) (*arm.ResourceID, error) {
	pool, err := arm.ParseResourceID(id)
	if err != nil || pool.Parent == nil ||
		!strings.EqualFold(pool.ResourceType.String(), "Microsoft.ContainerService/managedClusters/agentPools") ||
		!strings.EqualFold(pool.Parent.String(), p.clusterID) {
		return nil, invalid("FlexInvalidPool", "Flex agentPoolID must identify a pool in this controller's AKS cluster")
	}
	return pool, nil
}

// Resolve validates externally owned prerequisites without creating or changing them.
func (p *Provider) Resolve(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) (v1beta1.NodeImage, error) {
	if err := ValidateConfiguration(ctx, nodeClass); err != nil {
		return v1beta1.NodeImage{}, err
	}
	if nodeClass.Spec.Flex == nil {
		return v1beta1.NodeImage{}, fmt.Errorf("resolving Flex prerequisites without a Flex configuration")
	}
	if p.networks == nil || p.images == nil || p.identities == nil || p.pools == nil {
		return v1beta1.NodeImage{}, fmt.Errorf("flex prerequisite clients are not configured")
	}
	if err := p.validateSubnet(ctx, nodeClass); err != nil {
		return v1beta1.NodeImage{}, err
	}
	if err := p.validatePool(ctx, nodeClass); err != nil {
		return v1beta1.NodeImage{}, err
	}
	if err := p.validateIdentity(ctx, nodeClass.Spec.Flex); err != nil {
		return v1beta1.NodeImage{}, err
	}
	return p.resolveHostImage(ctx, nodeClass.Spec.Flex.GetLocation(p.location), nodeClass.Spec.Flex.HostImageVersion)
}

func (p *Provider) validateSubnet(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) error {
	subnet, err := utils.GetVnetSubnetIDComponents(lo.FromPtr(nodeClass.Spec.VNETSubnetID))
	if err != nil || !strings.EqualFold(subnet.SubscriptionID, p.subscriptionID) {
		return invalid("FlexInvalidSubnet", "Flex subnet must belong to this controller's subscription")
	}
	network, err := p.networks.Get(ctx, subnet.ResourceGroupName, subnet.VNetName, nil)
	if err != nil {
		return fmt.Errorf("reading Flex virtual network: %w", err)
	}
	if !strings.EqualFold(lo.FromPtr(network.Location), nodeClass.Spec.Flex.GetLocation(p.location)) {
		return invalid("FlexRegionMismatch", "Flex subnet must be in the selected location")
	}
	return nil
}

func (p *Provider) validatePool(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) error {
	pool, err := p.parsePoolID(nodeClass.Spec.Flex.AgentPoolID)
	if err != nil {
		return err
	}
	poolResponse, err := p.pools.Get(ctx, pool.ResourceGroupName, pool.Parent.Name, pool.Name, nil)
	if err != nil {
		return fmt.Errorf("reading Flex agent pool: %w", err)
	}
	properties := poolResponse.Properties
	if properties == nil || string(lo.FromPtr(properties.Type)) != "FlexNodes" ||
		lo.FromPtr(properties.ProvisioningState) != "Succeeded" {
		return invalid("FlexPoolNotReady", "Flex pool must have type FlexNodes and provisioningState Succeeded")
	}
	version, err := nodeClass.GetKubernetesVersion()
	if err != nil {
		return err
	}
	expectedVersion, err := semver.ParseTolerant(version)
	if err != nil {
		return err
	}
	// Flex pools can expose only the declared orchestratorVersion. It must
	// still be a full patch version matching the selected NodeClass version.
	poolVersionString := lo.CoalesceOrEmpty(lo.FromPtr(properties.CurrentOrchestratorVersion), lo.FromPtr(properties.OrchestratorVersion))
	poolVersion, err := semver.Parse(strings.TrimPrefix(poolVersionString, "v"))
	if err != nil || !poolVersion.Equals(expectedVersion) {
		return invalid("FlexPoolVersionMismatch", "Flex pool's current Kubernetes version must match the NodeClass")
	}
	if lo.FromPtr(properties.MaxPods) != utils.GetMaxPods(nodeClass, options.FromContext(ctx).NetworkPlugin, options.FromContext(ctx).NetworkPluginMode) {
		return invalid("FlexPoolMaxPodsMismatch", "Flex pool maxPods must match the NodeClass")
	}
	return nil
}

func (p *Provider) validateIdentity(ctx context.Context, config *v1beta1.FlexNodeConfiguration) error {
	identity, err := arm.ParseResourceID(config.IdentityID)
	if err != nil || !strings.EqualFold(identity.SubscriptionID, p.subscriptionID) ||
		!strings.EqualFold(identity.ResourceType.String(), "Microsoft.ManagedIdentity/userAssignedIdentities") {
		return invalid("FlexInvalidIdentity", "Flex identityID must identify a user-assigned identity in this subscription")
	}
	identityResponse, err := p.identities.Get(ctx, identity.ResourceGroupName, identity.Name, nil)
	if err != nil {
		return fmt.Errorf("reading Flex host identity: %w", err)
	}
	if identityResponse.Properties == nil || !strings.EqualFold(lo.FromPtr(identityResponse.Properties.ClientID), config.IdentityClientID) {
		return invalid("FlexIdentityMismatch", "Flex identityClientID does not match identityID")
	}
	return nil
}

func (p *Provider) resolveHostImage(ctx context.Context, location, imageVersion string) (v1beta1.NodeImage, error) {
	image, err := p.images.Get(ctx, location, HostImagePublisher, HostImageOffer, HostImageSKU, imageVersion, nil)
	if err != nil {
		return v1beta1.NodeImage{}, fmt.Errorf("reading pinned Flex host image: %w", err)
	}
	if image.Properties == nil || image.Properties.OSDiskImage == nil ||
		lo.FromPtr(image.Properties.Architecture) != armcompute.ArchitectureTypesX64 ||
		lo.FromPtr(image.Properties.OSDiskImage.OperatingSystem) != armcompute.OperatingSystemTypesLinux ||
		lo.FromPtr(image.Properties.HyperVGeneration) != armcompute.HyperVGenerationTypesV2 {
		return v1beta1.NodeImage{}, invalid("FlexUnsupportedHostImage", "Flex host image must be an x64 Linux Gen2 image")
	}
	return v1beta1.NodeImage{
		ID: strings.Join([]string{HostImagePublisher, HostImageOffer, HostImageSKU, imageVersion}, ":"),
		Requirements: []corev1.NodeSelectorRequirement{
			{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
			{Key: v1beta1.LabelSKUHyperVGeneration, Operator: corev1.NodeSelectorOpIn, Values: []string{v1beta1.HyperVGenerationV2}},
		},
	}, nil
}

// DeleteMachine never deletes the pool, identity, network, or backing VM.
func (p *Provider) DeleteMachine(ctx context.Context, poolID, nodeName string) error {
	pool, err := p.parsePoolID(poolID)
	if err != nil {
		return err
	}
	if p.machines == nil || p.pools == nil {
		return fmt.Errorf("flex lifecycle clients are not configured")
	}
	exists, err := p.machineExists(ctx, pool, nodeName)
	if err != nil || !exists {
		return err
	}
	poller, err := p.pools.BeginDeleteMachines(ctx, pool.ResourceGroupName, pool.Parent.Name, pool.Name,
		armcontainerservice.AgentPoolDeleteMachinesParameter{MachineNames: []*string{lo.ToPtr(nodeName)}}, nil)
	if err != nil {
		if sdkerrors.IsNotFoundErr(err) {
			return nil
		}
		return fmt.Errorf("starting Flex Machine deletion: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil && !sdkerrors.IsNotFoundErr(err) {
		return fmt.Errorf("waiting for Flex Machine deletion: %w", err)
	}
	return nil
}

func (p *Provider) machineExists(ctx context.Context, pool *arm.ResourceID, nodeName string) (bool, error) {
	poolResponse, err := p.pools.Get(ctx, pool.ResourceGroupName, pool.Parent.Name, pool.Name, nil)
	if err != nil {
		if sdkerrors.IsNotFoundErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading Flex pool before Machine deletion: %w", err)
	}
	if poolResponse.Properties == nil || string(lo.FromPtr(poolResponse.Properties.Type)) != "FlexNodes" {
		return false, invalid("FlexInvalidPool", "refusing to delete a Machine outside a FlexNodes pool")
	}
	if _, err := p.machines.Get(ctx, pool.ResourceGroupName, pool.Parent.Name, pool.Name, nodeName, nil); err != nil {
		if sdkerrors.IsNotFoundErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading Flex Machine before deletion: %w", err)
	}
	return true, nil
}
