/*
Copyright The Platform Mesh Authors.

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

// Package migrations holds idempotent steps that bring a running kcp
// workspace tree in line with the operator's current APIExport/APIBinding shape.
package migrations

import (
	"context"
	"time"

	pmcorev1alpha1 "go.platform-mesh.io/apis/core/v1alpha1"
	gcerrors "go.platform-mesh.io/golang-commons/errors"
	"go.platform-mesh.io/golang-commons/logger"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	kcptenancyv1alpha "github.com/kcp-dev/kcp/sdk/apis/tenancy/v1alpha1"
)

// KcpClientFactory copies subroutines.KcpHelper's method, avoids an import cycle.
type KcpClientFactory interface {
	NewKcpClient(config *rest.Config, workspacePath string) (ctrlruntimeclient.Client, error)
}

// Matches subroutines.fieldManagerKcpSetup, must stay identical: this is
// the field manager identity server-side apply uses for ownership.
const fieldManagerKcpSetup = "platform-mesh-kcp-setup"

// Deps is what a Migration needs: generic kcp/client plumbing, not
// subroutine internals.
type Deps struct {
	KcpHelper KcpClientFactory
	Config    *rest.Config
	Instance  *pmcorev1alpha1.PlatformMesh
}

// Migration is one idempotent migration step, checked on every reconcile.
// A run against already-migrated or never-legacy state must be a no-op.
type Migration interface {
	// Name identifies the step in logs and wrapped errors.
	Name() string
	// Migrate checks for the legacy state this step handles and fixes it up
	// if found.
	Migrate(ctx context.Context, deps Deps) error
}

// Migrator runs a fixed, ordered list of Migration steps.
type Migrator struct {
	steps []Migration
}

func NewMigrator(steps ...Migration) *Migrator {
	return &Migrator{steps: steps}
}

// Default returns the migrator wired with every migration step the operator
// currently ships.
func Default() *Migrator {
	return NewMigrator(
		orgsAPIExportSplitMigration{},
		providerAPIExportSplitMigration{},
	)
}

// Migrate runs every step in order, stopping at the first error. Steps
// must be idempotent so a retry safely restarts from the top.
func (m *Migrator) Migrate(ctx context.Context, deps Deps) error {
	log := logger.LoadLoggerFromContext(ctx).ChildLogger("component", "migrations")

	for _, step := range m.steps {
		if err := step.Migrate(ctx, deps); err != nil {
			return gcerrors.Wrap(err, "migration %s failed", step.Name())
		}
		log.Debug().Str("migration", step.Name()).Msg("migration step checked")
	}

	return nil
}

const corePlatformMeshIOExport = "core.platform-mesh.io"

// findLegacyBinding returns the APIBinding, if any, that still references the pre-split
// core.platform-mesh.io export and has resourceMarker among its locked boundResources.
func findLegacyBinding(bindings *unstructured.UnstructuredList, resourceMarker string) *unstructured.Unstructured {
	for i := range bindings.Items {
		b := &bindings.Items[i]
		exportName, _, _ := unstructured.NestedString(b.Object, "spec", "reference", "export", "name")
		if exportName != corePlatformMeshIOExport {
			continue
		}

		boundResources, _, _ := unstructured.NestedSlice(b.Object, "status", "boundResources")
		for _, br := range boundResources {
			if resource, ok := br.(map[string]any); ok && resource["resource"] == resourceMarker {
				return b
			}
		}
	}

	return nil
}

// deleteWithSuccessorWait sets deletionPolicy=WaitForSuccessor and deletes binding; kcp (>=v0.33.0)
// holds the finalizer until a same-identity successor adopts its instances. Apply a successor first, then call waitForBindingGone.
func deleteWithSuccessorWait(ctx context.Context, client ctrlruntimeclient.Client, binding *unstructured.Unstructured) error {
	policyPatch := ctrlruntimeclient.RawPatch(types.MergePatchType, []byte(`{"spec":{"deletionPolicy":"WaitForSuccessor"}}`))
	if err := client.Patch(ctx, binding, policyPatch); err != nil {
		return gcerrors.Wrap(err, "Failed to set deletionPolicy=WaitForSuccessor on binding %s", binding.GetName())
	}

	if err := client.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
		return gcerrors.Wrap(err, "Failed to delete binding %s", binding.GetName())
	}

	return nil
}

// waitForBindingGone waits for a deleted binding to actually disappear, i.e. for kcp to find and
// apply a successor for every one of its bound resources.
func waitForBindingGone(ctx context.Context, client ctrlruntimeclient.Client, bindingName string) error {
	err := wait.PollUntilContextTimeout(ctx, time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(schema.GroupVersionKind{Group: "apis.kcp.io", Version: "v1alpha2", Kind: "APIBinding"})
		getErr := client.Get(ctx, types.NamespacedName{Name: bindingName}, check)
		return apierrors.IsNotFound(getErr), nil
	})

	if err != nil {
		return gcerrors.Wrap(err, "Timed out waiting for binding %s to be adopted and removed", bindingName)
	}

	return nil
}

// applyBinding creates or updates an APIBinding named name, referencing export at path.
func applyBinding(ctx context.Context, client ctrlruntimeclient.Client, name, export, path string) error {
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(schema.GroupVersionKind{Group: "apis.kcp.io", Version: "v1alpha2", Kind: "APIBinding"})
	fresh.SetName(name)
	if err := unstructured.SetNestedMap(fresh.Object, map[string]any{
		"export": map[string]any{"name": export, "path": path},
	}, "spec", "reference"); err != nil {
		return gcerrors.Wrap(err, "Failed to build %s binding", name)
	}
	return client.Apply(ctx, ctrlruntimeclient.ApplyConfigurationFromUnstructured(fresh),
		ctrlruntimeclient.FieldOwner(fieldManagerKcpSetup), ctrlruntimeclient.ForceOwnership)
}

// orgsAPIExportSplitMigration swaps root:orgs off the pre-split core.platform-mesh.io
// binding onto orgs.core.platform-mesh.io, which shares identity so kcp adopts automatically.
// Safe to remove once every environment has reconciled past this change (#171/#47).
type orgsAPIExportSplitMigration struct{}

func (orgsAPIExportSplitMigration) Name() string { return "orgs-apiexport-split" }

func (orgsAPIExportSplitMigration) Migrate(ctx context.Context, deps Deps) error {
	log := logger.LoadLoggerFromContext(ctx).ChildLogger("migration", "orgs-apiexport-split")

	orgsClient, err := deps.KcpHelper.NewKcpClient(deps.Config, "root:orgs")
	if err != nil {
		return gcerrors.Wrap(err, "Failed to create kcp client for root:orgs workspace")
	}

	bindings := &unstructured.UnstructuredList{}
	bindings.SetGroupVersionKind(schema.GroupVersionKind{Group: "apis.kcp.io", Version: "v1alpha2", Kind: "APIBindingList"})
	if err := orgsClient.List(ctx, bindings); err != nil {
		// root:orgs may not exist yet on a fresh install; nothing to migrate.
		return nil //nolint:nilerr
	}

	legacyBinding := findLegacyBinding(bindings, "stores")
	if legacyBinding == nil {
		return nil
	}

	log.Info().Str("binding", legacyBinding.GetName()).
		Msg("root:orgs still on pre-split core.platform-mesh.io binding, migrating to orgs.core.platform-mesh.io")

	// The fresh successor bindings are applied by createKcpResources before migrations run, so
	// they're already in place by now.
	if err := deleteWithSuccessorWait(ctx, orgsClient, legacyBinding); err != nil {
		return gcerrors.Wrap(err, "Failed to migrate legacy core.platform-mesh.io binding in root:orgs")
	}

	if err := waitForBindingGone(ctx, orgsClient, legacyBinding.GetName()); err != nil {
		return gcerrors.Wrap(err, "Failed to migrate legacy core.platform-mesh.io binding in root:orgs")
	}

	log.Info().Msg("legacy core.platform-mesh.io binding removed from root:orgs, resources adopted by orgs.core.platform-mesh.io")
	return nil
}

// uiExportPath returns the path ui.platform-mesh.io lives at for provider workspaces.
// Unconditional, so existing providers migrate to it rather than being left behind.
func uiExportPath(_ *pmcorev1alpha1.PlatformMesh) (string, bool) {
	return "root:platform-mesh-system", true
}

// providerAPIExportSplitMigration does for provider workspaces what
// orgsAPIExportSplitMigration does for root:orgs (trimmed core.platform-mesh.io + ui.platform-mesh.io).
// Safe to remove under the same condition as orgsAPIExportSplitMigration above.
type providerAPIExportSplitMigration struct{}

func (providerAPIExportSplitMigration) Name() string { return "provider-apiexport-split" }

func (providerAPIExportSplitMigration) Migrate(ctx context.Context, deps Deps) error {
	providersClient, err := deps.KcpHelper.NewKcpClient(deps.Config, "root:providers")
	if err != nil {
		return gcerrors.Wrap(err, "Failed to create kcp client for root:providers workspace")
	}

	var workspaces kcptenancyv1alpha.WorkspaceList
	if err := providersClient.List(ctx, &workspaces); err != nil {
		// root:providers may not exist yet on a fresh install; nothing to migrate.
		return nil //nolint:nilerr
	}

	uiPath, uiOptedIn := uiExportPath(deps.Instance)

	for _, ws := range workspaces.Items {
		if ws.Name == "system" {
			continue
		}
		if err := migrateLegacyProviderBinding(ctx, deps.KcpHelper, deps.Config, ws.Name, uiPath, uiOptedIn); err != nil {
			return gcerrors.Wrap(err, "Failed to migrate legacy provider binding for %s", ws.Name)
		}
	}
	
	return nil
}

// migrateLegacyProviderBinding swaps a single provider workspace off the old, pre-split
// core.platform-mesh.io binding. If opted into ui.platform-mesh.io, it shares identity and kcp
// adopts the instances automatically; otherwise they're deleted as before.
func migrateLegacyProviderBinding(
	ctx context.Context, kcpHelper KcpClientFactory, config *rest.Config, providerName, uiPath string, uiOptedIn bool,
) error {
	log := logger.LoadLoggerFromContext(ctx).ChildLogger("migration", "provider-apiexport-split")

	providerClient, err := kcpHelper.NewKcpClient(config, "root:providers:"+providerName)
	if err != nil {
		return gcerrors.Wrap(err, "Failed to create kcp client for provider workspace %s", providerName)
	}

	bindings := &unstructured.UnstructuredList{}
	bindings.SetGroupVersionKind(schema.GroupVersionKind{Group: "apis.kcp.io", Version: "v1alpha2", Kind: "APIBindingList"})
	if err := providerClient.List(ctx, bindings); err != nil {
		return nil //nolint:nilerr
	}

	legacyBinding := findLegacyBinding(bindings, "contentconfigurations")
	if legacyBinding == nil {
		return nil
	}

	log.Info().Str("provider", providerName).Str("binding", legacyBinding.GetName()).
		Msg("provider workspace still on pre-split core.platform-mesh.io binding, migrating")

	if !uiOptedIn {
		// No successor will ever exist here, so WaitForSuccessor would hold forever; delete outright.
		for _, kind := range []string{"ContentConfiguration", "ProviderMetadata"} {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(schema.GroupVersionKind{Group: "ui.platform-mesh.io", Version: "v1alpha1", Kind: kind + "List"})
			if err := providerClient.List(ctx, list); err != nil {
				return gcerrors.Wrap(err, "Failed to list %ss in provider workspace %s", kind, providerName)
			}
			for i := range list.Items {
				obj := &list.Items[i]
				if len(obj.GetFinalizers()) == 0 {
					continue
				}
				patch := ctrlruntimeclient.RawPatch(types.JSONPatchType, []byte(`[{"op":"remove","path":"/metadata/finalizers"}]`))
				if err := providerClient.Patch(ctx, obj, patch); err != nil {
					return gcerrors.Wrap(err, "Failed to clear finalizers on %s in provider workspace %s", kind, providerName)
				}
			}
			if len(list.Items) > 0 {
				log.Warn().Str("provider", providerName).Str("kind", kind).Int("count", len(list.Items)).
					Msg("provider workspace not opted into ui.platform-mesh.io, existing data is now unreachable and will be deleted")
			}
		}
		if err := providerClient.Delete(ctx, legacyBinding); err != nil && !apierrors.IsNotFound(err) {
			return gcerrors.Wrap(err, "Failed to delete legacy core.platform-mesh.io binding in provider workspace %s", providerName)
		}
		if err := applyBinding(ctx, providerClient, "core.platform-mesh.io", corePlatformMeshIOExport, "root:platform-mesh-system"); err != nil {
			return gcerrors.Wrap(err, "Failed to apply core.platform-mesh.io binding for provider workspace %s", providerName)
		}
		log.Info().Str("provider", providerName).Msg("legacy core.platform-mesh.io binding removed, fresh core.platform-mesh.io binding applied")
		return nil
	}

	// Apply the fresh bindings before deleting the legacy one, or the CRD has a zero-binding
	// window where it can get garbage collected regardless of kcp's adoption bookkeeping.
	if err := applyBinding(ctx, providerClient, "core.platform-mesh.io", corePlatformMeshIOExport, "root:platform-mesh-system"); err != nil {
		return gcerrors.Wrap(err, "Failed to apply core.platform-mesh.io binding for provider workspace %s", providerName)
	}
	if err := applyBinding(ctx, providerClient, "ui.platform-mesh.io", "ui.platform-mesh.io", uiPath); err != nil {
		return gcerrors.Wrap(err, "Failed to apply ui.platform-mesh.io binding for provider workspace %s", providerName)
	}
	if err := deleteWithSuccessorWait(ctx, providerClient, legacyBinding); err != nil {
		return gcerrors.Wrap(err, "Failed to migrate legacy core.platform-mesh.io binding in provider workspace %s", providerName)
	}
	if err := waitForBindingGone(ctx, providerClient, legacyBinding.GetName()); err != nil {
		return gcerrors.Wrap(err, "Failed to migrate legacy core.platform-mesh.io binding in provider workspace %s", providerName)
	}

	log.Info().Str("provider", providerName).
		Msg("legacy core.platform-mesh.io binding removed, fresh bindings applied, content adopted by ui.platform-mesh.io")
	return nil
}
