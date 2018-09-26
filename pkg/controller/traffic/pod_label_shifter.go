package traffic

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corev1informer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"

	shipperv1 "github.com/bookingcom/shipper/pkg/apis/shipper/v1"
	"github.com/bookingcom/shipper/pkg/util/replicas"
)

type podLabelShifter struct {
	appName               string
	namespace             string
	serviceSelector       string
	clusterReleaseWeights clusterReleaseWeights
}

type clusterReleaseWeights map[string]map[string]uint32

func newPodLabelShifter(
	appName string,
	namespace string,
	trafficTargets []*shipperv1.TrafficTarget,
) (*podLabelShifter, error) {

	weights, err := buildClusterReleaseWeights(trafficTargets)
	if err != nil {
		return nil, err
	}

	serviceSelector := map[string]string{
		shipperv1.AppLabel: appName,
		shipperv1.LBLabel:  shipperv1.LBForProduction,
	}

	return &podLabelShifter{
		appName:               appName,
		namespace:             namespace,
		serviceSelector:       labels.Set(serviceSelector).AsSelector().String(),
		clusterReleaseWeights: weights,
	}, nil
}

func (p *podLabelShifter) Clusters() []string {
	clusters := make([]string, 0, len(p.clusterReleaseWeights))
	for cluster, _ := range p.clusterReleaseWeights {
		clusters = append(clusters, cluster)
	}
	sort.Strings(clusters)
	return clusters
}

func (p *podLabelShifter) SyncCluster(
	cluster string,
	clientset kubernetes.Interface,
	informer corev1informer.PodInformer,
) (map[string]uint32, []error, error) {
	releaseWeights, ok := p.clusterReleaseWeights[cluster]
	if !ok {
		return nil, nil, fmt.Errorf(
			"podLabelShifter has no weights for cluster %q", cluster)
	}

	podsClient := clientset.CoreV1().Pods(p.namespace)
	servicesClient := clientset.CoreV1().Services(p.namespace)

	svcList, err := servicesClient.List(metav1.ListOptions{LabelSelector: p.serviceSelector})
	if err != nil {
		return nil, nil, NewTargetClusterFetchServiceFailedError(cluster, p.serviceSelector, p.namespace, err)
	} else if n := len(svcList.Items); n != 1 {
		return nil, nil,
			NewTargetClusterWrongServiceCountError(cluster, p.serviceSelector, p.namespace, n)
	}

	prodSvc := svcList.Items[0]
	trafficSelector := prodSvc.Spec.Selector
	if trafficSelector == nil {
		return nil, nil,
			NewTargetClusterServiceMissesSelectorError(cluster, p.namespace, prodSvc.Name)
	}

	nsPodLister := informer.Lister().Pods(p.namespace)

	appSelector := labels.Set{shipperv1.AppLabel: p.appName}.AsSelector()
	pods, err := nsPodLister.List(appSelector)
	if err != nil {
		return nil, nil,
			NewTargetClusterPodListingError(cluster, p.namespace, err)
	}

	totalPods := len(pods)
	var totalWeight uint32 = 0
	for _, weight := range releaseWeights {
		totalWeight += weight
	}

	achievedWeights := map[string]uint32{}
	errors := []error{}
	for release, weight := range releaseWeights {

		releaseSelector := labels.Set{shipperv1.ReleaseLabel: release}.AsSelector()
		releasePods, err := nsPodLister.List(releaseSelector)
		if err != nil {
			return nil, nil,
				NewTargetClusterReleasePodListingError(
					release, cluster, p.namespace, err)
		}

		targetPods := calculateReleasePodTarget(len(releasePods), weight, totalPods, totalWeight)

		var trafficPods []*corev1.Pod
		var idlePods []*corev1.Pod
		for _, pod := range releasePods {
			if getsTraffic(pod, trafficSelector) {
				trafficPods = append(trafficPods, pod)
				continue
			}
			idlePods = append(idlePods, pod)
		}

		// everything is fine, nothing to do
		if len(trafficPods) == targetPods {
			achievedWeights[release] = weight
			continue
		}

		if len(trafficPods) > targetPods {
			excess := len(trafficPods) - targetPods
			removedFromLB := 0
			for i := 0; i < excess; i++ {
				pod := trafficPods[i].DeepCopy()

				if value, ok := pod.Labels[shipperv1.PodTrafficStatusLabel]; !ok || value == shipperv1.Enabled {
					patch := patchPodTrafficStatusLabel(pod, shipperv1.Disabled)
					_, err := podsClient.Patch(pod.Name, types.JSONPatchType, patch)
					if err != nil {
						errors = append(errors,
							NewTargetClusterTrafficModifyingLabelError(
								cluster, p.namespace, pod.Name, err))
						continue
					}
				}

				removedFromLB++
			}
			finalTrafficPods := len(trafficPods) - removedFromLB
			proportion := float64(finalTrafficPods) / float64(totalPods)
			achievedWeights[release] = uint32(round(proportion * float64(totalWeight)))
			continue
		}

		if len(trafficPods) < targetPods {
			missing := targetPods - len(trafficPods)
			addedToLB := 0
			if missing > len(idlePods) {
				errors = append(errors,
					NewTargetClusterMathError(release, len(idlePods), missing))
				continue
			}

			for i := 0; i < missing; i++ {
				pod := idlePods[i].DeepCopy()

				if value, ok := pod.Labels[shipperv1.PodTrafficStatusLabel]; !ok || ok && value == shipperv1.Disabled {
					patch := patchPodTrafficStatusLabel(pod, shipperv1.Enabled)
					_, err := podsClient.Patch(pod.Name, types.JSONPatchType, patch)
					if err != nil {
						errors = append(errors,
							NewTargetClusterTrafficModifyingLabelError(
								cluster, p.namespace, pod.Name, err))
						continue
					}
				}

				addedToLB++
			}
			finalTrafficPods := len(trafficPods) + addedToLB
			proportion := float64(finalTrafficPods) / float64(totalPods)
			achievedWeights[release] = uint32(round(proportion * float64(totalWeight)))
		}
	}

	return achievedWeights, errors, nil
}

