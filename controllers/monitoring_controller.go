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
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	metrics "k8s.io/metrics/pkg/client/clientset/versioned"
)

// MonitoringReconciler reconciles a Monitoring object
type MonitoringReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	DynamicClient dynamic.Interface
}

// RealTimeWCET is part of RealTimeData Struct
type RealTimeWCET struct {
	Node   string
	RTWcet int
}

// RealTimeData is a struct to extract data from the RealTime scheduling CRD
type RealTimeData struct {
	Criticality string
	RTDeadline  int
	RTPeriod    int
	RTWcets     []RealTimeWCET
}

// Map that contains as key the name of the node, and as value the time left before removing the taint
// The value is encoded as "value * polling_rate" seconds
var Timers = make(map[string]int)

// The polling rate to remove the taint
const polling_rate = 10

//+kubebuilder:rbac:groups=rt.francescol96.univr,resources=monitorings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rt.francescol96.univr,resources=monitorings/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=rt.francescol96.univr,resources=monitorings/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=nodes/status,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list;watch
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
	defer duration(track("Reconcile")) // This call measures the Reconcile run-time
	logger := log.Log.WithValues("Moniroting/rt", req.NamespacedName)
	loggerLowPrio := logger.V(1)  // Debug level
	loggerHighPrio := logger.V(0) // Info level
	loggerLowPrio.Info("Moniroting/rt Reconcile method")

	rt := &rtv1alpha1.Monitoring{}

	// Verify if monitoring object still exists
	err := r.Get(ctx, req.NamespacedName, rt)
	if err != nil {
		if errors.IsNotFound(err) {
			loggerLowPrio.Info("Moniroting/rt resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Moniroting/rt instance")
		return ctrl.Result{}, err
	}

	// Check if node specified in monitoring object exists
	foundNode := &corev1.Node{}
	loggerLowPrio.Info("Checking if node exists:", "Node", rt.Spec.Node)
	err = r.Get(ctx, types.NamespacedName{Name: rt.Spec.Node}, foundNode)
	if err != nil {
		if errors.IsNotFound(err) {
			loggerLowPrio.Info("Checking if node exists: Node not found. Ignoring..")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get node instance for comparison with RT monitoring")
		return ctrl.Result{}, err
	}

	// Check if pod specified in monitoring object exists
	podList := &corev1.PodList{}
	loggerLowPrio.Info("Checking if pod exists:", "Pod", rt.Spec.PodName)
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
		loggerLowPrio.Info("Checking if pod exists: Pod not found. Ignoring...")
		return ctrl.Result{}, nil
	}

	// The pod and node exist, check if req missedDeadlinesPeriod are higher than VALUE
	if rt.Spec.MissedDeadlinesPeriod > 10 {
		loggerLowPrio.Info("Deleting pod: too many missed RT deadlines", "MissedDeadlinesPeriod", rt.Spec.MissedDeadlinesPeriod)

		// Taint the node so that no other pod can be scheduled on it
		taintExists := false
		for _, taint := range foundNode.Spec.Taints {
			if taint.Key == "RTDeadlinePressure" {
				taintExists = true
			}
		}
		if taintExists {
			loggerLowPrio.Info("Node already tainted with RTDeadlinePressure:noSchedule, updating timer")
			Timers[foundNode.Name]++
		} else {
			foundNode.Spec.Taints = append(foundNode.Spec.Taints, corev1.Taint{
				Key:    "RTDeadlinePressure",
				Value:  "True",
				Effect: corev1.TaintEffectNoSchedule,
			})
			loggerLowPrio.Info("Tainting node with RTDeadlinePressure:noSchedule")
			err = r.Update(ctx, foundNode)
			if err != nil {
				logger.Error(err, "Error while tainting the node")
				return ctrl.Result{}, err
			}
			Timers[foundNode.Name] = 1
		}

		// Delete the victim pod with some policy # selectPodVictimForDeletion(rt, podList)
		// Delete the current pod
		victimPod := r.selectPodVictimForDeletion(rt, podList)
		if victimPod == nil {
			loggerHighPrio.Info("No pod can be evicted")
			return ctrl.Result{}, nil
		}
		err = r.Delete(ctx, victimPod)
		loggerHighPrio.Info("Deleting Pod", "Pod", victimPod)
		if err != nil {
			if errors.IsNotFound(err) {
				loggerHighPrio.Info("Pod not found. Ignoring since pod must be deleted")
				return ctrl.Result{}, nil
			}
			logger.Error(err, "Error while deleting pod", "Pod", victimPod)
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// Insert your policy for eviction here
func (r *MonitoringReconciler) selectPodVictimForDeletion(rt *rtv1alpha1.Monitoring, podList *corev1.PodList) *corev1.Pod {
	listMetrics, err := listMetrics()
	if err != nil {
		log.Log.Error(err, "selectPodVictimForDeletion: error retrieving pods metrics")
		return &corev1.Pod{}
	}
	realTimeData, err := r.GetRealTimeData(context.TODO())
	if err != nil {
		log.Log.Error(err, "could not obtain RT data")
	} else {
		max_nonRT := *resource.NewQuantity(0, "DecimalSI")
		max_RT := *resource.NewQuantity(0, "DecimalSI")
		res_nonRT := &corev1.Pod{}
		res_RT := &corev1.Pod{}

		for i, pod := range podList.Items {
			var usagePod resource.Quantity
			if metricsItem, ok := listMetrics[pod.Name]; ok {
				usagePod = metricsItem["cpu"]
			}
			if rtItem, ok := realTimeData[pod.Labels["scheduling.francescol96.univr"]]; ok {
				if rtItem.Criticality != "C" {
					if usagePod.AsDec().Cmp(max_RT.AsDec()) > 0 {
						max_RT = usagePod
						res_RT = &podList.Items[i]
					}
				}
			} else {
				if usagePod.AsDec().Cmp(max_nonRT.AsDec()) > 0 {
					max_nonRT = usagePod
					res_nonRT = &podList.Items[i]
				}
			}
		}
		if max_nonRT.AsDec().Cmp(resource.NewQuantity(0, "DecimalSI").AsDec()) > 0 && res_nonRT != nil {
			return res_nonRT
		} else if max_RT.AsDec().Cmp(resource.NewQuantity(0, "DecimalSI").AsDec()) > 0 && res_RT != nil {
			return res_RT
		}
	}
	return nil
}

// This function is not called, but contains a linearithmic policy
func (r *MonitoringReconciler) selectPodVictimForDeletionLinearithmic(rt *rtv1alpha1.Monitoring, podList *corev1.PodList) *corev1.Pod {
	listMetrics, err := listMetrics()
	if err != nil {
		log.Log.Error(err, "selectPodVictimForDeletion: error retrieving pods metrics")
		return &corev1.Pod{}
	}
	realTimeData, err := r.GetRealTimeData(context.TODO())
	if err != nil {
		log.Log.Error(err, "could not obtain RT data")
	} else {
		// Sort by CPU usage using QuickSort in o(nlog(n))
		sort.SliceStable(podList.Items, func(i, j int) bool {
			podI := podList.Items[i]
			podJ := podList.Items[j]
			var usageI resource.Quantity
			var usageJ resource.Quantity
			var isRealTimeI bool
			var isRealTimeJ bool
			_, isRealTimeI = realTimeData[podI.Labels["scheduling.francescol96.univr"]]
			_, isRealTimeJ = realTimeData[podJ.Labels["scheduling.francescol96.univr"]]

			// If only one of the two containers is RT, then its CPU usage is always "lower" (i.e., we consider non-RT containers first)
			if isRealTimeI && !isRealTimeJ {
				return false
			} else if !isRealTimeI && isRealTimeJ {
				return true
			} else {
				// If they are both RT or neither is, then we compare the actual CPU usage
				if metricsItem, ok := listMetrics[podI.Name]; ok {
					usageI = metricsItem["cpu"]
				} else {
					usageI = *resource.NewQuantity(0, "DecimalSI")
				}
				if metricsItem, ok := listMetrics[podJ.Name]; ok {
					usageJ = metricsItem["cpu"]
				} else {
					usageJ = *resource.NewQuantity(0, "DecimalSI")
				}
				if usageI.AsDec().Cmp(usageJ.AsDec()) < 0 {
					return false
				} else {
					return true
				}
			}
		})
		// Sift through the pods, with the first being the highest CPU usage non RT-container
		for _, pod := range podList.Items {
			if rtItem, ok := realTimeData[pod.Labels["scheduling.francescol96.univr"]]; ok {
				log.Log.V(1).Info("RealTimeData found", "pod", pod.Name, "rtItem criticality", rtItem.Criticality)
				// If the node only has RT containers, start evicting RT containers too
				// Only if they are not criticality C
				if rtItem.Criticality != "C" {
					return &pod
				}
			} else {
				return &pod
			}
		}
	}
	return nil
}

func listMetrics() (map[string]corev1.ResourceList, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	mc, err := metrics.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	podMetricses, err := mc.MetricsV1beta1().PodMetricses(metav1.NamespaceDefault).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	result := make(map[string]corev1.ResourceList)
	for _, pod := range podMetricses.Items {
		for _, container := range pod.Containers {
			// We assume there is only one container for each pod
			result[pod.Name] = container.Usage
		}
	}
	return result, nil
}

// Uses the function "GetResourcesDynamically" to obtain the RT objects used for scheduling
// These objects are obtained for the eviction policy
func (r *MonitoringReconciler) GetRealTimeData(ctx context.Context) (map[string]RealTimeData, error) {
	resultErr := make(map[string]RealTimeData)
	result := make(map[string]RealTimeData)

	items, err := r.GetResourcesDynamically(ctx, "scheduling.francescol96.univr", "v1", "realtimes", "default")
	if err != nil {
		return resultErr, err
	} else {
		// For each unstructured item in the list, we get the fields and compile an ad-hoc data strcture manually
		for _, item := range items {
			typedData := RealTimeData{}
			appName, appNameFound, appNameErr := unstructured.NestedString(item.Object, "metadata", "name")
			criticality, criticalityFound, criticalityErr := unstructured.NestedString(item.Object, "spec", "criticality")
			rtDeadline, rtDeadlineFound, rtDeadlineErr := unstructured.NestedInt64(item.Object, "spec", "rtDeadline")
			rtPeriod, rtPeriodFound, rtPeriodErr := unstructured.NestedInt64(item.Object, "spec", "rtPeriod")

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
			// As there may be more than one WCET listed in the object, we have to iterate on a list
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
			if appNameFound && appNameErr == nil {
				result[appName] = typedData
			} else {
				return resultErr, appNameErr
			}
		}
	}
	return result, nil
}

// This function obtains untyped resources, such as CRDs defined thrugh a yaml
func (r *MonitoringReconciler) GetResourcesDynamically(ctx context.Context, group string, version string, resource string, namespace string) ([]unstructured.Unstructured, error) {
	resourceId := schema.GroupVersionResource{
		Group:    group,
		Version:  version,
		Resource: resource,
	}
	list, err := r.DynamicClient.Resource(resourceId).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// Timing function to measure performance, starts the timer
func track(msg string) (string, time.Time) {
	return msg, time.Now()
}

var max time.Duration = time.Duration(0) * time.Nanosecond
var counter int = 1

// Timing function to measure performance, calculates the delay since the timer started
func duration(msg string, start time.Time) {
	elapsed := time.Since(start)
	if counter > 1 {
		if elapsed > max {
			max = elapsed
		}
	}
	if counter%50 == 0 {
		log.Log.V(0).Info("Time", msg, elapsed, "Max", max)
		counter = 1
	}
	counter++
}

// This thread uses the variable "Timers" to keep track of the nodes tainted with "RTDeadlinePressure"
// After the "polling_rate", if the timer for the node is zero and the taint is present, the taint is removed
func (r *MonitoringReconciler) StartTaintThread() {
	go func() {
		logger := log.Log.WithValues("Moniroting/rt.TaintMonitoringThread", "Taint")
		logger.V(1).Info("Starting taint monitoring thread")
		for {
			// Sleeps for "polling_rate" seconds
			time.Sleep(time.Duration(polling_rate) * time.Second)
			logger.V(1).Info("Taint Thread: Waking up, working...", "len(Timers)", len(Timers))
			// Checks all timers
			for nodeName, timer := range Timers {
				// For each timer that has expired
				if timer <= 0 {
					node := &corev1.Node{}
					// Obtaines the node for the timer
					// Note: we cannot store the node in the data structure because it may change inside Kubernetes and we need the latest version
					err := r.Get(context.TODO(), types.NamespacedName{Name: nodeName}, node)
					if err != nil {
						if errors.IsNotFound(err) {
							logger.Error(err, "Taint Thread: node not found, ignoring...")
							continue
						}
						logger.Error(err, "Taint Thread: failed to get node instance")
						continue
					}
					// We check all the taints, if "RTDeadlinePressure" is present, we remove it and update the node
					for i, taint := range node.Spec.Taints {
						if taint.Key == "RTDeadlinePressure" {
							// To remove the taint from the array:
							// assign last element to RTDeadlinePressure position
							node.Spec.Taints[i] = node.Spec.Taints[len(node.Spec.Taints)-1]
							// Update array without last element
							node.Spec.Taints = node.Spec.Taints[:len(node.Spec.Taints)-1]
						}
						logger.V(0).Info("Taint Thread: untaining node", "node", nodeName)
						err = r.Update(context.TODO(), node)
						if err != nil {
							logger.Error(err, "Taint Thread: error while un-tainting the node")
						}
					}
					// We remove the entry about the tainted node because we removed the taint
					delete(Timers, nodeName)
				} else {
					// If the timer is not zero, we decrement it
					logger.V(0).Info("Decrementing timer", nodeName, Timers[nodeName])
					Timers[nodeName]--
				}
			}
		}
	}()
}

// SetupWithManager sets up the controller with the Manager.
func (r *MonitoringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rtv1alpha1.Monitoring{}).
		Complete(r)
}
