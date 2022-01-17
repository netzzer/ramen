/*
Copyright 2021 The RamenDR authors.

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

package util

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/ghodss/yaml"
	"github.com/go-logr/logr"
	ocmworkv1 "github.com/open-cluster-management/api/work/v1"
	operatorsv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	errorswrapper "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	rmn "github.com/ramendr/ramen/api/v1alpha1"
)

const (
	DrClusterManifestWorkName = "ramen-dr-cluster"

	// ManifestWorkNameFormat is a formated a string used to generate the manifest name
	// The format is name-namespace-type-mw where:
	// - name is the DRPC name
	// - namespace is the DRPC namespace
	// - type is "vrg"
	ManifestWorkNameFormat string = "%s-%s-%s-mw"

	// ManifestWork Types
	MWTypeVRG string = "vrg"
	MWTypeNS  string = "ns"

	// Annotations for MW and PlacementRule
	DRPCNameAnnotation      = "drplacementcontrol.ramendr.openshift.io/drpc-name"
	DRPCNamespaceAnnotation = "drplacementcontrol.ramendr.openshift.io/drpc-namespace"
)

type MWUtil struct {
	client.Client
	Ctx           context.Context
	Log           logr.Logger
	InstName      string
	InstNamespace string
}

func ManifestWorkName(name, namespace, mwType string) string {
	return fmt.Sprintf(ManifestWorkNameFormat, name, namespace, mwType)
}

func (mwu *MWUtil) BuildManifestWorkName(mwType string) string {
	return ManifestWorkName(mwu.InstName, mwu.InstNamespace, mwType)
}

func (mwu *MWUtil) FindManifestWork(mwName, managedCluster string) (*ocmworkv1.ManifestWork, error) {
	if managedCluster == "" {
		return nil, fmt.Errorf("invalid cluster for MW %s", mwName)
	}

	mw := &ocmworkv1.ManifestWork{}

	err := mwu.Client.Get(mwu.Ctx, types.NamespacedName{Name: mwName, Namespace: managedCluster}, mw)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("%w", err)
		}

		return nil, fmt.Errorf("failed to retrieve manifestwork (%w)", err)
	}

	return mw, nil
}

func IsManifestInAppliedState(mw *ocmworkv1.ManifestWork) bool {
	applied := false
	available := false
	degraded := false
	conditions := mw.Status.Conditions

	if len(conditions) > 0 {
		for _, condition := range conditions {
			if condition.Status == metav1.ConditionTrue {
				switch {
				case condition.Type == ocmworkv1.WorkApplied:
					applied = true
				case condition.Type == ocmworkv1.WorkAvailable:
					available = true
				case condition.Type == ocmworkv1.WorkDegraded:
					degraded = true
				}
			}
		}
	}

	return applied && available && !degraded
}

func (mwu *MWUtil) CreateOrUpdateVRGManifestWork(
	name, namespace, homeCluster string,
	vrg rmn.VolumeReplicationGroup) error {
	mwu.Log.Info(fmt.Sprintf("Create or Update manifestwork %s:%s:%s:%+v",
		name, namespace, homeCluster, vrg))

	manifestWork, err := mwu.generateVRGManifestWork(name, namespace, homeCluster, vrg)
	if err != nil {
		return err
	}

	return mwu.createOrUpdateManifestWork(manifestWork, homeCluster)
}

func (mwu *MWUtil) generateVRGManifestWork(name, namespace, homeCluster string,
	vrg rmn.VolumeReplicationGroup) (*ocmworkv1.ManifestWork, error) {
	vrgClientManifest, err := mwu.generateVRGManifest(vrg)
	if err != nil {
		mwu.Log.Error(err, "failed to generate VolumeReplicationGroup manifest")

		return nil, err
	}

	manifests := []ocmworkv1.Manifest{*vrgClientManifest}

	return mwu.newManifestWork(
		fmt.Sprintf(ManifestWorkNameFormat, name, namespace, MWTypeVRG),
		homeCluster,
		map[string]string{"app": "VRG"},
		manifests), nil
}

func (mwu *MWUtil) generateVRGManifest(vrg rmn.VolumeReplicationGroup) (*ocmworkv1.Manifest, error) {
	return mwu.GenerateManifest(vrg)
}

func (mwu *MWUtil) CreateOrUpdateNamespaceManifest(
	name string, namespaceName string, managedClusterNamespace string) error {
	manifest, err := mwu.GenerateManifest(namespace(namespaceName))
	if err != nil {
		return err
	}

	labels := map[string]string{}
	manifests := []ocmworkv1.Manifest{
		*manifest,
	}

	mwName := fmt.Sprintf(ManifestWorkNameFormat, name, namespaceName, MWTypeNS)
	manifestWork := mwu.newManifestWork(mwName, managedClusterNamespace, labels, manifests)

	return mwu.createOrUpdateManifestWork(manifestWork, managedClusterNamespace)
}

func namespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func (mwu *MWUtil) CreateOrUpdateDrClusterManifestWork(
	clusterName string,
	ramenConfig *rmn.RamenConfig,
	drClusterOperatorChannelName string,
	drClusterOperatorPackageName string,
	drClusterOperatorNamespaceName string,
	drClusterOperatorCatalogSourceName string,
	drClusterOperatorCatalogSourceNamespaceName string,
	drClusterOperatorClusterServiceVersionName string,
) error {
	objects := []interface{}{
		vrgClusterRole,
		vrgClusterRoleBinding,
	}

	if ramenConfig.DrClusterOperator.DeploymentAutomationEnabled {
		configMap, err := configMap(drClusterOperatorNamespaceName, ramenConfig)
		if err != nil {
			return err
		}

		objects = append(objects,
			namespace(drClusterOperatorNamespaceName),
			olmClusterRole,
			olmRoleBinding(drClusterOperatorNamespaceName),
			operatorGroup(drClusterOperatorNamespaceName),
			subscription(
				drClusterOperatorChannelName,
				drClusterOperatorPackageName,
				drClusterOperatorNamespaceName,
				drClusterOperatorCatalogSourceName,
				drClusterOperatorCatalogSourceNamespaceName,
				drClusterOperatorClusterServiceVersionName,
			),
			configMap,
		)
	}

	manifests := make([]ocmworkv1.Manifest, len(objects))

	for i, object := range objects {
		manifest, err := mwu.GenerateManifest(object)
		if err != nil {
			mwu.Log.Error(err, "failed to generate manifest", "object", object)

			return err
		}

		manifests[i] = *manifest
	}

	return mwu.createOrUpdateManifestWork(
		mwu.newManifestWork(
			DrClusterManifestWorkName,
			clusterName,
			map[string]string{},
			manifests,
		),
		clusterName,
	)
}

var (
	vrgClusterRole = &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{Kind: "ClusterRole", APIVersion: "rbac.authorization.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "open-cluster-management:klusterlet-work-sa:agent:volrepgroup-edit"},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"ramendr.openshift.io"},
				Resources: []string{"volumereplicationgroups"},
				Verbs:     []string{"create", "get", "list", "update", "delete"},
			},
		},
	}

	vrgClusterRoleBinding = &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{Kind: "ClusterRoleBinding", APIVersion: "rbac.authorization.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "open-cluster-management:klusterlet-work-sa:agent:volrepgroup-edit"},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "klusterlet-work-sa",
				Namespace: "open-cluster-management-agent",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "open-cluster-management:klusterlet-work-sa:agent:volrepgroup-edit",
		},
	}

	olmClusterRole = &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{Kind: "ClusterRole", APIVersion: "rbac.authorization.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "open-cluster-management:klusterlet-work-sa:agent:olm-edit"},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"operators.coreos.com"},
				Resources: []string{"operatorgroups"},
				Verbs:     []string{"create", "get", "list", "update", "delete"},
			},
		},
	}
)

func olmRoleBinding(namespaceName string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{Kind: "RoleBinding", APIVersion: "rbac.authorization.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "open-cluster-management:klusterlet-work-sa:agent:olm-edit",
			Namespace: namespaceName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "klusterlet-work-sa",
				Namespace: "open-cluster-management-agent",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "open-cluster-management:klusterlet-work-sa:agent:olm-edit",
		},
	}
}

func operatorGroup(namespaceName string) *operatorsv1.OperatorGroup {
	return &operatorsv1.OperatorGroup{
		TypeMeta:   metav1.TypeMeta{Kind: "OperatorGroup", APIVersion: "operators.coreos.com/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "ramen-operator-group", Namespace: namespaceName},
	}
}

func subscription(
	channelName string,
	packageName string,
	namespaceName string,
	catalogSourceName string,
	catalogSourceNamespaceName string,
	clusterServiceVersionName string,
) *operatorsv1alpha1.Subscription {
	return &operatorsv1alpha1.Subscription{
		TypeMeta:   metav1.TypeMeta{Kind: "Subscription", APIVersion: "operators.coreos.com/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "ramen-dr-cluster-subscription", Namespace: namespaceName},
		Spec: &operatorsv1alpha1.SubscriptionSpec{
			CatalogSource:          catalogSourceName,
			CatalogSourceNamespace: catalogSourceNamespaceName,
			Package:                packageName,
			Channel:                channelName,
			StartingCSV:            clusterServiceVersionName,
			InstallPlanApproval:    "Automatic",
		},
	}
}

func configMap(namespaceName string, ramenConfigHub *rmn.RamenConfig) (*corev1.ConfigMap, error) {
	ramenConfigDrCluster := *ramenConfigHub
	ramenConfigDrCluster.LeaderElection.ResourceName = "dr-cluster.ramendr.openshift.io"
	ramenConfigDrCluster.RamenControllerType = rmn.DRCluster

	ramenConfigDrClusterYaml, err := yaml.Marshal(ramenConfigDrCluster)
	if err != nil {
		return nil, fmt.Errorf("config map yaml marshal %w", err)
	}

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ramen-dr-cluster-operator-config",
			Namespace: namespaceName,
		},
		Data: map[string]string{
			"ramen_manager_config.yaml": string(ramenConfigDrClusterYaml),
		},
	}, nil
}

func (mwu *MWUtil) GenerateManifest(obj interface{}) (*ocmworkv1.Manifest, error) {
	objJSON, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %v to JSON, error %w", obj, err)
	}

	manifest := &ocmworkv1.Manifest{}
	manifest.RawExtension = runtime.RawExtension{Raw: objJSON}

	return manifest, nil
}

func (mwu *MWUtil) newManifestWork(name string, mcNamespace string,
	labels map[string]string, manifests []ocmworkv1.Manifest) *ocmworkv1.ManifestWork {
	return &ocmworkv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: mcNamespace,
			Labels:    labels,
			Annotations: map[string]string{
				DRPCNameAnnotation:      mwu.InstName,
				DRPCNamespaceAnnotation: mwu.InstNamespace,
			},
		},
		Spec: ocmworkv1.ManifestWorkSpec{
			Workload: ocmworkv1.ManifestsTemplate{
				Manifests: manifests,
			},
		},
	}
}

func (mwu *MWUtil) createOrUpdateManifestWork(
	mw *ocmworkv1.ManifestWork,
	managedClusternamespace string) error {
	foundMW := &ocmworkv1.ManifestWork{}

	err := mwu.Client.Get(mwu.Ctx,
		types.NamespacedName{Name: mw.Name, Namespace: managedClusternamespace},
		foundMW)
	if err != nil {
		if !errors.IsNotFound(err) {
			return errorswrapper.Wrap(err, fmt.Sprintf("failed to fetch ManifestWork %s", mw.Name))
		}

		// Let DRPC receive notification for any changes to ManifestWork CR created by it.
		// if err := ctrl.SetControllerReference(d.instance, mw, d.reconciler.Scheme); err != nil {
		//	return fmt.Errorf("failed to set owner reference to ManifestWork resource (%s/%s) (%v)",
		//		mw.Name, mw.Namespace, err)
		// }

		mwu.Log.Info("Creating ManifestWork", "cluster", managedClusternamespace, "MW", mw)

		return mwu.Client.Create(mwu.Ctx, mw)
	}

	if !reflect.DeepEqual(foundMW.Spec, mw.Spec) {
		mw.Spec.DeepCopyInto(&foundMW.Spec)

		mwu.Log.Info("ManifestWork exists.", "name", mw, "namespace", foundMW)

		return mwu.Client.Update(mwu.Ctx, foundMW)
	}

	return nil
}

func (mwu *MWUtil) DeleteManifestWorksForCluster(clusterName string) error {
	// VRG
	err := mwu.deleteManifestWorkWrapper(clusterName, MWTypeVRG)
	if err != nil {
		mwu.Log.Error(err, "failed to delete MW for VRG")

		return fmt.Errorf("failed to delete ManifestWork for VRG in namespace %s (%w)", clusterName, err)
	}

	// The ManifestWork that created a Namespace is intentionally left on the server

	return nil
}

func (mwu *MWUtil) deleteManifestWorkWrapper(fromCluster string, mwType string) error {
	mwName := mwu.BuildManifestWorkName(mwType)
	mwNamespace := fromCluster

	return mwu.DeleteManifestWork(mwName, mwNamespace)
}

func (mwu *MWUtil) DeleteManifestWork(mwName, mwNamespace string) error {
	mwu.Log.Info("Delete ManifestWork from", "namespace", mwNamespace, "name", mwName)

	mw := &ocmworkv1.ManifestWork{}

	err := mwu.Client.Get(mwu.Ctx, types.NamespacedName{Name: mwName, Namespace: mwNamespace}, mw)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("failed to retrieve manifestwork for type: %s. Error: %w", mwName, err)
	}

	mwu.Log.Info("Deleting ManifestWork", "name", mw.Name, "namespace", mwNamespace)

	err = mwu.Client.Delete(mwu.Ctx, mw)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete MW. Error %w", err)
	}

	return nil
}

func GetMetricValueSingle(name string, mfType dto.MetricType) (float64, error) {
	mf, err := getMetricFamilyFromRegistry(name)
	if err != nil {
		return 0.0, fmt.Errorf("GetMetricValueSingle returned error finding MetricFamily: %w", err)
	}

	val, err := getMetricValueFromMetricFamilyByType(mf, mfType)
	if err != nil {
		return 0.0, fmt.Errorf("GetMetricValueSingle returned error finding Value: %w", err)
	}

	return val, nil
}

func getMetricFamilyFromRegistry(name string) (*dto.MetricFamily, error) {
	metricsFamilies, err := metrics.Registry.Gather() // TODO: see if this can be made more generic
	if err != nil {
		return nil, fmt.Errorf("found error during Gather step of getMetricFamilyFromRegistry: %w", err)
	}

	if len(metricsFamilies) == 0 {
		return nil, fmt.Errorf("couldn't get metricsFamilies from Prometheus Registry")
	}

	// TODO: find out if there's a better way to search than linear scan
	for i := 0; i < len(metricsFamilies); i++ {
		if *metricsFamilies[i].Name == name {
			return metricsFamilies[i], nil
		}
	}

	return nil, fmt.Errorf(fmt.Sprint("couldn't find MetricFamily with name", name))
}

func getMetricValueFromMetricFamilyByType(mf *dto.MetricFamily, mfType dto.MetricType) (float64, error) {
	if *mf.Type != mfType {
		return 0.0, fmt.Errorf("getMetricValueFromMetricFamilyByType passed invalid type. Wanted %s, got %s",
			string(mfType), string(*mf.Type))
	}

	if len(mf.Metric) != 1 {
		return 0.0, fmt.Errorf("getMetricValueFromMetricFamilyByType only supports Metric length=1")
	}

	switch mfType {
	case dto.MetricType_COUNTER:
		return *mf.Metric[0].Counter.Value, nil
	case dto.MetricType_GAUGE:
		return *mf.Metric[0].Gauge.Value, nil
	case dto.MetricType_HISTOGRAM:
		// Count is more useful for testing over Sum; get Sum elsewhere if needed
		return float64(*mf.Metric[0].Histogram.SampleCount), nil
	case dto.MetricType_SUMMARY:
		fallthrough
	case dto.MetricType_UNTYPED:
		fallthrough
	default:
		return 0.0, fmt.Errorf("getMetricValueFromMetricFamilyByType doesn't support type %s yet. Implement this",
			string(mfType))
	}
}
