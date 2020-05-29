/*
Copyright 2015 The Kubernetes Authors.

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

package pleg

import (
	"fmt"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
)

// GenericPLEG is an extremely simple generic PLEG that relies solely on
// periodic listing to discover container changes. It should be used
// as temporary replacement for container runtimes do not support a proper
// event generator yet.
//
// Note that GenericPLEG assumes that a container would not be created,
// terminated, and garbage collected within one relist period. If such an
// incident happens, GenenricPLEG would miss all events regarding this
// container. In the case of relisting failure, the window may become longer.
// Note that this assumption is not unique -- many kubelet internal components
// rely on terminated containers as tombstones for bookkeeping purposes. The
// garbage collector is implemented to work with such situations. However, to
// guarantee that kubelet can handle missing container events, it is
// recommended to set the relist period short and have an auxiliary, longer
// periodic sync in kubelet as the safety net.
/**
GenericPLEG是一个非常简单通用的PLEG（PodLifecycleEventGenerator），它只依赖于周期性的列表来
发现容器的变化。它应该被用作容器运行时的临时替换，但还不支持正确的事件生成器。
//
注意，GenericPLEG假设容器不会在一个重新列表（relist）周期内被创建、终止和垃圾收集。如果发生此类事件，
GenenricPLEG将错过有关此容器的所有事件。如果 relist 失败，窗口可能会变长。注意，这种假设并非
唯一的——许多kubelet内部组件依赖于作为墓碑的终止容器来记账。垃圾收集器的实现是为了处理这种情况。
但是，为了确保kubelet能够处理丢失的容器事件，建议将 relist 周期设置为较短，并在kubelet中有一个
辅助的较长周期同步作为安全网。
 */
// PodLifecycleEventGenerator 周期通过cri接口遍历所有Pod和container，并上报状态的event
type GenericPLEG struct {
	// The period for relisting.
	relistPeriod time.Duration
	// The container runtime.
	runtime kubecontainer.Runtime
	// The channel from which the subscriber listens events.
	eventChannel chan *PodLifecycleEvent
	// The internal cache for pod/container information.
	podRecords podRecords
	// Time of the last relisting.
	relistTime atomic.Value
	// Cache for storing the runtime states required for syncing pods.
	cache kubecontainer.Cache
	// For testability.
	clock clock.Clock
	// Pods that failed to have their status retrieved during a relist. These pods will be
	// retried during the next relisting.
	podsToReinspect map[types.UID]*kubecontainer.Pod
}

// plegContainerState has a one-to-one mapping to the
// kubecontainer.ContainerState except for the non-existent state. This state
// is introduced here to complete the state transition scenarios.
type plegContainerState string

const (
	plegContainerRunning     plegContainerState = "running"
	plegContainerExited      plegContainerState = "exited"
	plegContainerUnknown     plegContainerState = "unknown"
	plegContainerNonExistent plegContainerState = "non-existent"

	// The threshold needs to be greater than the relisting period + the
	// relisting time, which can vary significantly. Set a conservative
	// threshold to avoid flipping between healthy and unhealthy.
	relistThreshold = 3 * time.Minute
)

func convertState(state kubecontainer.ContainerState) plegContainerState {
	switch state {
	case kubecontainer.ContainerStateCreated:
		// kubelet doesn't use the "created" state yet, hence convert it to "unknown".
		return plegContainerUnknown
	case kubecontainer.ContainerStateRunning:
		return plegContainerRunning
	case kubecontainer.ContainerStateExited:
		return plegContainerExited
	case kubecontainer.ContainerStateUnknown:
		return plegContainerUnknown
	default:
		panic(fmt.Sprintf("unrecognized container state: %v", state))
	}
}

type podRecord struct {
	old     *kubecontainer.Pod
	current *kubecontainer.Pod
}

type podRecords map[types.UID]*podRecord

// NewGenericPLEG instantiates a new GenericPLEG object and return it.
func NewGenericPLEG(runtime kubecontainer.Runtime, channelCapacity int,
	relistPeriod time.Duration, cache kubecontainer.Cache, clock clock.Clock) PodLifecycleEventGenerator {
	return &GenericPLEG{
		relistPeriod: relistPeriod,
		runtime:      runtime,
		eventChannel: make(chan *PodLifecycleEvent, channelCapacity),
		podRecords:   make(podRecords),
		cache:        cache,
		clock:        clock,
	}
}

