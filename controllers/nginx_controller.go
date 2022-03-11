/*
Copyright 2022.

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

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8saliceydevv1 "github.com/Alicey0719/kubecon-prac/api/v1"
)

// NginxReconciler reconciles a Nginx object
type NginxReconciler struct {
	client.Client                      //apiServerにアクセスするやつ
	Log           logr.Logger          //ロガー
	Scheme        *runtime.Scheme      //runtime.ObjectをGVK,GVRに解決するやつ
	Recorder      record.EventRecorder //Event記録するやつ
}

// +kubebuilder:rbac:groups=k8s.alicey.dev,resources=nginxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.alicey.dev,resources=nginxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8s.alicey.dev,resources=nginxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Nginx object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *NginxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("nginx", req.NamespacedName)
	// TODO(user): your logic here
	//ctx := context.Background() //空のcontext作成
	//log := r.Log.WithValues("nginx", req.NamespacedName)

	// NginxObjの取得
	var nginx k8saliceydevv1.Nginx
	log.Info("fetch Nginx Rresource")
	if err := r.Get(ctx, req.NamespacedName, &nginx); err != nil { //取得
		log.Error(err, "unable to fetch Nginx")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 古いDeploymentの削除
	if err := r.cleanupOwnedResources(ctx, log, &nginx); err != nil {
		log.Error(err, "failed to clean up old Deployment resources for this Nginx")
		return ctrl.Result{}, err
	}

	// Nginxが管理するDeploymentの更新・作成
	deploymentName := nginx.Spec.DeploymentName //DeploymentNameの取得
	deploy := &appsv1.Deployment{               //DeploymentTempの作成
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: req.Namespace,
		},
	}
	// Create or Update
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		// get replicas
		replicas := int32(1)
		if nginx.Spec.Replicas != nil {
			replicas = *nginx.Spec.Replicas
		}
		deploy.Spec.Replicas = &replicas

		// labelsの定義
		labels := map[string]string{
			"app":        "nginx",
			"controller": req.Name,
		}
		if deploy.Spec.Selector == nil {
			deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		}
		if deploy.Spec.Template.ObjectMeta.Labels == nil {
			deploy.Spec.Template.ObjectMeta.Labels = labels
		}

		// containersの定義
		containers := []corev1.Container{
			{
				Name:  "nginx",
				Image: "nginx:latest",
			},
		}
		if deploy.Spec.Template.Spec.Containers == nil {
			deploy.Spec.Template.Spec.Containers = containers
		}

		// NginxがDeploymentを管理していることを示すOwnerReferenceをMetadataに登録
		if err := ctrl.SetControllerReference(&nginx, deploy, r.Scheme); err != nil {
			log.Error(err, "unable to set ownerReference from Nginx to Deployment")
			return err
		}

		return nil
	}); err != nil {
		// エラーハンドリング
		log.Error(err, "unable to ensure deployment is correct")
		return ctrl.Result{}, err
	}

	// −ステータスの更新(AvailableReplicas)
	var deployment appsv1.Deployment
	var deploymentNamespacedName = client.ObjectKey{Namespace: req.Namespace, Name: nginx.Spec.DeploymentName} //取得するDeploymentObjのNamespaceとNameの絞り込み
	if err := r.Get(ctx, deploymentNamespacedName, &deployment); err != nil {                                  // 対象のDevObjをin-memory-cacheから取得
		log.Error(err, "unable to fetch Deployment")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	//availableReplicasとnginx.Status.AvailableReplicasが一致しなければ更新
	availableReplicas := deployment.Status.AvailableReplicas
	if availableReplicas == nginx.Status.AvailableReplicas {
		return ctrl.Result{}, nil
	}
	nginx.Status.AvailableReplicas = availableReplicas

	// Nginxのステータス更新
	if err := r.Status().Update(ctx, &nginx); err != nil {
		log.Error(err, "unable to update Nginx status")
		return ctrl.Result{}, err
	}

	// ステータスの更新をEventに記録
	r.Recorder.Eventf(&nginx, corev1.EventTypeNormal, "Updated", "Update nginx.status.AvailableReplicas: %d", nginx.Status.AvailableReplicas)

	// end
	return ctrl.Result{}, nil
}

// cleanupOwnedResources
// nginx.spec.deploymentName field.
func (r *NginxReconciler) cleanupOwnedResources(ctx context.Context, log logr.Logger, nginx *k8saliceydevv1.Nginx) error {
	log.Info("finding existing Deployments for Nginx resource")

	// Deployment一覧取得(r.ListでNamespaceでの絞り込み・Nginxが管理しているものに絞り込む)
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(nginx.Namespace), client.MatchingFields(map[string]string{deploymentOwnerKey: nginx.Name})); err != nil {
		return err
	}

	// nginx.spec.deploymentnameとListで取得したDeploymentObjを比較し、一致すれば削除をスキップ
	for _, deployment := range deployments.Items {
		if deployment.Name == nginx.Spec.DeploymentName {
			continue
		}

		// 一致しない古いDeploymentの削除
		if err := r.Delete(ctx, &deployment); err != nil {
			log.Error(err, "failed to delete Deployment resource")
			return err
		}

		// DeploymentのDeletedEventの登録
		log.Info("delete deployment resource: " + deployment.Name)
		r.Recorder.Eventf(nginx, corev1.EventTypeNormal, "Deleted", "Deleted deployment %q", deployment.Name)
	}

	return nil
}

// setupWithMng
var (
	deploymentOwnerKey = ".metadata.controller"
	apiGVStr           = k8saliceydevv1.GroupVersion.String()
)

// SetupWithManager sets up the controller with the Manager.
func (r *NginxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &appsv1.Deployment{}, deploymentOwnerKey, func(rawObj client.Object) []string { //DeploymentOwnerKey(IndexField)の追加
		deployment := rawObj.(*appsv1.Deployment)
		owner := metav1.GetControllerOf(deployment) //DeploymentのOwnerReferenceの取得
		if owner == nil {
			return nil
		}
		//Nginx以外は除外
		if owner.APIVersion != apiGVStr || owner.Kind != "Nginx" {
			return nil
		}

		// DeploymentOwnerKey:owner.Name をDeploymentObjに追加
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	// Watch対象Resourcesの選択
	return ctrl.NewControllerManagedBy(mgr).
		For(&k8saliceydevv1.Nginx{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
