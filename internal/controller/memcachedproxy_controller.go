/*
Copyright 2024.

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

package controller

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "github.com/cloud/memcached-operator/api/v1alpha1"
)

const memcachedproxyFinalizer = "cache.example.com/finalizer"

// Definitions to manage status conditions
const (
	// typeAvailableMemcachedProxy represents the status of the Deployment reconciliation
	typeAvailableMemcachedProxy = "Available"
	// typeDegradedMemcachedProxy represents the status used when the custom resource is deleted and the finalizer operations are must to occur.
	typeDegradedMemcachedProxy = "Degraded"
)

// MemcachedProxyReconciler reconciles a Memcached object
type MemcachedProxyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=cache.example.com,resources=memcacheds,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cache.example.com,resources=memcacheds/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cache.example.com,resources=memcacheds/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// It is essential for the controller's reconciliation loop to be idempotent. By following the Operator
// pattern you will create Controllers which provide a reconcile function
// responsible for synchronizing resources until the desired state is reached on the cluster.
// Breaking this recommendation goes against the design principles of controller-runtime.
// and may lead to unforeseen consequences such as resources becoming stuck and requiring manual intervention.
// For further info:
// - About Operator Pattern: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
// - About Controllers: https://kubernetes.io/docs/concepts/architecture/controller/
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.3/pkg/reconcile
func (r *MemcachedProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Memcached instance
	// The purpose is check if the Custom Resource for the Kind Memcached
	// is applied on the cluster if not we return nil to stop the reconciliation
	memcachedproxy := &cachev1alpha1.MemcachedProxy{}
	err := r.Get(ctx, req.NamespacedName, memcachedproxy)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("memcachedproxy resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get memcachedproxy")
		return ctrl.Result{}, err
	}

	// Let's just set the status as Unknown when no status are available
	if memcachedproxy.Status.Conditions == nil || len(memcachedproxy.Status.Conditions) == 0 {
		meta.SetStatusCondition(&memcachedproxy.Status.Conditions, metav1.Condition{Type: typeAvailableMemcachedProxy, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
		if err = r.Status().Update(ctx, memcachedproxy); err != nil {
			log.Error(err, "Failed to update MemcachedProxy status 1")
			return ctrl.Result{}, err
		}

		// Let's re-fetch the memcachedproxy Custom Resource after update the status
		// so that we have the latest state of the resource on the cluster and we will avoid
		// raise the issue "the object has been modified, please apply
		// your changes to the latest version and try again" which would re-trigger the reconciliation
		// if we try to update it again in the following operations
		if err := r.Get(ctx, req.NamespacedName, memcachedproxy); err != nil {
			log.Error(err, "Failed to re-fetch memcachedproxy")
			return ctrl.Result{}, err
		}
	}

	// Let's add a finalizer. Then, we can define some operations which should
	// occurs before the custom resource to be deleted.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers
	if !controllerutil.ContainsFinalizer(memcachedproxy, memcachedproxyFinalizer) {
		log.Info("Adding Finalizer for Memcached")
		if ok := controllerutil.AddFinalizer(memcachedproxy, memcachedproxyFinalizer); !ok {
			log.Error(err, "Failed to add finalizer into the custom resource")
			return ctrl.Result{Requeue: true}, nil
		}

		if err = r.Update(ctx, memcachedproxy); err != nil {
			log.Error(err, "Failed to update custom resource to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Check if the Memcached instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isMemcachedProxyMarkedToBeDeleted := memcachedproxy.GetDeletionTimestamp() != nil
	if isMemcachedProxyMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(memcachedproxy, memcachedproxyFinalizer) {
			log.Info("Performing Finalizer Operations for Memcached before delete CR")

			// Let's add here an status "Downgrade" to define that this resource begin its process to be terminated.
			meta.SetStatusCondition(&memcachedproxy.Status.Conditions, metav1.Condition{Type: typeDegradedMemcachedProxy,
				Status: metav1.ConditionUnknown, Reason: "Finalizing",
				Message: fmt.Sprintf("Performing finalizer operations for the custom resource: %s ", memcachedproxy.Name)})

			if err := r.Status().Update(ctx, memcachedproxy); err != nil {
				log.Error(err, "Failed to update MemcachedProxy status 2")
				return ctrl.Result{}, err
			}

			// Perform all operations required before remove the finalizer and allow
			// the Kubernetes API to remove the custom resource.
			r.doFinalizerOperationsForMemcachedProxy(memcachedproxy)

			// TODO(user): If you add operations to the doFinalizerOperationsForMemcachedProxy method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

			// Re-fetch the memcachedproxy Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, memcachedproxy); err != nil {
				log.Error(err, "Failed to re-fetch memcachedproxy")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(&memcachedproxy.Status.Conditions, metav1.Condition{Type: typeDegradedMemcachedProxy,
				Status: metav1.ConditionTrue, Reason: "Finalizing",
				Message: fmt.Sprintf("Finalizer operations for custom resource %s name were successfully accomplished", memcachedproxy.Name)})

			if err := r.Status().Update(ctx, memcachedproxy); err != nil {
				log.Error(err, "Failed to update MemcachedProxy status 3")
				return ctrl.Result{}, err
			}

			log.Info("Removing Finalizer for Memcached after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(memcachedproxy, memcachedproxyFinalizer); !ok {
				log.Error(err, "Failed to remove finalizer for Memcached")
				return ctrl.Result{Requeue: true}, nil
			}

			if err := r.Update(ctx, memcachedproxy); err != nil {
				log.Error(err, "Failed to remove finalizer for Memcached")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Check if the deployment already exists, if not create a new one
	found := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: memcachedproxy.Name, Namespace: memcachedproxy.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new deployment
		dep, err := r.deploymentForMemcachedProxy(memcachedproxy)
		if err != nil {
			log.Error(err, "Failed to define new Deployment resource for Memcached")

			// The following implementation will update the status
			meta.SetStatusCondition(&memcachedproxy.Status.Conditions, metav1.Condition{Type: typeAvailableMemcachedProxy,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create Deployment for the custom resource (%s): (%s)", memcachedproxy.Name, err)})

			if err := r.Status().Update(ctx, memcachedproxy); err != nil {
				log.Error(err, "Failed to update MemcachedProxy status 4")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new Deployment",
			"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
		if err = r.Create(ctx, dep); err != nil {
			log.Error(err, "Failed to create new Deployment",
				"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
			return ctrl.Result{}, err
		}

		// Deployment created successfully
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// The CRD API is defining that the Memcached type, have a MemcachedSpec.Size field
	// to set the quantity of Deployment instances is the desired state on the cluster.
	// Therefore, the following code will ensure the Deployment size is the same as defined
	// via the Size spec of the Custom Resource which we are reconciling.
	size := memcachedproxy.Spec.Size
	if *found.Spec.Replicas != size {
		found.Spec.Replicas = &size
		if err = r.Update(ctx, found); err != nil {
			log.Error(err, "Failed to update Deployment",
				"Deployment.Namespace", found.Namespace, "Deployment.Name", found.Name)

			// Re-fetch the memcachedproxy Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, memcachedproxy); err != nil {
				log.Error(err, "Failed to re-fetch memcachedproxy")
				return ctrl.Result{}, err
			}

			// The following implementation will update the status
			meta.SetStatusCondition(&memcachedproxy.Status.Conditions, metav1.Condition{Type: typeAvailableMemcachedProxy,
				Status: metav1.ConditionFalse, Reason: "Resizing",
				Message: fmt.Sprintf("Failed to update the size for the custom resource (%s): (%s)", memcachedproxy.Name, err)})

			if err := r.Status().Update(ctx, memcachedproxy); err != nil {
				log.Error(err, "Failed to update MemcachedProxy status 5")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		// Now, that we update the size we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
	}

	// Check if the Service already exists, if not create a new one
	// NOTE: The Service is used to expose the Deployment. However, the Service is not required at all for the memcached example to work. The purpose is to add more examples of what you can do in your operator project.
	service := &corev1.Service{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: memcachedproxy.Name, Namespace: memcachedproxy.Namespace}, service)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new Service object
		svc := r.serviceForMemcachedProxy(memcachedproxy)
		log.Info("Creating a new Service.", "Service.Namespace", svc.Namespace, "Service.Name", svc.Name)
		err = r.Client.Create(context.TODO(), svc)
		if err != nil {
			log.Error(err, "Failed to create new Service.", "Service.Namespace", svc.Namespace, "Service.Name", svc.Name)
			return ctrl.Result{}, err
		}
	} else if err != nil {
		log.Error(err, "Failed to get Service.")
		return ctrl.Result{}, err
	}
	// The following implementation will update the status
	meta.SetStatusCondition(&memcachedproxy.Status.Conditions, metav1.Condition{Type: typeAvailableMemcachedProxy,
		Status: metav1.ConditionTrue, Reason: "Reconciling",
		Message: fmt.Sprintf("Deployment for custom resource (%s) with %d replicas created successfully", memcachedproxy.Name, size)})

	if err := r.Status().Update(ctx, memcachedproxy); err != nil {
		log.Error(err, "Failed to update MemcachedProxy status 6")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// finalizeMemcached will perform the required operations before delete the CR.
func (r *MemcachedProxyReconciler) doFinalizerOperationsForMemcachedProxy(cr *cachev1alpha1.MemcachedProxy) {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as depended of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// The following implementation will raise an event
	r.Recorder.Event(cr, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			cr.Name,
			cr.Namespace))
}

// deploymentForMemcachedProxy returns a Memcached Deployment object
func (r *MemcachedProxyReconciler) deploymentForMemcachedProxy(
	memcachedproxy *cachev1alpha1.MemcachedProxy) (*appsv1.Deployment, error) {
	ls := labelsForMemcachedProxy(memcachedproxy, memcachedproxy.Name)
	replicas := memcachedproxy.Spec.Size

	// Get the Operand image
	image, err := imageForMemcachedProxy(memcachedproxy)
	if err != nil {
		return nil, err
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      memcachedproxy.Name,
			Namespace: memcachedproxy.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					// TODO(user): Uncomment the following code to configure the nodeAffinity expression
					// according to the platforms which are supported by your solution. It is considered
					// best practice to support multiple architectures. build your manager image using the
					// makefile target docker-buildx. Also, you can use docker manifest inspect <image>
					// to check what are the platforms supported.
					// More info: https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#node-affinity
					//Affinity: &corev1.Affinity{
					//	NodeAffinity: &corev1.NodeAffinity{
					//		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					//			NodeSelectorTerms: []corev1.NodeSelectorTerm{
					//				{
					//					MatchExpressions: []corev1.NodeSelectorRequirement{
					//						{
					//							Key:      "kubernetes.io/arch",
					//							Operator: "In",
					//							Values:   []string{"amd64", "arm64", "ppc64le", "s390x"},
					//						},
					//						{
					//							Key:      "kubernetes.io/os",
					//							Operator: "In",
					//							Values:   []string{"linux"},
					//						},
					//					},
					//				},
					//			},
					//		},
					//	},
					//},
					//SecurityContext: &corev1.PodSecurityContext{
					//	RunAsNonRoot: &[]bool{true}[0],
					//	SeccompProfile: &corev1.SeccompProfile{
					//		Type: corev1.SeccompProfileTypeRuntimeDefault,
					//	},
					//},
					Containers: []corev1.Container{{
						Image:           image,
						Name:            "memcachedproxy",
						ImagePullPolicy: corev1.PullIfNotPresent,
						// Ensure restrictive context for the container
						// More info: https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted
						//SecurityContext: &corev1.SecurityContext{
						//	RunAsNonRoot: &[]bool{true}[0],
						//	RunAsUser:                &[]int64{1001}[0],
						//	AllowPrivilegeEscalation: &[]bool{false}[0],
						//	Capabilities: &corev1.Capabilities{
						//		Drop: []corev1.Capability{
						//			"ALL",
						//		},
						//	},
						//},
						Ports: []corev1.ContainerPort{{
							ContainerPort: memcachedproxy.Spec.ContainerPort,
							Name:          "memcachedproxy",
						}},
						//Command: []string{"mcrouter", "-m=64", "-o", "modern", "-v"},
						Command: []string{"mcrouter"},
						Args: []string{
							"-p", fmt.Sprint(memcachedproxy.Spec.ContainerPort),
							fmt.Sprint("--config-str=", getpoolSetup(memcachedproxy)),
						},
					}},
				},
			},
		},
	}

	// Set the ownerRef for the Deployment
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(memcachedproxy, dep, r.Scheme); err != nil {
		return nil, err
	}
	return dep, nil
}

// memcachedPodNames
func getMemcachedPodName(memcachedproxy *cachev1alpha1.MemcachedProxy) string {
	memcachedService := memcachedproxy.Spec.MemcachedSts
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}

	// 获取指定命名空间和名称的 StatefulSet
	sts, _ := generateK8sDynamicClient().Resource(gvr).Namespace(memcachedproxy.Namespace).Get(context.TODO(), memcachedService, metav1.GetOptions{})

	// 提取服务名称
	serviceName, _, err := unstructured.NestedString(sts.Object, "spec", "serviceName")
	//unreadyServiceName := serviceName + "-unready"
	if err != nil {
		log.Log.Info("failed to get replicas: %v", err)
	}
	// 提取副本数
	replicas, _, _ := unstructured.NestedInt64(sts.Object, "spec", "replicas")

	var domains []string

	memcachedPort := strconv.Itoa(11211)
	for i := 0; i < int(replicas); i++ {
		podName := string(serviceName) + "-" + strconv.Itoa(i)
		domain := fmt.Sprintf("\"%s.%s:%s\"", podName, serviceName, memcachedPort)
		domains = append(domains, domain)
	}
	result := fmt.Sprint(strings.Join(domains, ","))

	return result
}

// pool setup
func getpoolSetup(memcachedproxy *cachev1alpha1.MemcachedProxy) string {
	poolSetup := memcachedproxy.Spec.PoolSetup
	memcachedPoolStr := getMemcachedPodName(memcachedproxy)

	if poolSetup == "sharded" {
		return fmt.Sprintf(`{"pools":{"A":{"servers":[%s]}},"route":"PoolRoute|A"}`, memcachedPoolStr)
	} else if poolSetup == "replicated" {
		return fmt.Sprintf(`{"pools":{"A":{"servers":["%s"]}},"route":{"type":"OperationSelectorRoute","operation_policies":{"add":"AllSyncRoute|Pool|A","delete":"AllSyncRoute|Pool|A","get":"RandomRoute|Pool|A","set":"AllSyncRoute|Pool|A"}}`, memcachedPoolStr)
	} else {

		return ""
	}
}

// serviceForMemcached 创建与 Memcached 关联的 Service
func (r *MemcachedProxyReconciler) serviceForMemcachedProxy(memcachedproxy *cachev1alpha1.MemcachedProxy) *corev1.Service {
	// 定义 Service 的配置
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      memcachedproxy.Name,
			Namespace: memcachedproxy.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: labelsForMemcachedProxy(memcachedproxy, memcachedproxy.Name),
			Ports: []corev1.ServicePort{
				{
					Port:     memcachedproxy.Spec.ContainerPort,
					Protocol: corev1.ProtocolTCP,
					Name:     memcachedproxy.Name,
				},
			},
		},
	}
	// Set Memcached instance as the owner of the Service.
	ctrl.SetControllerReference(memcachedproxy, svc, r.Scheme) //todo check how to get the schema
	return svc
}

// labelsForMemcachedProxy returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForMemcachedProxy(memcachedproxy *cachev1alpha1.MemcachedProxy, name string) map[string]string {
	var imageTag string
	image, err := imageForMemcachedProxy(memcachedproxy)
	if err == nil {
		imageTag = strings.Split(image, ":")[1]
	}
	return map[string]string{"app.kubernetes.io/name": "Memcached",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/version":    imageTag,
		"app.kubernetes.io/part-of":    "memcached-operator",
		"app.kubernetes.io/created-by": "controller-manager",
	}
}

// imageForMemcached gets the Operand image which is managed by this controller
// from the MEMCACHED_IMAGE environment variable defined in the config/manager/manager.yaml
func imageForMemcachedProxy(memcachedproxy *cachev1alpha1.MemcachedProxy) (string, error) {

	image := memcachedproxy.Spec.Image

	return image, nil
}

// SetupWithManager sets up the controller with the Manager.
// Note that the Deployment will be also watched in order to ensure its
// desirable state on the cluster
func (r *MemcachedProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.MemcachedProxy{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
