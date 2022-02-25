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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	webserverv1 "kubecom-prac.k8s.alicey.dev/api/v1"
)

// NginxReconciler reconciles a Nginx object
type NginxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=webserver.kubecom-prac.k8s.alicey.dev,resources=nginxes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=webserver.kubecom-prac.k8s.alicey.dev,resources=nginxes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=webserver.kubecom-prac.k8s.alicey.dev,resources=nginxes/finalizers,verbs=update

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
	_ = log.FromContext(ctx)

	// TODO(user): your logic here
	if err := r.Get(ctx, req.NamespacedName, &nginx); err != nil{
		log.Error(err, "unable to fetch Nginx")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.cleanupOwnedResources(ctx, log, &nginx); err != nil {
		log.Error(err, "failed to clean up old Deployment resources for this Nginx")
		return ctrl.Result{}, err
	}

	deploymentName := nginx.Spec.DeploymentName
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: req.Namespace,
		},
	}

	// deploymerntの新規作成・更新
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		// set the replicas from nginx.Spec
		replicas := int32(1)
		if nginx.Spec.Replicas != nil {
			replicas = *nginx.Spec.Replicas
		}
		deploy.Spec.Replicas = &replicas

		// set a label for our deployment
		labels := map[string]string{
			"app":        "nginx",
			"controller": req.Name,
		}

		// set labels to spec.selector for our deployment
		if deploy.Spec.Selector == nil {
			deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		}

		// set labels to template.objectMeta for our deployment
		if deploy.Spec.Template.ObjectMeta.Labels == nil {
			deploy.Spec.Template.ObjectMeta.Labels = labels
		}

		// set a container for our deployment
		containers := []corev1.Container{
			{
				Name:  "nginx",
				Image: "nginx:latest",
			},
		}

		// set containers to template.spec.containers for our deployment
		if deploy.Spec.Template.Spec.Containers == nil {
			deploy.Spec.Template.Spec.Containers = containers
		}

		// set the owner so that garbage collection can kicks in
		if err := ctrl.SetControllerReference(&nginx, deploy, r.Scheme); err != nil {
			log.Error(err, "unable to set ownerReference from Nginx to Deployment")
			return err
		}

		// end of ctrl.CreateOrUpdate
		return nil

	}); err != nil {

		// error handling of ctrl.CreateOrUpdate
		log.Error(err, "unable to ensure deployment is correct")
		return ctrl.Result{}, err

	}

	/*
		### 4: Update Nginx status.
		First, we get deployment object from in-memory-cache.
		Second, we get deployment.status.AvailableReplicas in order to update nginx.status.AvailableReplicas.
		Third, we update nginx.status from deployment.status.AvailableReplicas.
		Finally, finish reconcile. and the next reconcile loop would start unless controller process ends.
	*/

	// get deployment object from in-memory-cache
	var deployment appsv1.Deployment
	var deploymentNamespacedName = client.ObjectKey{Namespace: req.Namespace, Name: nginx.Spec.DeploymentName}
	if err := r.Get(ctx, deploymentNamespacedName, &deployment); err != nil {
		log.Error(err, "unable to fetch Deployment")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// set nginx.status.AvailableReplicas from deployment
	availableReplicas := deployment.Status.AvailableReplicas
	if availableReplicas == nginx.Status.AvailableReplicas {
		// if availableReplicas equals availableReplicas, we wouldn't update anything.
		// exit Reconcile func without updating nginx.status
		return ctrl.Result{}, nil
	}
	nginx.Status.AvailableReplicas = availableReplicas

	// update nginx.status
	if err := r.Status().Update(ctx, &nginx); err != nil {
		log.Error(err, "unable to update Nginx status")
		return ctrl.Result{}, err
	}

	// create event for updated nginx.status
	r.Recorder.Eventf(&nginx, corev1.EventTypeNormal, "Updated", "Update nginx.status.AvailableReplicas: %d", nginx.Status.AvailableReplicas)



	return ctrl.Result{}, nil
}


func (r *NginxReconciler) cleanupOwnedResources(ctx context.Context, log logr.Logger, nginx *samplecontrollerv1alpha1.Nginx) error {
	log.Info("finding existing Deployments for Nginx resource")

	// List all deployment resources owned by this Nginx
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(nginx.Namespace), client.MatchingFields(map[string]string{deploymentOwnerKey: nginx.Name})); err != nil {
		return err
	}

	// Delete deployment if the deployment name doesn't match nginx.spec.deploymentName
	for _, deployment := range deployments.Items {
		if deployment.Name == nginx.Spec.DeploymentName {
			// If this deployment's name matches the one on the Nginx resource
			// then do not delete it.
			continue
		}

		// Delete old deployment object which doesn't match nginx.spec.deploymentName
		if err := r.Delete(ctx, &deployment); err != nil {
			log.Error(err, "failed to delete Deployment resource")
			return err
		}

		log.Info("delete deployment resource: " + deployment.Name)
		r.Recorder.Eventf(nginx, corev1.EventTypeNormal, "Deleted", "Deleted deployment %q", deployment.Name)
	}

	return nil
}



// SetupWithManager sets up the controller with the Manager.
func (r *NginxReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// add deploymentOwnerKey index to deployment object which nginx resource owns
	if err := mgr.GetFieldIndexer().IndexField(&appsv1.Deployment{}, deploymentOwnerKey, func(rawObj runtime.Object) []string {
		// grab the deployment object, extract the owner...
		deployment := rawObj.(*appsv1.Deployment)
		owner := metav1.GetControllerOf(deployment)
		if owner == nil {
			return nil
		}
		// ...make sure it's a Nginx...
		if owner.APIVersion != apiGVStr || owner.Kind != "Nginx" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&webserverv1.Nginx{}).
		Complete(r)
}