func getsTraffic(pod *corev1.Pod, trafficSelectors map[string]string) bool {
	for key, trafficValue := range trafficSelectors {
		podValue, ok := pod.Labels[key]
		if !ok || podValue != trafficValue {
			return false
		}
	}
	return true
}

// PatchOperation represents a JSON PatchOperation in a very specific way.
// Using jsonpatch's types could be a possiblity, but there's no need to be
// generic in here.
type PatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

// patchPodTrafficStatusLabel returns a JSON Patch that modifies the
// PodTrafficStatusLabel value of a given Pod.
func patchPodTrafficStatusLabel(pod *corev1.Pod, value string) []byte {
	var op string

	if _, ok := pod.Labels[shipperv1.PodTrafficStatusLabel]; ok {
		op = "replace"
	} else {
		op = "add"
	}

	patchList := []PatchOperation{
		{
			Op:    op,
			Path:  fmt.Sprintf("/metadata/labels/%s", shipperv1.PodTrafficStatusLabel),
			Value: value,
		},
	}

	// Don't know what to do in here. From my perspective it is quite
	// unlikely that the json.Marshal operation above would fail since its
	// input should be a valid serializable value.
	patchBytes, _ := json.Marshal(patchList)

	return patchBytes
}

func calculateReleasePodTarget(releasePods int, releaseWeight uint32, totalPods int, totalWeight uint32) int {
	// What percentage of the entire fleet (across all releases) should this set of
	// pods represent.
	var targetPercent float64
	if totalWeight == 0 {
		targetPercent = 0
	} else {
		targetPercent = float64(releaseWeight) / float64(totalWeight) * 100
	}
	// Round up to the nearest pod, clamped to the number of pods this release has.
	targetPods := int(replicas.CalculateDesiredReplicaCount(uint(totalPods), float64(targetPercent)))

	targetPods = int(
		math.Min(
			float64(releasePods),
			float64(targetPods),
		),
	)
	return targetPods
}

/*
	Transform this (a list of each release's traffic target object in this namespace):
	[
		{ tt-reviewsapi-1: { cluster-1: 90 } },
		{ tt-reviewsapi-2: { cluster-1: 5 } },
		{ tt-reviewsapi-3: { cluster-1: 5 } },
	]

	Into this (a map of release weight per cluster):
	{
		cluster-1: {
			reviewsapi-1: 90,
			reviewsapi-2: 5,
			reviewsapi-3: 5,
		}
	}
*/
func buildClusterReleaseWeights(trafficTargets []*shipperv1.TrafficTarget) (clusterReleaseWeights, error) {
	clusterReleases := map[string]map[string]uint32{}
	releaseTT := map[string]*shipperv1.TrafficTarget{}
	for _, tt := range trafficTargets {
		release, ok := tt.Labels[shipperv1.ReleaseLabel]
		if !ok {
			return nil, fmt.Errorf(
				"TrafficTarget '%s/%s' needs a 'release' label in order to select resources in the target clusters.",
				tt.Namespace, tt.Name,
			)
		}
		existingTT, ok := releaseTT[release]
		if ok {
			return nil, fmt.Errorf(
				"TrafficTargets %q and %q in namespace %q both operate on release %q. This is wrong, please fix",
				existingTT.Name, tt.Name, tt.Namespace, release,
			)
		}
		releaseTT[release] = tt

		for _, cluster := range tt.Spec.Clusters {
			weights, ok := clusterReleases[cluster.Name]
			if !ok {
				weights = map[string]uint32{}
				clusterReleases[cluster.Name] = weights
			}
			weights[release] += cluster.Weight
		}
	}
	return clusterReleaseWeights(clusterReleases), nil
}

func round(num float64) int {
	if num < 0 {
		return int(num - 0.5)
	}
	return int(num + 0.5)
}
