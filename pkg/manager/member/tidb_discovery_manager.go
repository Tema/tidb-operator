// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package member

import (
	"encoding/json"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pingcap/tidb-operator/pkg/apis/label"
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/controller"
	"github.com/pingcap/tidb-operator/pkg/util"
)

const (
	PdTlsCertPath = "/var/lib/pd-tls"
)

type TidbDiscoveryManager interface {
	Reconcile(obj client.Object) error
}

type realTidbDiscoveryManager struct {
	deps *controller.Dependencies
}

func NewTidbDiscoveryManager(deps *controller.Dependencies) TidbDiscoveryManager {
	return &realTidbDiscoveryManager{deps: deps}
}

func (m *realTidbDiscoveryManager) Reconcile(obj client.Object) error {
	metaObj, ok := obj.(metav1.Object)
	if !ok {
		return fmt.Errorf("%T is not a metav1.Object", obj)
	}

	var (
		clusterPolicyRule rbacv1.PolicyRule
		preferIPv6        bool
	)
	switch cluster := obj.(type) {
	case *v1alpha1.TidbCluster:
		// If PD is not specified return
		if cluster.Spec.PD == nil && !cluster.AcrossK8s() {
			return nil
		}
		clusterPolicyRule = rbacv1.PolicyRule{
			APIGroups:     []string{v1alpha1.GroupName},
			Resources:     []string{v1alpha1.TiDBClusterName},
			ResourceNames: []string{metaObj.GetName()},
			Verbs:         []string{"get"},
		}
		preferIPv6 = cluster.Spec.PreferIPv6
	case *v1alpha1.DMCluster:
		clusterPolicyRule = rbacv1.PolicyRule{
			APIGroups:     []string{v1alpha1.GroupName},
			Resources:     []string{v1alpha1.DMClusterName},
			ResourceNames: []string{metaObj.GetName()},
			Verbs:         []string{"get"},
		}
	default:
		klog.Warningf("unsupported type %T for discovery", obj)
		return nil
	}

	meta, _ := getDiscoveryMeta(metaObj, controller.DiscoveryMemberName)
	// Ensure RBAC
	_, err := m.deps.TypedControl.CreateOrUpdateRole(obj, &rbacv1.Role{
		ObjectMeta: meta,
		Rules: []rbacv1.PolicyRule{
			clusterPolicyRule,
			{
				APIGroups: []string{corev1.GroupName},
				Resources: []string{"secrets"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	})
	if err != nil {
		return controller.RequeueErrorf("error creating or updating discovery role: %v", err)
	}
	_, err = m.deps.TypedControl.CreateOrUpdateServiceAccount(obj, &corev1.ServiceAccount{
		ObjectMeta: meta,
	})
	if err != nil {
		return controller.RequeueErrorf("error creating or updating discovery serviceaccount: %v", err)
	}
	_, err = m.deps.TypedControl.CreateOrUpdateRoleBinding(obj, &rbacv1.RoleBinding{
		ObjectMeta: meta,
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind,
			Name: meta.Name,
		}},
		RoleRef: rbacv1.RoleRef{
			Kind:     "Role",
			Name:     meta.Name,
			APIGroup: rbacv1.GroupName,
		},
	})
	if err != nil {
		return controller.RequeueErrorf("error creating or updating discovery rolebinding: %v", err)
	}
	d, err := m.getTidbDiscoveryDeployment(metaObj)
	if err != nil {
		return controller.RequeueErrorf("error generating discovery deployment: %v", err)
	}
	deploy, err := m.deps.TypedControl.CreateOrUpdateDeployment(obj, d)
	if err != nil {
		return controller.RequeueErrorf("error creating or updating discovery service: %v", err)
	}
	// RBAC ensured, reconcile
	_, err = m.deps.TypedControl.CreateOrUpdateService(obj, getTidbDiscoveryService(metaObj, deploy, preferIPv6))
	if err != nil {
		return controller.RequeueErrorf("error creating or updating discovery service: %v", err)
	}
	return nil
}

func getTidbDiscoveryService(obj metav1.Object, deploy *appsv1.Deployment, preferIPv6 bool) *corev1.Service {
	meta, _ := getDiscoveryMeta(obj, controller.DiscoveryMemberName)
	svc := &corev1.Service{
		ObjectMeta: meta,
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "discovery",
					Port:       10261,
					TargetPort: intstr.FromInt(10261),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "proxy",
					Port:       10262,
					TargetPort: intstr.FromInt(10262),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Selector: deploy.Spec.Template.Labels,
		},
	}
	if preferIPv6 {
		SetServiceWhenPreferIPv6(svc)
	}
	return svc
}