// Watch returns a channel from which the subscriber can receive PodLifecycleEvent
// events.
// TODO: support multiple subscribers.
func (g *GenericPLEG) Watch() chan *PodLifecycleEvent {
	return g.eventChannel
}

// Start spawns a goroutine to relist periodically.
// PodLifecycleEventGenerator 周期通过cri接口遍历所有Pod和container，并上报状态的event
func (g *GenericPLEG) Start() {
	/**
	GenericPLEG假设容器不会在一个重新列表（relist）周期内被创建、终止和垃圾收集。如果发生此类事件，
	GenenricPLEG将错过有关此容器的所有事件。为了确保kubelet能够处理丢失的容器事件，建议将 relist 周期设置为较短。
	 */
	// 默认为每一秒钟执行一次 relist。
	go wait.Until(g.relist, g.relistPeriod, wait.NeverStop)
}

// Healthy check if PLEG work properly.
// relistThreshold is the maximum interval between two relist.
func (g *GenericPLEG) Healthy() (bool, error) {
	relistTime := g.getRelistTime()
	if relistTime.IsZero() {
		return false, fmt.Errorf("pleg has yet to be successful")
	}
	// Expose as metric so you can alert on `time()-pleg_last_seen_seconds > nn`
	metrics.PLEGLastSeen.Set(float64(relistTime.Unix()))
	elapsed := g.clock.Since(relistTime)
	if elapsed > relistThreshold {
		return false, fmt.Errorf("pleg was last seen active %v ago; threshold is %v", elapsed, relistThreshold)
	}
	return true, nil
}

func generateEvents(podID types.UID, cid string, oldState, newState plegContainerState) []*PodLifecycleEvent {
	if newState == oldState {
		return nil
	}

	klog.V(4).Infof("GenericPLEG: %v/%v: %v -> %v", podID, cid, oldState, newState)
	switch newState {
	case plegContainerRunning:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerStarted, Data: cid}}
	case plegContainerExited:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerDied, Data: cid}}
	case plegContainerUnknown:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerChanged, Data: cid}}
	case plegContainerNonExistent:
		switch oldState {
		case plegContainerExited:
			// We already reported that the container died before.
			return []*PodLifecycleEvent{{ID: podID, Type: ContainerRemoved, Data: cid}}
		default:
			return []*PodLifecycleEvent{{ID: podID, Type: ContainerDied, Data: cid}, {ID: podID, Type: ContainerRemoved, Data: cid}}
		}
	default:
		panic(fmt.Sprintf("unrecognized container state: %v", newState))
	}
}

func (g *GenericPLEG) getRelistTime() time.Time {
	val := g.relistTime.Load()
	if val == nil {
		return time.Time{}
	}
	return val.(time.Time)
}

func (g *GenericPLEG) updateRelistTime(timestamp time.Time) {
	g.relistTime.Store(timestamp)
}

