/*

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
	predictorv1 "github.com/kserve/modelmesh-serving/apis/serving/v1alpha1"
	inferenceservicev1 "github.com/kserve/modelmesh-serving/apis/serving/v1beta1"
	kservev1beta1 "github.com/kserve/modelmesh-serving/apis/serving/v1beta1"
	routev1 "github.com/openshift/api/route/v1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	authv1 "k8s.io/api/rbac/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// OpenshiftInferenceServiceReconciler holds the controller configuration.
type OpenshiftInferenceServiceReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Log          logr.Logger
	MeshDisabled bool
}

const (
	inferenceServiceDeploymentModeAnnotation      = "serving.kserve.io/deploymentMode"
	inferenceServiceDeploymentModeAnnotationValue = "ModelMesh"
)

func (r *OpenshiftInferenceServiceReconciler) isDeploymentModeForIsvcModelMesh(inferenceservice *inferenceservicev1.InferenceService) bool {
	value, exists := inferenceservice.Annotations[inferenceServiceDeploymentModeAnnotation]
	if exists && value == inferenceServiceDeploymentModeAnnotationValue {
		return true
	}
	return false
}

// Reconcile performs the reconciling of the Openshift objects for a Kubeflow
// InferenceService.
func (r *OpenshiftInferenceServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Initialize logger format
	log := r.Log.WithValues("InferenceService", req.Name, "namespace", req.Namespace)

	// Get the InferenceService object when a reconciliation event is triggered (create,
	// update, delete)
	inferenceservice := &kservev1beta1.InferenceService{}
	err := r.Get(ctx, req.NamespacedName, inferenceservice)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Stop InferenceService reconciliation")
		// InferenceService not found, so we check for any other inference services that might be using Kserve
		// If none are found, we delete the common namespace-scoped resources that were created for Kserve Metrics.
		err1 := r.DeleteKserveMetricsResourcesIfNoKserveIsvcExists(ctx, req, req.Namespace)
		if err1 != nil {
			log.Error(err1, "Unable to clean up resources")
			return ctrl.Result{}, err1
		}
		return ctrl.Result{}, nil
	} else if err != nil {
		log.Error(err, "Unable to fetch the InferenceService")
		return ctrl.Result{}, err
	}

	// Check what deployment mode is used by the InferenceService. We have differing reconciliation logic for Kserve and ModelMesh
	if r.isDeploymentModeForIsvcModelMesh(inferenceservice) {
		log.Info("Reconciling InferenceService for ModelMesh")
		err = r.ReconcileRoute(inferenceservice, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		err = r.ReconcileSA(inferenceservice, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else {
		log.Info("Reconciling InferenceService for Kserve")
		err = r.ReconcileKserveInference(ctx, req, inferenceservice)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenshiftInferenceServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&kservev1beta1.InferenceService{}).
		Owns(&predictorv1.ServingRuntime{}).
		Owns(&corev1.Namespace{}).
		Owns(&routev1.Route{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&authv1.ClusterRoleBinding{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&monitoringv1.ServiceMonitor{}).
		Owns(&monitoringv1.PodMonitor{}).
		Watches(&source.Kind{Type: &predictorv1.ServingRuntime{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				r.Log.Info("Reconcile event triggered by serving runtime: " + o.GetName())
				inferenceServicesList := &inferenceservicev1.InferenceServiceList{}
				opts := []client.ListOption{client.InNamespace(o.GetNamespace())}

				// Todo: Get only Inference Services that are deploying on the specific serving runtime
				err := r.List(context.TODO(), inferenceServicesList, opts...)
				if err != nil {
					r.Log.Info("Error getting list of inference services for namespace")
					return []reconcile.Request{}
				}

				if len(inferenceServicesList.Items) == 0 {
					r.Log.Info("No InferenceServices found for Serving Runtime: " + o.GetName())
					return []reconcile.Request{}
				}

				reconcileRequests := make([]reconcile.Request, 0, len(inferenceServicesList.Items))
				for _, inferenceService := range inferenceServicesList.Items {
					reconcileRequests = append(reconcileRequests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      inferenceService.Name,
							Namespace: inferenceService.Namespace,
						},
					})
				}
				return reconcileRequests
			}))
	err := builder.Complete(r)
	if err != nil {
		return err
	}

	return nil
}