func (m *realTidbDiscoveryManager) getTidbDiscoveryDeployment(obj metav1.Object) (*appsv1.Deployment, error) {
	var (
		resources corev1.ResourceRequirements
		timezone  string
		baseSpec  v1alpha1.ComponentAccessor
		podSpec   corev1.PodSpec
	)

	switch cluster := obj.(type) {
	case *v1alpha1.TidbCluster:
		resources = cluster.Spec.Discovery.ResourceRequirements
		timezone = cluster.Timezone()
		baseSpec = cluster.BaseDiscoverySpec()
		podSpec = baseSpec.BuildPodSpec()
	case *v1alpha1.DMCluster:
		resources = cluster.Spec.Discovery.ResourceRequirements
		timezone = cluster.Timezone()
		baseSpec = cluster.BaseDiscoverySpec()
		podSpec = baseSpec.BuildPodSpec()
	default:
		panic(fmt.Sprintf("unsupported type %T for discovery meta", obj))
	}

	meta, l := getDiscoveryMeta(obj, controller.DiscoveryMemberName)

	envs := []corev1.EnvVar{
		{
			Name: "MY_POD_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		{
			Name:  "TZ",
			Value: timezone,
		},
		{
			Name:  "TC_NAME",
			Value: obj.GetName(), // for DmCluster, we still name it as TC_NAME because only ProxyServer use it now.
		},
	}
	envs = util.AppendEnv(envs, baseSpec.Env())
	volMounts := []corev1.VolumeMount{}
	volMounts = append(volMounts, baseSpec.AdditionalVolumeMounts()...)
	podSpec.Containers = append(podSpec.Containers, corev1.Container{
		Name:      "discovery",
		Resources: controller.ContainerResource(resources),
		Command: []string{
			"/usr/local/bin/tidb-discovery",
		},
		Image:           m.deps.CLIConfig.TiDBDiscoveryImage,
		ImagePullPolicy: baseSpec.ImagePullPolicy(),
		Env:             envs,
		EnvFrom:         baseSpec.EnvFrom(),
		VolumeMounts:    volMounts,
		Ports: []corev1.ContainerPort{
			{
				Name:          "discovery",
				Protocol:      corev1.ProtocolTCP,
				ContainerPort: 10261,
			},
			{
				Name:          "proxy",
				Protocol:      corev1.ProtocolTCP,
				ContainerPort: 10262,
			},
		},
	})

	var err error
	podSpec.Containers, err = MergePatchContainers(podSpec.Containers, baseSpec.AdditionalContainers())
	if err != nil {
		return nil, fmt.Errorf("failed to merge containers spec for Discovery of [%s/%s], error: %v", meta.Namespace, meta.Name, err)
	}

	podSpec.InitContainers = append(podSpec.InitContainers, baseSpec.InitContainers()...)

	podSpec.ServiceAccountName = meta.Name

	podSpec.Volumes = append(podSpec.Volumes, baseSpec.AdditionalVolumes()...)
	if tc, ok := obj.(*v1alpha1.TidbCluster); ok && tc.IsTLSClusterEnabled() && !tc.WithoutLocalPD() {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: "pd-tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: util.ClusterTLSSecretName(obj.GetName(), label.PDLabelVal),
				},
			},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      "pd-tls",
			ReadOnly:  true,
			MountPath: PdTlsCertPath,
		})
		podSpec.Containers[0].Env = append(podSpec.Containers[0].Env, corev1.EnvVar{
			Name:  "TC_TLS_ENABLED",
			Value: strconv.FormatBool(true),
		})
	}

	podLabels := util.CombineStringMap(l.Labels(), baseSpec.Labels())
	podAnnotations := baseSpec.Annotations()
	d := &appsv1.Deployment{
		ObjectMeta: meta,
		Spec: appsv1.DeploymentSpec{
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Replicas: pointer.Int32Ptr(1),
			Selector: l.LabelSelector(),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: podSpec,
			},
		},
	}

	b, err := json.Marshal(d.Spec.Template.Spec)
	if err != nil {
		return nil, err
	}
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations[controller.LastAppliedPodTemplate] = string(b)

	return d, nil
}

func getDiscoveryMeta(obj metav1.Object, nameFunc func(string) string) (metav1.ObjectMeta, label.Label) {
	var (
		name           string
		ownerRef       metav1.OwnerReference
		discoveryLabel label.Label
	)

	switch cluster := obj.(type) {
	case *v1alpha1.TidbCluster:
		name = cluster.GetName()
		instanceName := cluster.GetInstanceName()
		ownerRef = controller.GetOwnerRef(cluster)
		discoveryLabel = label.New().Instance(instanceName).Discovery()
	case *v1alpha1.DMCluster:
		// NOTE: for DmCluster, add a `-dm` prefix for discovery to avoid name conflicts.
		name = fmt.Sprintf("%s-dm", cluster.GetName())
		instanceName := fmt.Sprintf("%s-dm", cluster.GetInstanceName())
		ownerRef = controller.GetDMOwnerRef(cluster) // TODO: refactor to unify methods
		discoveryLabel = label.NewDM().Instance(instanceName).Discovery()
	default:
		panic(fmt.Sprintf("unsupported type %T for discovery meta", obj))
	}

	objMeta := metav1.ObjectMeta{
		Name:            nameFunc(name),
		Namespace:       obj.GetNamespace(),
		Labels:          discoveryLabel,
		OwnerReferences: []metav1.OwnerReference{ownerRef},
	}
	return objMeta, discoveryLabel
}

type FakeDiscoveryManager struct {
	err error
}

func NewFakeDiscoveryManger() *FakeDiscoveryManager {
	return &FakeDiscoveryManager{}
}

func (m *FakeDiscoveryManager) SetReconcileError(err error) {
	m.err = err
}

func (m *FakeDiscoveryManager) Reconcile(_ client.Object) error {
	if m.err != nil {
		return m.err
	}
	return nil
}
