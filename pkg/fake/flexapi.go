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

package fake

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/samber/lo"
)

type VirtualNetworksAPI struct {
	Location string
	GetFunc  func(context.Context, string, string, *armnetwork.VirtualNetworksClientGetOptions) (armnetwork.VirtualNetworksClientGetResponse, error)
}

func (c *VirtualNetworksAPI) Get(ctx context.Context, resourceGroup, name string, opts *armnetwork.VirtualNetworksClientGetOptions) (armnetwork.VirtualNetworksClientGetResponse, error) {
	if c.GetFunc != nil {
		return c.GetFunc(ctx, resourceGroup, name, opts)
	}
	return armnetwork.VirtualNetworksClientGetResponse{
		VirtualNetwork: armnetwork.VirtualNetwork{Location: lo.ToPtr(c.Location)},
	}, nil
}

type VirtualMachineImagesAPI struct {
	GetFunc func(context.Context, string, string, string, string, string, *armcompute.VirtualMachineImagesClientGetOptions) (armcompute.VirtualMachineImagesClientGetResponse, error)
}

func (c *VirtualMachineImagesAPI) Get(ctx context.Context, location, publisher, offer, sku, version string, opts *armcompute.VirtualMachineImagesClientGetOptions) (armcompute.VirtualMachineImagesClientGetResponse, error) {
	if c.GetFunc != nil {
		return c.GetFunc(ctx, location, publisher, offer, sku, version, opts)
	}
	return armcompute.VirtualMachineImagesClientGetResponse{
		VirtualMachineImage: armcompute.VirtualMachineImage{
			Name: lo.ToPtr(version),
			Properties: &armcompute.VirtualMachineImageProperties{
				Architecture:     lo.ToPtr(armcompute.ArchitectureTypesX64),
				HyperVGeneration: lo.ToPtr(armcompute.HyperVGenerationTypesV2),
				OSDiskImage: &armcompute.OSDiskImage{
					OperatingSystem: lo.ToPtr(armcompute.OperatingSystemTypesLinux),
				},
			},
		},
	}, nil
}

type UserAssignedIdentitiesAPI struct {
	ClientID string
	GetFunc  func(context.Context, string, string, *armmsi.UserAssignedIdentitiesClientGetOptions) (armmsi.UserAssignedIdentitiesClientGetResponse, error)
}

func (c *UserAssignedIdentitiesAPI) Get(ctx context.Context, resourceGroup, name string, opts *armmsi.UserAssignedIdentitiesClientGetOptions) (armmsi.UserAssignedIdentitiesClientGetResponse, error) {
	if c.GetFunc != nil {
		return c.GetFunc(ctx, resourceGroup, name, opts)
	}
	return armmsi.UserAssignedIdentitiesClientGetResponse{
		Identity: armmsi.Identity{
			Properties: &armmsi.UserAssignedIdentityProperties{ClientID: lo.ToPtr(c.ClientID)},
		},
	}, nil
}
