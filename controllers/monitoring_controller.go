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
	errorsGo "errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rtv1alpha1 "github.com/francescol96/monitorrt/api/v1alpha1"
)

// MonitoringReconciler reconciles a Monitoring object
type MonitoringReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// RealTimeWCET is part of RealTimeData Struct
type RealTimeWCET struct {
	Node   string
	RTWcet int
}

// RealTimeData is a struct to extract data from the RealTime scheduling CRD
type RealTimeData struct {
	appName     string
	Criticality string
	RTDeadline  int
	RTPeriod    int
	RTWcets     []RealTimeWCET
}

// Map that contains the as key the name of the node, and as value the time left before removing the taint
// The value is encoded as "value * polling_rate" seconds
var Timers = make(map[string]int)

// The polling rate to remove the taint
const polling_rate = 30

//+kubebuilder:rbac:groups=rt.francescol96.univr,resources=monitorings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rt.francescol96.univr,resources=monitorings/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=rt.francescol96.univr,resources=monitorings/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=nodes/status,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=scheduling.francescol96.univr,resources=realtimes,verbs=get;list

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Monitoring object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *MonitoringReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	defer duration(track("Reconcile"))
	logger := log.Log.WithValues("Moniroting/rt", req.NamespacedName)
	logger.Info("Moniroting/rt Reconcile method")

	rt := &rtv1alpha1.Monitoring{}

	// Verify if monitoring object still exists
	err := r.Get(ctx, req.NamespacedName, rt)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Moniroting/rt resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Moniroting/rt instance")
		return ctrl.Result{}, err
	}

	// Check if node specified in monitoring object exists
	foundNode := &corev1.Node{}
	logger.Info("Checking if node exists:", "Node", rt.Spec.Node)
	err = r.Get(ctx, types.NamespacedName{Name: rt.Spec.Node}, foundNode)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Checking if node exists: Node not found. Ignoring..")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get node instance for comparison with RT monitoring")
		return ctrl.Result{}, err
	}

	// Check if pod specified in monitoring object exists
	podList := &corev1.PodList{}
	logger.Info("Checking if pod exists:", "Pod", rt.Spec.PodName)
	opts := []client.ListOption{
		client.InNamespace("default"),
	}
	err = r.List(ctx, podList, opts...)
	if err != nil {
		if podList.Size() == 0 {
			logger.Error(err, "Checking if pod exists: empty PodList")
			return ctrl.Result{}, err
		}
		logger.Error(err, "Failed to get PodList instance for comparison with RT monitoring")
		return ctrl.Result{}, err
	}

	foundPod := -1
	for i, pod := range podList.Items {
		if pod.Name == rt.Spec.PodName {
			foundPod = i
			break
		}
	}

	if foundPod == -1 || podList.Items[foundPod].Name != rt.Spec.PodName {
		logger.Info("Checking if pod exists: Pod not found. Ignoring...")
		return ctrl.Result{}, nil
	}

	// The pod and node exist, check if req missedDeadlinesPeriod are higher than VALUE
	if rt.Spec.MissedDeadlinesPeriod > 10 {
		logger.Info("Deleting pod: too many missed RT deadlines", "MissedDeadlinesPeriod", rt.Spec.MissedDeadlinesPeriod)

		// Taint the node so that no other pod can be scheduled on it
		taintExists := false
		for _, taint := range foundNode.Spec.Taints {
			if taint.Key == "RTDeadlinePressure" {
				taintExists = true
			}
		}
		if taintExists {
			logger.Info("Node already tainted with RTDeadlinePressure:noSchedule, updating timer")
			Timers[foundNode.Name]++
		} else {
			foundNode.Spec.Taints = append(foundNode.Spec.Taints, corev1.Taint{
				Key:    "RTDeadlinePressure",
				Value:  "True",
				Effect: corev1.TaintEffectNoSchedule,
			})
			logger.Info("Tainting node with RTDeadlinePressure:noSchedule")
			err = r.Update(ctx, foundNode)
			if err != nil {
				logger.Error(err, "Error while tainting the node")
				return ctrl.Result{}, err
			}
			Timers[foundNode.Name] = 1
		}

		// Delete the victim pod with some policy # selectPodVictimForDeletion(rt, podList)
		// Delete the current pod
		logger.Info("Deleting Pod")
		err = r.Delete(ctx, selectPodVictimForDeletion(rt, podList))
		if err != nil {
			logger.Error(err, "Error while deleting pod", "Pod", podList.Items[foundPod].Name)
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// Insert your policy for eviction here
func selectPodVictimForDeletion(rt *rtv1alpha1.Monitoring, podList *corev1.PodList) *corev1.Pod {
	for _, pod := range podList.Items {
		realTimeData, err := GetRealTimeData(context.TODO())
		if err != nil {
			log.Log.Error(err, "could not obtain RT data")
		} else {
			for _, rtItem := range realTimeData {
				if pod.Labels["scheduling.francescol96.univr"] == rtItem.appName {
					log.Log.Info("-----> rtItem found", "appName", rtItem.appName)
					// Do something with RT object and Pod list
				}
			}
		}
		if pod.Name == rt.Spec.PodName {
			return &pod
		}
	}
	return &corev1.Pod{}
}

// SetupWithManager sets up the controller with the Manager.
func (r *MonitoringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rtv1alpha1.Monitoring{}).
		Complete(r)
}

func (r *MonitoringReconciler) StartTaintThread() {
	go func() {
		logger := log.Log.WithValues("Moniroting/rt.TaintMonitoringThread", "Taint")
		logger.Info("Starting taint monitoring thread")
		for {
			time.Sleep(time.Duration(polling_rate) * time.Second)
			logger.Info("Taint Thread: Waking up, working...", "len(Timers)", len(Timers))
			for nodeName, timer := range Timers {
				if timer <= 0 {
					node := &corev1.Node{}
					err := r.Get(context.TODO(), types.NamespacedName{Name: nodeName}, node)
					if err != nil {
						if errors.IsNotFound(err) {
							logger.Error(err, "Taint Thread: node not found, ignoring...")
							continue
						}
						logger.Error(err, "Taint Thread: failed to get node instance")
						continue
					}
					for i, taint := range node.Spec.Taints {
						if taint.Key == "RTDeadlinePressure" {
							// assign last element to RTDeadlinePressure position
							node.Spec.Taints[i] = node.Spec.Taints[len(node.Spec.Taints)-1]
							// Update array without last element
							node.Spec.Taints = node.Spec.Taints[:len(node.Spec.Taints)-1]
						}
						logger.Info("Taint Thread: untaining node", "node", nodeName)
						err = r.Update(context.TODO(), node)
						if err != nil {
							logger.Error(err, "Taint Thread: error while un-tainting the node")
						}
					}
					delete(Timers, nodeName)
				} else {
					logger.Info("Decrementing timer", nodeName, Timers[nodeName])
					Timers[nodeName]--
				}
			}
		}
	}()
}

func GetRealTimeData(ctx context.Context) ([]RealTimeData, error) {
	resultErr := []RealTimeData{{Criticality: "N"}}
	var result []RealTimeData
	config, err := rest.InClusterConfig()
	if err != nil {
		return resultErr, errorsGo.New("could not obtain incluster config")
	}
	dynamic := dynamic.NewForConfigOrDie(config)
	items, err := GetResourcesDynamically(dynamic, ctx, "scheduling.francescol96.univr", "v1", "realtimes", "default")

	if err != nil {
		return resultErr, err
	} else {
		for _, item := range items {
			typedData := RealTimeData{}
			appName, appNameFound, appNameErr := unstructured.NestedString(item.Object, "metadata", "name")
			criticality, criticalityFound, criticalityErr := unstructured.NestedString(item.Object, "spec", "criticality")
			rtDeadline, rtDeadlineFound, rtDeadlineErr := unstructured.NestedInt64(item.Object, "spec", "rtDeadline")
			rtPeriod, rtPeriodFound, rtPeriodErr := unstructured.NestedInt64(item.Object, "spec", "rtPeriod")
			if appNameFound && appNameErr == nil {
				typedData.appName = appName
			} else {
				return resultErr, appNameErr
			}
			if criticalityFound && criticalityErr == nil {
				typedData.Criticality = criticality
			} else {
				return resultErr, criticalityErr
			}

			if rtDeadlineFound && rtDeadlineErr == nil {
				typedData.RTDeadline = int(rtDeadline)
			} else {
				return resultErr, rtDeadlineErr
			}

			if rtPeriodFound && rtPeriodErr == nil {
				typedData.RTPeriod = int(rtPeriod)
			} else {
				return resultErr, rtPeriodErr
			}

			rtWcets, rtWcetsFound, rtWcetsErr := unstructured.NestedSlice(item.Object, "spec", "rtWcets")
			if rtWcetsFound && rtWcetsErr == nil {
				rtWcetsArray := []RealTimeWCET{}
				for _, rtWcet := range rtWcets {
					mapRTWcet, ok := rtWcet.(map[string]interface{})
					if !ok {
						return resultErr, errorsGo.New("unable to obtain map from rtWcet object")
					}
					rtWcetsArray = append(rtWcetsArray, RealTimeWCET{Node: mapRTWcet["node"].(string), RTWcet: int(mapRTWcet["rtWcet"].(int64))})
				}
				typedData.RTWcets = append(typedData.RTWcets, rtWcetsArray...)
			} else {
				return resultErr, rtWcetsErr
			}
			result = append(result, typedData)
		}
	}
	return result, nil
}

func GetResourcesDynamically(dynamic dynamic.Interface, ctx context.Context, group string, version string, resource string, namespace string) ([]unstructured.Unstructured, error) {
	resourceId := schema.GroupVersionResource{
		Group:    group,
		Version:  version,
		Resource: resource,
	}
	list, err := dynamic.Resource(resourceId).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func track(msg string) (string, time.Time) {
	return msg, time.Now()
}

func duration(msg string, start time.Time) {
	log.Log.Info("Time", msg, time.Since(start))
}