// relist queries the container runtime for list of pods/containers, compare
// with the internal pods/containers, and generates events accordingly.
func (g *GenericPLEG) relist() {
	klog.V(5).Infof("GenericPLEG: Relisting")

	if lastRelistTime := g.getRelistTime(); !lastRelistTime.IsZero() {
		metrics.PLEGRelistInterval.Observe(metrics.SinceInSeconds(lastRelistTime))
	}

	timestamp := g.clock.Now()
	defer func() {
		metrics.PLEGRelistDuration.Observe(metrics.SinceInSeconds(timestamp))
	}()

	// Get all the pods.
	podList, err := g.runtime.GetPods(true)
	if err != nil {
		klog.Errorf("GenericPLEG: Unable to retrieve pods: %v", err)
		return
	}

	g.updateRelistTime(timestamp)

	pods := kubecontainer.Pods(podList)
	// update running pod and container count
	updateRunningPodAndContainerMetrics(pods)
	g.podRecords.setCurrent(pods)

	// Compare the old and the current pods, and generate events.
	eventsByPodID := map[types.UID][]*PodLifecycleEvent{}
	for pid := range g.podRecords {
		oldPod := g.podRecords.getOld(pid)
		pod := g.podRecords.getCurrent(pid)
		// Get all containers in the old and the new pod.
		allContainers := getContainersFromPods(oldPod, pod)
		for _, container := range allContainers {
			events := computeEvents(oldPod, pod, &container.ID)
			for _, e := range events {
				updateEvents(eventsByPodID, e)
			}
		}
	}

	var needsReinspection map[types.UID]*kubecontainer.Pod
	if g.cacheEnabled() {
		needsReinspection = make(map[types.UID]*kubecontainer.Pod)
	}

	// If there are events associated with a pod, we should update the
	// podCache.
	for pid, events := range eventsByPodID {
		pod := g.podRecords.getCurrent(pid)
		if g.cacheEnabled() {
			// updateCache() will inspect the pod and update the cache. If an
			// error occurs during the inspection, we want PLEG to retry again
			// in the next relist. To achieve this, we do not update the
			// associated podRecord of the pod, so that the change will be
			// detect again in the next relist.
			// TODO: If many pods changed during the same relist period,
			// inspecting the pod and getting the PodStatus to update the cache
			// serially may take a while. We should be aware of this and
			// parallelize if needed.
			if err := g.updateCache(pod, pid); err != nil {
				// Rely on updateCache calling GetPodStatus to log the actual error.
				klog.V(4).Infof("PLEG: Ignoring events for pod %s/%s: %v", pod.Name, pod.Namespace, err)

				// make sure we try to reinspect the pod during the next relisting
				needsReinspection[pid] = pod

				continue
			} else {
				// this pod was in the list to reinspect and we did so because it had events, so remove it
				// from the list (we don't want the reinspection code below to inspect it a second time in
				// this relist execution)
				delete(g.podsToReinspect, pid)
			}
		}
		// Update the internal storage and send out the events.
		g.podRecords.update(pid)
		for i := range events {
			// Filter out events that are not reliable and no other components use yet.
			if events[i].Type == ContainerChanged {
				continue
			}
			select {
			case g.eventChannel <- events[i]:
			default:
				metrics.PLEGDiscardEvents.Inc()
				klog.Error("event channel is full, discard this relist() cycle event")
			}
		}
	}

	if g.cacheEnabled() {
		// reinspect any pods that failed inspection during the previous relist
		if len(g.podsToReinspect) > 0 {
			klog.V(5).Infof("GenericPLEG: Reinspecting pods that previously failed inspection")
			for pid, pod := range g.podsToReinspect {
				if err := g.updateCache(pod, pid); err != nil {
					// Rely on updateCache calling GetPodStatus to log the actual error.
					klog.V(5).Infof("PLEG: pod %s/%s failed reinspection: %v", pod.Name, pod.Namespace, err)
					needsReinspection[pid] = pod
				}
			}
		}

		// Update the cache timestamp.  This needs to happen *after*
		// all pods have been properly updated in the cache.
		g.cache.UpdateTime(timestamp)
	}

	// make sure we retain the list of pods that need reinspecting the next time relist is called
	g.podsToReinspect = needsReinspection
}

func getContainersFromPods(pods ...*kubecontainer.Pod) []*kubecontainer.Container {
	cidSet := sets.NewString()
	var containers []*kubecontainer.Container
	for _, p := range pods {
		if p == nil {
			continue
		}
		for _, c := range p.Containers {
			cid := string(c.ID.ID)
			if cidSet.Has(cid) {
				continue
			}
			cidSet.Insert(cid)
			containers = append(containers, c)
		}
		// Update sandboxes as containers
		// TODO: keep track of sandboxes explicitly.
		for _, c := range p.Sandboxes {
			cid := string(c.ID.ID)
			if cidSet.Has(cid) {
				continue
			}
			cidSet.Insert(cid)
			containers = append(containers, c)
		}

	}
	return containers
}

func computeEvents(oldPod, newPod *kubecontainer.Pod, cid *kubecontainer.ContainerID) []*PodLifecycleEvent {
	var pid types.UID
	if oldPod != nil {
		pid = oldPod.ID
	} else if newPod != nil {
		pid = newPod.ID
	}
	oldState := getContainerState(oldPod, cid)
	newState := getContainerState(newPod, cid)
	return generateEvents(pid, cid.ID, oldState, newState)
}

func (g *GenericPLEG) cacheEnabled() bool {
	return g.cache != nil
}

// getPodIP preserves an older cached status' pod IP if the new status has no pod IPs
// and its sandboxes have exited
func (g *GenericPLEG) getPodIPs(pid types.UID, status *kubecontainer.PodStatus) []string {
	if len(status.IPs) != 0 {
		return status.IPs
	}

	oldStatus, err := g.cache.Get(pid)
	if err != nil || len(oldStatus.IPs) == 0 {
		return nil
	}

	for _, sandboxStatus := range status.SandboxStatuses {
		// If at least one sandbox is ready, then use this status update's pod IP
		if sandboxStatus.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			return status.IPs
		}
	}

	// For pods with no ready containers or sandboxes (like exited pods)
	// use the old status' pod IP
	return oldStatus.IPs
}

func (g *GenericPLEG) updateCache(pod *kubecontainer.Pod, pid types.UID) error {
	if pod == nil {
		// The pod is missing in the current relist. This means that
		// the pod has no visible (active or inactive) containers.
		klog.V(4).Infof("PLEG: Delete status for pod %q", string(pid))
		g.cache.Delete(pid)
		return nil
	}
	timestamp := g.clock.Now()
	// TODO: Consider adding a new runtime method
	// GetPodStatus(pod *kubecontainer.Pod) so that Docker can avoid listing
	// all containers again.
	status, err := g.runtime.GetPodStatus(pod.ID, pod.Name, pod.Namespace)
	klog.V(4).Infof("PLEG: Write status for %s/%s: %#v (err: %v)", pod.Name, pod.Namespace, status, err)
	if err == nil {
		// Preserve the pod IP across cache updates if the new IP is empty.
		// When a pod is torn down, kubelet may race with PLEG and retrieve
		// a pod status after network teardown, but the kubernetes API expects
		// the completed pod's IP to be available after the pod is dead.
		status.IPs = g.getPodIPs(pid, status)
	}

	g.cache.Set(pod.ID, status, err, timestamp)
	return err
}

func updateEvents(eventsByPodID map[types.UID][]*PodLifecycleEvent, e *PodLifecycleEvent) {
	if e == nil {
		return
	}
	eventsByPodID[e.ID] = append(eventsByPodID[e.ID], e)
}

func getContainerState(pod *kubecontainer.Pod, cid *kubecontainer.ContainerID) plegContainerState {
	// Default to the non-existent state.
	state := plegContainerNonExistent
	if pod == nil {
		return state
	}
	c := pod.FindContainerByID(*cid)
	if c != nil {
		return convertState(c.State)
	}
	// Search through sandboxes too.
	c = pod.FindSandboxByID(*cid)
	if c != nil {
		return convertState(c.State)
	}

	return state
}

func updateRunningPodAndContainerMetrics(pods []*kubecontainer.Pod) {
	// Set the number of running pods in the parameter
	metrics.RunningPodCount.Set(float64(len(pods)))
	// intermediate map to store the count of each "container_state"
	containerStateCount := make(map[string]int)

	for _, pod := range pods {
		containers := pod.Containers
		for _, container := range containers {
			// update the corresponding "container_state" in map to set value for the gaugeVec metrics
			containerStateCount[string(container.State)]++
		}
	}
	for key, value := range containerStateCount {
		metrics.RunningContainerCount.WithLabelValues(key).Set(float64(value))
	}
}

func (pr podRecords) getOld(id types.UID) *kubecontainer.Pod {
	r, ok := pr[id]
	if !ok {
		return nil
	}
	return r.old
}

func (pr podRecords) getCurrent(id types.UID) *kubecontainer.Pod {
	r, ok := pr[id]
	if !ok {
		return nil
	}
	return r.current
}

func (pr podRecords) setCurrent(pods []*kubecontainer.Pod) {
	for i := range pr {
		pr[i].current = nil
	}
	for _, pod := range pods {
		if r, ok := pr[pod.ID]; ok {
			r.current = pod
		} else {
			pr[pod.ID] = &podRecord{current: pod}
		}
	}
}

func (pr podRecords) update(id types.UID) {
	r, ok := pr[id]
	if !ok {
		return
	}
	pr.updateInternal(id, r)
}

func (pr podRecords) updateInternal(id types.UID, r *podRecord) {
	if r.current == nil {
		// Pod no longer exists; delete the entry.
		delete(pr, id)
		return
	}
	r.old = r.current
	r.current = nil
}
